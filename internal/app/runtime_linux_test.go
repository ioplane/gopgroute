//go:build linux

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestDaemonHelper(t *testing.T) {
	if os.Getenv("GOPGROUTE_DAEMON_TEST") != "1" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	cfg, err := readSnapshot(os.Stdin)
	if err == nil {
		switch os.Args[len(os.Args)-1] {
		case "speaker":
			err = Speaker(ctx, cfg)
		case "adapter":
			progress := os.NewFile(3, "progress")
			err = Adapter(ctx, cfg, progress)
			if closeErr := progress.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		default:
			err = errors.New("invalid helper mode")
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := Decode(strings.NewReader(example))
	if err != nil {
		t.Fatal(err)
	}
	cfg.SpeakerBinary, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(cfg.SpeakerBinary)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SpeakerSHA256 = fmt.Sprintf("%x", sha256.Sum256(binary))
	cfg.StartupTimeout = Duration(2 * time.Second)
	cfg.HeartbeatTimeout = Duration(5 * time.Second)
	cfg.Peers[0].Address = netip.MustParseAddr("127.0.0.2")
	root, err := os.MkdirTemp("", "gopgroute-app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	cfg.RuntimeDir, cfg.JournalDir, cfg.PasswordFile = filepath.Join(root, "run"), filepath.Join(root, "journal"), filepath.Join(root, "password")
	for _, dir := range []string{cfg.RuntimeDir, cfg.JournalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.PasswordFile, []byte("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGuardianRejectsSpeakerDigestMismatch(t *testing.T) {
	cfg := testConfig(t)
	cfg.SpeakerSHA256 = strings.Repeat("0", 64)
	speaker, adapter := childCommand("speaker"), childCommand("adapter")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := Guardian(ctx, cfg, speaker, adapter)
	if err == nil || !strings.Contains(err.Error(), "speaker digest mismatch") || speaker.Process != nil || adapter.Process != nil {
		t.Fatalf("unverified executable admitted: %v", err)
	}
}

func childCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHelper$", "--", mode) // #nosec G204 G702 -- Only this test executable and its fixed child role.
	cmd.Env = append(os.Environ(), "GOPGROUTE_DAEMON_TEST=1", "GORACE=atexit_sleep_ms=0")
	cmd.Stderr = os.Stderr
	return cmd
}

func TestGuardianRejectsOccupiedRuntimeBeforeStartingChildren(t *testing.T) {
	cfg := testConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.RuntimeDir, "speaker.sock"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	speaker, adapter := childCommand("speaker"), childCommand("adapter")
	err := Guardian(t.Context(), cfg, speaker, adapter)
	if err == nil || speaker.Process != nil || adapter.Process != nil {
		t.Fatalf("occupied runtime accepted: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.RuntimeDir, "speaker.sock"))
	if err != nil || string(data) != "foreign" {
		t.Fatal("foreign runtime entry changed")
	}
}

func TestRunnableDaemonDeniesRoutesWithoutSQL(t *testing.T) {
	cfg := testConfig(t)
	var requests atomic.Int32
	patroni := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/primary" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if _, err := w.Write([]byte(`{"state":"running","role":"primary"}`)); err != nil {
			t.Error(err)
		}
	}))
	defer patroni.Close()
	cfg.PatroniURL = patroni.URL
	speaker, adapter := childCommand("speaker"), childCommand("adapter")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Guardian(ctx, cfg, speaker, adapter); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Log(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("guardian did not reap children")
		}
	})
	connection, err := grpc.NewClient("unix://"+filepath.Join(cfg.RuntimeDir, "speaker.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	client := api.NewGoBgpServiceClient(connection)
	call, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	state, err := client.GetBgp(call, &api.GetBgpRequest{}, grpc.WaitForReady(true))
	if err != nil || state.GetGlobal().GetAsn() != cfg.ASN {
		t.Fatalf("native speaker not ready: %v", err)
	}
	if err := waitUntil(call, func() (bool, error) { return speakerReady(call, client, cfg) }); err != nil {
		t.Fatal(err)
	}
	// A second pair of role probes proves the first SQL/route cycle completed.
	if err := waitUntil(call, func() (bool, error) { return requests.Load() >= 4, nil }); err != nil {
		t.Fatal(err)
	}
	stream, err := client.ListPath(call, &api.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}})
	if err != nil {
		t.Fatal(err)
	}
	if path, err := stream.Recv(); !errors.Is(err, io.EOF) || path != nil {
		t.Fatalf("route advertised without SQL: %v %v", path, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || speaker.ProcessState == nil || adapter.ProcessState == nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown exceeded bound")
	}
}
