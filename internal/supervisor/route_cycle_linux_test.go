//go:build linux

package supervisor_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/gobgp"
	"github.com/ioplane/gopgroute/internal/health"
	"github.com/ioplane/gopgroute/internal/reconcile"
	"github.com/ioplane/gopgroute/internal/supervisor"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// This helper runs native GoBGP or the real route manager in separate processes.
func TestRouteCycleHelper(t *testing.T) {
	mode := os.Getenv("GOPGROUTE_ROUTE_HELPER")
	if mode == "" {
		return
	}
	if err := routeChild(mode, os.Args[len(os.Args)-1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(20)
	}
	os.Exit(0)
}

func routeChild(mode, root string) (result error) {
	socket := filepath.Join(root, "rpc.sock")
	if mode == "speaker" {
		speaker := server.NewBgpServer(server.GrpcListenAddress("unix://" + socket))
		go speaker.Serve()
		connection, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, connection.Close()) }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err = api.NewGoBgpServiceClient(connection).StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 64512, RouterId: "192.0.2.254", ListenPort: -1}}, grpc.WaitForReady(true))
		if err != nil {
			return err
		}
		if err := os.Chmod(socket, 0o600); err != nil { // #nosec G703 -- Private test socket under the parent-created temporary directory.
			return err
		}
		select {} // The supervisor owns this dedicated speaker's complete lifetime.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(socket) // #nosec G703 -- Private test socket under the parent-created temporary directory.
		if err == nil && info.Mode().Perm() == 0o600 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("speaker startup: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	routes, err := gobgp.Open(ctx, gobgp.Config{
		Write: netip.MustParsePrefix("192.0.2.100/32"), Read: netip.MustParsePrefix("192.0.2.101/32"),
		NextHop: netip.MustParseAddr("192.0.2.1"), RPCTimeout: time.Second, MaxAge: time.Second,
	}, filepath.Join(root, "journal"), socket)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, routes.Close()) }()
	progress := os.NewFile(3, "supervisor-progress")
	defer func() { result = errors.Join(result, progress.Close()) }()
	first := true
	return reconcile.Run(context.Background(), reconcile.Config{Interval: time.Millisecond, CycleTimeout: 10 * time.Second}, func(ctx context.Context) error {
		if !first {
			var trigger [1]byte
			if _, err := io.ReadFull(os.Stdin, trigger[:]); err != nil {
				return err
			}
			if mode == "stall" {
				<-ctx.Done()
				return ctx.Err()
			}
			return gobgp.ErrUncertain
		}
		first = false
		now := time.Now()
		if err := routes.Reconcile(ctx, health.Snapshot{
			Primary: health.Check{State: health.Healthy, StartedAt: now},
			Pooler:  health.Check{State: health.Healthy, StartedAt: now},
		}); err != nil {
			return err
		}
		_, err := os.Stdout.Write([]byte{'R'})
		return err
	}, progress)
}

func TestNativeRouteCycleFailureStopsDedicatedSpeaker(t *testing.T) {
	for _, mode := range []string{"stall", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			root, err := os.MkdirTemp("", "gopgroute-cycle-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(root); err != nil {
					t.Error(err)
				}
			})
			if err := os.Mkdir(filepath.Join(root, "journal"), 0o700); err != nil {
				t.Fatal(err)
			}
			input, command, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer closeTest(t, input)
			defer closeTest(t, command)
			ready, output, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer closeTest(t, ready)
			defer closeTest(t, output)
			if err := ready.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := func(mode string) *exec.Cmd {
				cmd := exec.Command(executable, "-test.run=^TestRouteCycleHelper$", "--", root) // #nosec G204 -- Relaunch this test executable with a private temporary directory.
				cmd.Env = append(os.Environ(), "GOPGROUTE_ROUTE_HELPER="+mode)
				cmd.Stderr = os.Stderr
				return cmd
			}
			speaker, adapter := child("speaker"), child(mode)
			adapter.Stdin, adapter.Stdout = input, output
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- supervisor.Run(ctx, supervisor.Config{Speaker: speaker, Adapter: adapter, HeartbeatTimeout: 3 * time.Second, StopTimeout: 200 * time.Millisecond})
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Logf("supervisor cleanup: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("supervisor did not join native children")
				}
			})
			var signal [1]byte
			if _, err := io.ReadFull(ready, signal[:]); err != nil || signal[0] != 'R' {
				t.Fatalf("route cycle did not complete: %q %v", signal, err)
			}
			connection, err := grpc.NewClient("unix://"+filepath.Join(root, "rpc.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
			if err != nil {
				t.Fatal(err)
			}
			defer closeTest(t, connection)
			client := api.NewGoBgpServiceClient(connection)
			probe, stop := context.WithTimeout(t.Context(), time.Second)
			stream, err := client.ListPath(probe, &api.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}})
			if err != nil {
				stop()
				t.Fatal(err)
			}
			path, err := stream.Recv()
			if err != nil || path.GetDestination().GetPrefix() != "192.0.2.100/32" {
				stop()
				t.Fatalf("native route absent before fault: %v %v", path, err)
			}
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				stop()
				t.Fatalf("unexpected native route: %v", err)
			}
			stop()
			if _, err := command.Write([]byte{'F'}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil || speaker.ProcessState == nil || adapter.ProcessState == nil {
					t.Fatalf("fault did not stop and reap children: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("native fault exceeded supervisor bound")
			}
			probe, stop = context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer stop()
			if _, err := client.GetBgp(probe, &api.GetBgpRequest{}); err == nil {
				t.Fatal("dedicated speaker still serves RPC after adapter failure")
			}
		})
	}
}

func closeTest(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Error(err)
	}
}
