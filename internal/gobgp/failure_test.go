package gobgp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
)

type addFault struct {
	api.GoBgpServiceClient
	after func(*api.AddPathResponse)
	adds  int
}

func (c *addFault) AddPath(ctx context.Context, r *api.AddPathRequest, o ...grpc.CallOption) (*api.AddPathResponse, error) {
	c.adds++
	response, err := c.GoBgpServiceClient.AddPath(ctx, r, o...)
	if err == nil {
		c.after(response)
	}
	return response, err
}

func TestNativeInvalidUUIDAndPostAddPersistenceFailure(t *testing.T) {
	for _, kind := range []string{"nil", "short", "zero", "persist"} {
		t.Run(kind, func(t *testing.T) {
			c := nativeClient(t)
			dir := privateDir(t)
			fault := &addFault{GoBgpServiceClient: c}
			fault.after = func(r *api.AddPathResponse) {
				switch kind {
				case "nil":
					r.Uuid = nil
				case "short":
					r.Uuid = []byte{1}
				case "zero":
					r.Uuid = make([]byte, 16)
				case "persist":
					if err := os.WriteFile(filepath.Join(dir, "journal.next"), []byte("injected failure"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			m := manager(t, fault, dir, config())
			if err := m.Reconcile(t.Context(), healthy()); !errors.Is(err, ErrUncertain) {
				t.Fatalf("unsafe result accepted: %v", err)
			}
			if err := m.Reconcile(t.Context(), health.Snapshot{}); !errors.Is(err, ErrRecovery) {
				t.Fatalf("unsafe recovery allowed: %v", err)
			}
			if fault.adds != 1 || len(nativeList(t, c)) != 1 {
				t.Fatal("mutation was retried or lost")
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := newManager(t.Context(), config(), dir, c)
			if restarted != nil || !errors.Is(err, ErrRecovery) {
				t.Fatalf("unsafe restart: %v", err)
			}
		})
	}
}

type deleteFault struct {
	api.GoBgpServiceClient
	deletes int
}

func (c *deleteFault) DeletePath(ctx context.Context, r *api.DeletePathRequest, o ...grpc.CallOption) (*api.DeletePathResponse, error) {
	c.deletes++
	if !validUUID(r.Uuid) || r.Path != nil || r.Family != nil {
		return nil, errors.New("unsafe deletion request")
	}
	if _, err := c.GoBgpServiceClient.DeletePath(ctx, r, o...); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}

func TestNativeLostDeleteReplyPreservesForeignAndBlocksRestart(t *testing.T) {
	c := nativeClient(t)
	nativeAdd(t, c, "192.0.2.101/32")
	fault := &deleteFault{GoBgpServiceClient: c}
	dir := privateDir(t)
	m := manager(t, fault, dir, config())
	if err := m.Reconcile(t.Context(), healthy()); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(t.Context(), health.Snapshot{}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("lost delete: %v", err)
	}
	if err := m.Reconcile(t.Context(), healthy()); !errors.Is(err, ErrRecovery) {
		t.Fatalf("retry allowed: %v", err)
	}
	paths := nativeList(t, c)
	if fault.deletes != 1 || len(paths) != 1 || paths[0].Prefix != "192.0.2.101/32" {
		t.Fatalf("foreign route changed: %+v", paths)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newManager(t.Context(), config(), dir, c)
	if restarted != nil || !errors.Is(err, ErrRecovery) {
		t.Fatalf("pending delete recovered: %v", err)
	}
}

type slowPreflight struct {
	api.GoBgpServiceClient
	until time.Time
	adds  int
	reads int
}

func (c *slowPreflight) ListPath(ctx context.Context, r *api.ListPathRequest, o ...grpc.CallOption) (grpc.ServerStreamingClient[api.ListPathResponse], error) {
	c.reads++
	stream, err := c.GoBgpServiceClient.ListPath(ctx, r, o...)
	timer := time.NewTimer(time.Until(c.until))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return stream, err
}

func (c *slowPreflight) AddPath(ctx context.Context, r *api.AddPathRequest, o ...grpc.CallOption) (*api.AddPathResponse, error) {
	c.adds++
	return c.GoBgpServiceClient.AddPath(ctx, r, o...)
}

func TestNativeHealthExpiresDuringPreflight(t *testing.T) {
	c := nativeClient(t)
	cfg := config()
	cfg.MaxAge = 100 * time.Millisecond
	fault := &slowPreflight{GoBgpServiceClient: c}
	m := manager(t, fault, privateDir(t), cfg)
	s := healthy()
	fault.until = s.Primary.StartedAt.Add(cfg.MaxAge + 10*time.Millisecond)
	if err := m.Reconcile(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if fault.reads != 1 || fault.adds != 0 || len(nativeList(t, c)) != 0 {
		t.Fatal("expired evidence reached AddPath")
	}
}

func TestNativeRoleChangeWithdrawsWriteBeforeRead(t *testing.T) {
	c := nativeClient(t)
	m := manager(t, c, privateDir(t), config())
	if err := m.Reconcile(t.Context(), healthy()); err != nil {
		t.Fatal(err)
	}
	s := healthy()
	s.Replica = s.Primary
	s.Primary = health.Check{}
	if err := m.Reconcile(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	paths := nativeList(t, c)
	if len(paths) != 1 || paths[0].Prefix != config().Read.String() {
		t.Fatalf("role transition left write route: %+v", paths)
	}
}

func TestOpenRequiresPrivateUnixTransport(t *testing.T) {
	c, socket, _ := nativeEndpoint(t)
	dir := privateDir(t)
	if err := os.Chmod(socket, 0o666); err != nil { // #nosec G302 -- Negative fixture verifies rejection of a public socket.
		t.Fatal(err)
	}
	if m, err := Open(t.Context(), config(), dir, socket); m != nil || err == nil {
		t.Fatal("public socket accepted")
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(t.Context(), config(), dir, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := m.Reconcile(t.Context(), healthy()); err != nil {
		t.Fatal(err)
	}
	if len(nativeList(t, c)) != 1 {
		t.Fatal("private transport did not advertise")
	}
	if err := m.Reconcile(t.Context(), health.Snapshot{}); err != nil {
		t.Fatal(err)
	}
}
