package gobgp

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
)

func config() Config {
	return Config{Write: netip.MustParsePrefix("192.0.2.100/32"), Read: netip.MustParsePrefix("192.0.2.102/32"), NextHop: netip.MustParseAddr("192.0.2.1"), RPCTimeout: time.Second, MaxAge: time.Minute}
}

func healthy() health.Snapshot {
	c := health.Check{State: health.Healthy, StartedAt: time.Now()}
	return health.Snapshot{Primary: c, Pooler: c}
}

func manager(t *testing.T, c api.GoBgpServiceClient, dir string, cfg Config) *Manager {
	t.Helper()
	m, err := newManager(t.Context(), cfg, dir, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func TestNativeManagerWithdrawsOnlyOwnedAndRestarts(t *testing.T) {
	c := nativeClient(t)
	nativeAdd(t, c, "192.0.2.101/32")
	dir := privateDir(t)
	m := manager(t, c, dir, config())
	if err := m.Reconcile(t.Context(), healthy()); err != nil {
		t.Fatal(err)
	}
	if len(nativeList(t, c)) != 2 {
		t.Fatal("own route missing")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := manager(t, c, dir, config())
	paths := nativeList(t, c)
	if len(paths) != 1 || paths[0].Prefix != "192.0.2.101/32" {
		t.Fatalf("restart changed foreign path or retained owned path: %+v", paths)
	}
	if err := restarted.Reconcile(t.Context(), healthy()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(t.Context(), health.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	paths = nativeList(t, c)
	if len(paths) != 1 || paths[0].Prefix != "192.0.2.101/32" {
		t.Fatalf("withdrawal changed foreign path: %+v", paths)
	}
}

func TestNativeManagerRejectsOccupiedTarget(t *testing.T) {
	c := nativeClient(t)
	nativeAdd(t, c, "192.0.2.100/32")
	m := manager(t, c, privateDir(t), config())
	if err := m.Reconcile(t.Context(), healthy()); !errors.Is(err, ErrOccupied) {
		t.Fatalf("occupied prefix: %v", err)
	}
	if paths := nativeList(t, c); len(paths) != 1 {
		t.Fatalf("foreign target replaced: %+v", paths)
	}
	if err := m.Reconcile(t.Context(), health.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(nativeList(t, c)) != 1 {
		t.Fatal("foreign target deleted")
	}
}

type lostReply struct {
	api.GoBgpServiceClient
	adds int
}

func (c *lostReply) AddPath(ctx context.Context, r *api.AddPathRequest, o ...grpc.CallOption) (*api.AddPathResponse, error) {
	c.adds++
	if _, err := c.GoBgpServiceClient.AddPath(ctx, r, o...); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}

func TestNativeLostAddReplyBlocksRetryAndRestart(t *testing.T) {
	c := nativeClient(t)
	nativeAdd(t, c, "192.0.2.101/32")
	fault := &lostReply{GoBgpServiceClient: c}
	dir := privateDir(t)
	m := manager(t, fault, dir, config())
	if err := m.Reconcile(t.Context(), healthy()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("lost reply: %v", err)
	}
	if err := m.Reconcile(t.Context(), healthy()); !errors.Is(err, ErrRecovery) {
		t.Fatalf("retry allowed: %v", err)
	}
	if fault.adds != 1 || len(nativeList(t, c)) != 2 {
		t.Fatal("ambiguous add retried or native mutation missing")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := newManager(t.Context(), config(), dir, c)
	if next != nil || !errors.Is(err, ErrRecovery) {
		t.Fatalf("pending startup allowed: %v", err)
	}
	if len(nativeList(t, c)) != 2 {
		t.Fatal("startup changed unknown paths")
	}
}

func TestNativeSpeakerRestartRejectsStaleUUID(t *testing.T) {
	c, _, stop := nativeEndpoint(t)
	dir := privateDir(t)
	m := manager(t, c, dir, config())
	if err := m.Reconcile(t.Context(), healthy()); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	stop()
	replacement := nativeClient(t)
	nativeAdd(t, replacement, "192.0.2.100/32")
	next, err := newManager(t.Context(), config(), dir, replacement)
	if next != nil || !errors.Is(err, ErrRecovery) {
		t.Fatalf("stale UUID accepted: %v", err)
	}
	if len(nativeList(t, replacement)) != 1 {
		t.Fatal("foreign replacement deleted")
	}
}

func TestManagerRejectsStaleHealthAndSerializesOwners(t *testing.T) {
	c := nativeClient(t)
	dir := privateDir(t)
	m := manager(t, c, dir, config())
	other, err := newManager(t.Context(), config(), dir, c)
	if other != nil || err == nil {
		t.Fatal("concurrent owner accepted")
	}
	s := healthy()
	s.Primary.StartedAt = time.Now().Add(-2 * time.Minute)
	if err := m.Reconcile(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if len(nativeList(t, c)) != 0 {
		t.Fatal("stale health advertised")
	}
}

func TestManagerRejectsJournalTampering(t *testing.T) {
	for _, kind := range []string{"permissions", "symlink", "truncated", "config", "staging"} {
		t.Run(kind, func(t *testing.T) {
			c := nativeClient(t)
			dir := privateDir(t)
			m := manager(t, c, dir, config())
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, "journal.json")
			switch kind {
			case "permissions":
				if err := os.Chmod(name, 0o644); err != nil { // #nosec G302 -- Negative fixture verifies rejection of a public journal.
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(name, filepath.Join(dir, "original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("original", name); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.WriteFile(name, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "config":
				cfg := config()
				cfg.NextHop = netip.MustParseAddr("192.0.2.2")
				next, err := newManager(t.Context(), cfg, dir, c)
				if next != nil || err == nil {
					t.Fatal("changed identity accepted")
				}
				return
			case "staging":
				if err := os.WriteFile(filepath.Join(dir, "journal.next"), []byte("partial"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			next, err := newManager(t.Context(), config(), dir, c)
			if next != nil || err == nil {
				t.Fatalf("unsafe journal accepted: %v", err)
			}
		})
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- Private directory needs owner traversal, with no group/other access.
		t.Fatal(err)
	}
	return dir
}
