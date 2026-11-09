//go:build linux && sqlnative

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func fixtureCommand(t *testing.T, input, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- Test-local PostgreSQL executables and synthetic arguments.
	cmd.Stdin = strings.NewReader(input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v %s", name, err, output)
	}
}

func fixtureProcess(t *testing.T, name string, args ...string) func() {
	t.Helper()
	cmd := exec.Command(name, args...) // #nosec G204 -- Test-local PostgreSQL or PgBouncer fixture.
	log, err := os.CreateTemp(t.TempDir(), "fixture.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if err := cmd.Process.Signal(syscall.SIGINT); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			select {
			case err := <-done:
				var exit *exec.ExitError
				if err != nil && !errors.As(err, &exit) {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Error(err)
				}
				if err := <-done; err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Error(err)
					}
				}
			}
			if err := log.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestNativeDaemonSQLRouteLifecycle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("PostgreSQL fixture requires an unprivileged user")
	}
	cfg := testConfig(t)
	cfg.ProbeTimeout = Duration(500 * time.Millisecond)
	cfg.CycleTimeout = Duration(3 * time.Second)
	cfg.MaxAge = Duration(5 * time.Second)
	cfg.HeartbeatTimeout = Duration(10 * time.Second)
	cfg.StartupTimeout = Duration(3 * time.Second)
	root := filepath.Dir(cfg.RuntimeDir)
	data := filepath.Join(root, "pgdata")
	fixtureCommand(t, "", "initdb", "-D", data, "-U", "bootstrap", "--auth-local=trust", "--auth-host=scram-sha-256", "--no-sync", "--pwfile="+cfg.PasswordFile)
	fixtureProcess(t, "postgres", "-D", data, "-k", root, "-h", "127.0.0.1", "-p", "15432", "-c", "shared_buffers=16MB", "-c", "max_connections=20", "-c", "fsync=off")
	ready, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := waitUntil(ready, func() (bool, error) {
		call, stop := context.WithTimeout(ready, 200*time.Millisecond)
		defer stop()
		err := exec.CommandContext(call, "pg_isready", "-q", "-h", "127.0.0.1", "-p", "15432", "-U", "bootstrap").Run()
		return err == nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fixtureCommand(t, "CREATE ROLE route_health LOGIN PASSWORD 'test-only';\nCREATE DATABASE application;\n", "psql", "-h", root, "-p", "15432", "-U", "bootstrap", "-d", "postgres", "-v", "ON_ERROR_STOP=1")
	auth := filepath.Join(root, "users.txt")
	if err := os.WriteFile(auth, []byte("\"route_health\" \"test-only\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "pgbouncer.ini")
	text := fmt.Sprintf("[databases]\napplication = host=127.0.0.1 port=15432 dbname=application\n[pgbouncer]\nlisten_addr=127.0.0.1\nlisten_port=16432\nunix_socket_dir=\nauth_type=scram-sha-256\nauth_file=%s\npool_mode=transaction\nmax_client_conn=10\ndefault_pool_size=2\n", auth)
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	stopPooler := fixtureProcess(t, "pgbouncer", config)
	if err := waitUntil(ready, func() (bool, error) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:16432", 50*time.Millisecond)
		if err != nil {
			return false, nil
		}
		return true, conn.Close()
	}); err != nil {
		t.Fatal(err)
	}
	cfg.PoolerAddress = netip.MustParseAddrPort("127.0.0.1:16432")
	cfg.Backend = netip.MustParseAddrPort("127.0.0.1:15432")
	var replica atomic.Bool
	patroni := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := "primary"
		if replica.Load() {
			role = "replica"
		}
		if r.URL.Path != "/"+role {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if _, err := fmt.Fprintf(w, `{"state":"running","role":"%s"}`, role); err != nil {
			t.Error(err)
		}
	}))
	defer patroni.Close()
	cfg.PatroniURL = patroni.URL
	speaker, adapter := childCommand("speaker"), childCommand("adapter")
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Guardian(ctx, cfg, speaker, adapter); close(done) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Log(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("guardian did not reap native children")
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
	client := api.NewGoBgpServiceClient(connection)
	assertRIB := func(want string) {
		t.Helper()
		deadline, stop := context.WithTimeout(t.Context(), 8*time.Second)
		defer stop()
		if err := waitUntil(deadline, func() (bool, error) {
			call, cancel := context.WithTimeout(deadline, 500*time.Millisecond)
			defer cancel()
			stream, err := client.ListPath(call, &api.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}}, grpc.WaitForReady(true))
			if err != nil {
				return false, nil
			}
			var prefixes []string
			for {
				entry, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return false, err
				}
				prefixes = append(prefixes, entry.GetDestination().GetPrefix())
			}
			return (want == "" && len(prefixes) == 0) || (len(prefixes) == 1 && prefixes[0] == want), nil
		}); err != nil {
			t.Fatalf("native RIB did not become %q: %v", want, err)
		}
	}
	assertRIB(cfg.Write.String())
	replica.Store(true)
	assertRIB("") // Patroni replica and writable primary SQL must never combine.
	replica.Store(false)
	assertRIB(cfg.Write.String())
	stopPooler()
	assertRIB("")
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || speaker.ProcessState == nil || adapter.ProcessState == nil {
			t.Fatalf("native shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native shutdown exceeded bound")
	}
	probe, finish := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer finish()
	if _, err := client.GetBgp(probe, &api.GetBgpRequest{}); err == nil {
		t.Fatal("speaker survived native shutdown")
	}
}
