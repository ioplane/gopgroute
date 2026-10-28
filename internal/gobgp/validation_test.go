package gobgp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/osrg/gobgp/v4/api"
)

func TestInvalidRouteConfiguration(t *testing.T) {
	for _, kind := range []string{"same", "aggregate", "ipv6", "multicast", "unspecified", "hop", "timeout", "freshness"} {
		t.Run(kind, func(t *testing.T) {
			cfg := config()
			switch kind {
			case "same":
				cfg.Read = cfg.Write
			case "aggregate":
				cfg.Write = netip.MustParsePrefix("192.0.2.0/24")
			case "ipv6":
				cfg.Write = netip.MustParsePrefix("2001:db8::1/128")
			case "multicast":
				cfg.Read = netip.MustParsePrefix("224.0.0.1/32")
			case "unspecified":
				cfg.Read = netip.MustParsePrefix("0.0.0.0/32")
			case "hop":
				cfg.NextHop = netip.MustParseAddr("0.0.0.0")
			case "timeout":
				cfg.RPCTimeout = 0
			case "freshness":
				cfg.MaxAge = 0
			}
			if m, err := newManager(t.Context(), cfg, privateDir(t), nativeClient(t)); m != nil || err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestNoncanonicalAndInvalidJournalStates(t *testing.T) {
	for _, kind := range []string{"duplicate", "unknown", "UUID", "emptyUUID", "phase", "hardlink", "locklink"} {
		t.Run(kind, func(t *testing.T) {
			c := nativeClient(t)
			dir := privateDir(t)
			m := manager(t, c, dir, config())
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, "journal.json")
			data, err := os.ReadFile(name) // #nosec G304 -- Fixed journal basename inside a private test-owned temporary directory.
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "duplicate":
				data = bytes.Replace(data, []byte(`"Version":1`), []byte(`"Version":2,"Version":1`), 1)
			case "unknown":
				data = bytes.Replace(data, []byte(`"Version":1`), []byte(`"Extra":1,"Version":1`), 1)
			case "UUID", "emptyUUID", "phase":
				var j journal
				if err := json.Unmarshal(data, &j); err != nil {
					t.Fatal(err)
				}
				j.Entries[0] = entry{Phase: "owned", UUID: "01"}
				if kind == "emptyUUID" {
					j.Entries[0] = entry{Phase: "pending-add", UUID: "01"}
				}
				if kind == "phase" {
					j.Entries[0] = entry{Phase: "unrecognized"}
				}
				data, err = json.Marshal(j)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, '\n')
			case "hardlink":
				if err := os.Link(name, filepath.Join(dir, "linked")); err != nil {
					t.Fatal(err)
				}
			case "locklink":
				lock := filepath.Join(dir, "lock")
				if err := os.Rename(lock, filepath.Join(dir, "old-lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("old-lock", lock); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(name, data, 0o600); err != nil { // #nosec G703 -- Deliberately corrupt a fixed journal inside the private test fixture.
				t.Fatal(err)
			}
			if next, err := newManager(t.Context(), config(), dir, c); next != nil || err == nil {
				t.Fatal("unsafe journal accepted")
			}
		})
	}
}

func TestNativeSuccessfulAddExpiredBeforePersistenceReturnsIsWithdrawn(t *testing.T) {
	c := nativeClient(t)
	nativeAdd(t, c, "192.0.2.101/32")
	fault := &addFault{GoBgpServiceClient: c}
	cfg := config()
	cfg.MaxAge = 100 * time.Millisecond
	m := manager(t, fault, privateDir(t), cfg)
	s := healthy()
	fault.after = func(_response *api.AddPathResponse) {
		timer := time.NewTimer(time.Until(s.Primary.StartedAt.Add(cfg.MaxAge + 10*time.Millisecond)))
		defer timer.Stop()
		<-timer.C
	}
	if err := m.Reconcile(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	paths := nativeList(t, c)
	if fault.adds != 1 || len(paths) != 1 || paths[0].Prefix != "192.0.2.101/32" {
		t.Fatalf("expired successful add retained or foreign route changed: %+v", paths)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(t.Context(), health.Snapshot{}); !errors.Is(err, ErrRecovery) {
		t.Fatalf("closed manager used: %v", err)
	}
}
