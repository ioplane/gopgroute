//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestIncarnationsPreserveUncertainOldJournal(t *testing.T) {
	cfg := testConfig(t)
	first, err := newIncarnation(cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(first.JournalDir, "journal.next")
	if err := os.WriteFile(old, []byte("uncertain previous mutation"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := newIncarnation(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.JournalDir == second.JournalDir || first.socket() == second.socket() {
		t.Fatal("incarnation reused")
	}
	data, err := os.ReadFile(old) // #nosec G304 -- Exact journal fixture in the test-owned generated directory.
	if err != nil || string(data) != "uncertain previous mutation" {
		t.Fatalf("old journal changed: %v", err)
	}
	entries, err := os.ReadDir(second.JournalDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("new manager would reuse state: %v", err)
	}
}

func TestGuardianRejectsLegacyAndUnknownRoots(t *testing.T) {
	for _, name := range []string{"journal.json", "journal.next", "lock", "i-not-an-id", "unrelated"} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			path := filepath.Join(cfg.JournalDir, name)
			if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			speaker, adapter := childCommand("speaker"), childCommand("adapter")
			if err := Guardian(t.Context(), cfg, speaker, adapter); err == nil || speaker.Process != nil || adapter.Process != nil {
				t.Fatalf("unknown root admitted: %v", err)
			}
			data, err := os.ReadFile(path) // #nosec G304 -- Exact preserved legacy fixture in the test-owned directory.
			if err != nil || string(data) != "preserve" {
				t.Fatalf("legacy evidence changed: %v", err)
			}
		})
	}
}

func TestEitherLeaseExcludesChangedOtherRoot(t *testing.T) {
	for _, journal := range []bool{false, true} {
		cfg := testConfig(t)
		dir := cfg.RuntimeDir
		if journal {
			dir = cfg.JournalDir
		}
		lease, err := incarnationLease(dir)
		if err != nil {
			t.Fatal(err)
		}
		other := testConfig(t)
		if journal {
			cfg.RuntimeDir = other.RuntimeDir
		} else {
			cfg.JournalDir = other.JournalDir
		}
		speaker, adapter := childCommand("speaker"), childCommand("adapter")
		err = Guardian(t.Context(), cfg, speaker, adapter)
		if closeErr := lease.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err == nil || speaker.Process != nil || adapter.Process != nil {
			t.Fatalf("live incarnation bypassed: %v", err)
		}
	}
}

func TestGuardianSameRootsExcludesConcurrentStart(t *testing.T) {
	cfg := testConfig(t)
	lease, err := incarnationLease(cfg.RuntimeDir)
	if err != nil {
		t.Fatal(err)
	}
	speaker, adapter := childCommand("speaker"), childCommand("adapter")
	err = Guardian(t.Context(), cfg, speaker, adapter)
	if closeErr := lease.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil || speaker.Process != nil || adapter.Process != nil {
		t.Fatalf("concurrent same-root guardian admitted: %v", err)
	}
}

func TestIncarnationRejectsUnsafeLeaseAndDirectory(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "mode", "directory", "generation-symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t)
			path := filepath.Join(cfg.JournalDir, "guardian.lock")
			switch kind {
			case "symlink":
				if err := os.Symlink(cfg.PasswordFile, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(cfg.PasswordFile, path); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.WriteFile(path, nil, 0o644); err != nil { // #nosec G306 -- Deliberately invalid lease permissions must be rejected before child startup.
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "generation-symlink":
				if err := os.Symlink(cfg.RuntimeDir, filepath.Join(cfg.JournalDir, "i-"+strings.Repeat("a", 32))); err != nil {
					t.Fatal(err)
				}
			}
			speaker, adapter := childCommand("speaker"), childCommand("adapter")
			if err := Guardian(t.Context(), cfg, speaker, adapter); err == nil || speaker.Process != nil || adapter.Process != nil {
				t.Fatalf("unsafe root admitted: %v", err)
			}
		})
	}
}

func TestSpeakerExecRetainsLeasesAfterGuardianCopiesClose(t *testing.T) {
	cfg := testConfig(t)
	var leases []*os.File
	for _, dir := range []string{cfg.RuntimeDir, cfg.JournalDir} {
		lease, err := incarnationLease(dir)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	t.Cleanup(func() {
		for _, lease := range leases {
			if err := lease.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Error(err)
			}
		}
	})
	settings, err := snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := settings.Close(); err != nil {
			t.Error(err)
		}
	}()
	speaker := childCommand("speaker")
	speaker.Stdin, speaker.ExtraFiles = settings, leases
	if err := speaker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if speaker.ProcessState == nil {
			if err := speaker.Process.Kill(); err != nil {
				t.Error(err)
			}
			if err := speaker.Wait(); err == nil {
				t.Error("speaker survived kill")
			}
		}
	})
	connection, err := grpc.NewClient("unix://"+cfg.socket(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := waitUntil(ctx, func() (bool, error) { return speakerReady(ctx, api.NewGoBgpServiceClient(connection), cfg) }); err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases {
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{cfg.RuntimeDir, cfg.JournalDir} {
		lease, err := incarnationLease(dir)
		if err == nil {
			if err := lease.Close(); err != nil {
				t.Error(err)
			}
			t.Fatal("exec speaker released inherited lease")
		}
		if !strings.Contains(err.Error(), "lock incarnation") {
			t.Fatal(err)
		}
	}
	if err := speaker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := speaker.Wait(); err == nil {
		t.Fatal("speaker survived kill")
	}
	for _, dir := range []string{cfg.RuntimeDir, cfg.JournalDir} {
		lease, err := incarnationLease(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
