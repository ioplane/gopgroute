//go:build sqlnative

package pooler

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
)

func run(t *testing.T, stdin string, env []string, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture %s failed: %v %s", name, err, output)
	}
}

func launch(t *testing.T, name string, args ...string) func() {
	t.Helper()
	cmd := exec.Command(name, args...)
	log, err := os.CreateTemp(t.TempDir(), "process.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = log
	cmd.Stderr = log
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
			case <-done:
			case <-time.After(3 * time.Second):
				if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Error(err)
				}
				<-done
			}
			if err := log.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func ready(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			if err := connection.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fixture did not listen: %s", address)
}

func readyPostgres(t *testing.T, port string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		err := exec.CommandContext(ctx, "pg_isready", "-q", "-h", "127.0.0.1", "-p", port, "-U", "bootstrap").Run()
		cancel()
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PostgreSQL fixture not ready on %s", port)
}

func TestNativeSQLThroughPgBouncer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("native PostgreSQL fixture requires an unprivileged user")
	}
	for _, binary := range []string{"initdb", "postgres", "pgbouncer", "pg_basebackup", "psql"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatal(err)
		}
	}
	for _, component := range []struct{ name, prefix string }{{"postgres", "postgres (PostgreSQL) 18.6\n"}, {"pgbouncer", "PgBouncer 1.26.1\n"}} {
		output, err := exec.CommandContext(t.Context(), component.name, "--version").Output()
		actual := strings.Join(strings.Fields(string(output)), " ") + " "
		want := strings.Join(strings.Fields(component.prefix), " ") + " "
		if err != nil || !strings.HasPrefix(actual, want) {
			t.Fatalf("component version mismatch: %s", component.name)
		}
		t.Log(strings.TrimSpace(component.prefix))
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	password := "synthetic-test-password"
	passfile := filepath.Join(root, "password")
	if err := os.WriteFile(passfile, []byte(password), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "primary")
	run(t, "", nil, "initdb", "-D", data, "-U", "bootstrap", "--auth-local=trust", "--auth-host=scram-sha-256", "--no-sync", "--pwfile="+passfile)
	stopPrimary := launch(t, "postgres", "-D", data, "-k", root, "-h", "127.0.0.1", "-p", "15432", "-c", "shared_buffers=16MB", "-c", "max_connections=20", "-c", "fsync=off")
	readyPostgres(t, "15432")
	sql := func(statement string) {
		t.Helper()
		run(t, statement, nil, "psql", "-h", root, "-p", "15432", "-U", "bootstrap", "-d", "postgres", "-v", "ON_ERROR_STOP=1")
	}
	sql("CREATE ROLE monitor LOGIN PASSWORD 'synthetic-test-password';\nCREATE DATABASE example;\n")
	standby := filepath.Join(root, "replica")
	run(t, "", []string{"PGPASSWORD=" + password}, "pg_basebackup", "-h", "127.0.0.1", "-p", "15432", "-U", "bootstrap", "-D", standby, "-R", "-X", "stream", "--checkpoint=fast", "--no-sync")
	launch(t, "postgres", "-D", standby, "-k", root, "-h", "127.0.0.1", "-p", "15433", "-c", "shared_buffers=16MB", "-c", "max_connections=20", "-c", "fsync=off")
	readyPostgres(t, "15433")
	bouncer := func(port, backend uint16, database string) Config {
		t.Helper()
		dir := filepath.Join(root, fmt.Sprint(port))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		auth := filepath.Join(dir, "users.txt")
		if err := os.WriteFile(auth, []byte(`"monitor" "synthetic-test-password"`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		config := fmt.Sprintf("[databases]\nexample = host=127.0.0.1 port=%d dbname=%s\n[pgbouncer]\nlisten_addr=127.0.0.1\nlisten_port=%d\nunix_socket_dir=\nauth_type=scram-sha-256\nauth_file=%s\npool_mode=transaction\nmax_client_conn=10\ndefault_pool_size=2\nlog_connections=0\nlog_disconnections=0\n", backend, database, port, auth)
		file := filepath.Join(dir, "pgbouncer.ini")
		if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		launch(t, "pgbouncer", file)
		ready(t, fmt.Sprintf("127.0.0.1:%d", port))
		cfg := fixture()
		cfg.Address = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
		cfg.Backend = netip.AddrPortFrom(cfg.Address.Addr(), backend)
		return cfg
	}
	primary := bouncer(16432, 15432, "example")
	replica := bouncer(16433, 15433, "example")
	wrongDB := bouncer(16434, 15432, "postgres")
	check := func(t *testing.T, cfg Config, role health.Role, want health.State, targets ...error) {
		t.Helper()
		p, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		o, err := p.Check(t.Context(), role)
		if o.Check.State != want || o.Role != role || o.Check.StartedAt.Before(start) || (want == health.Unknown) != (err != nil) {
			t.Fatalf("SQL observation: state=%v role=%v error=%v, want %v", o.Check.State, o.Role, err, want)
		}
		if err != nil && strings.Contains(err.Error(), cfg.Password) {
			t.Fatal("password disclosed")
		}
		for _, target := range targets {
			if !errors.Is(err, target) {
				t.Fatalf("SQL observation error %v, want %v", err, target)
			}
		}
	}
	t.Run("primary", func(t *testing.T) { check(t, primary, health.Primary, health.Healthy) })
	t.Run("cancel-during-close", func(t *testing.T) {
		p, err := New(primary)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		p.connection.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &cancelOnClose{Conn: conn, cancel: cancel}, nil
		}
		observation, err := p.Check(ctx, health.Primary)
		if observation.Check.State != health.Unknown || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled close retained success: state=%v error=%v", observation.Check.State, err)
		}
	})
	t.Run("replica", func(t *testing.T) { check(t, replica, health.Replica, health.Healthy) })
	t.Run("wrong-role", func(t *testing.T) { check(t, primary, health.Replica, health.Unhealthy) })
	t.Run("wrong-database", func(t *testing.T) { check(t, wrongDB, health.Primary, health.Unhealthy) })
	t.Run("wrong-backend", func(t *testing.T) {
		cfg := primary
		cfg.Backend = netip.MustParseAddrPort("127.0.0.1:15433")
		check(t, cfg, health.Primary, health.Unhealthy)
	})
	t.Run("bad-password", func(t *testing.T) {
		cfg := primary
		cfg.Password = "synthetic-wrong-password"
		check(t, cfg, health.Primary, health.Unknown)
	})
	for _, kind := range []string{"malformed-row", "extra-row", "zero-rows", "oversized-row", "query-timeout"} {
		t.Run(kind, func(t *testing.T) {
			cfg := primary
			cfg.Address = listener(t, func(front net.Conn) {
				upstream, err := net.DialTimeout("tcp", primary.Address.String(), time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, front); _ = upstream.Close(); close(copied) }()
				defer func() { _ = front.Close(); _ = upstream.Close(); <-copied }()
				for {
					var header [5]byte
					if _, err := io.ReadFull(upstream, header[:]); err != nil {
						return
					}
					length := binary.BigEndian.Uint32(header[1:])
					if length < 4 || length > 64*1024 {
						t.Error("invalid fixture packet length")
						return
					}
					body := make([]byte, int(length)-4)
					if _, err := io.ReadFull(upstream, body); err != nil {
						return
					}
					if header[0] == 'D' {
						switch kind {
						case "query-timeout":
							// pgx asyncClose waits for the server close, so stall only until the subtest ends.
							select {
							case <-copied:
							case <-t.Context().Done():
							}
							return
						case "zero-rows":
							continue
						case "malformed-row":
							if len(body) < 6 {
								t.Error("short fixture row")
								return
							}
							binary.BigEndian.PutUint32(body[2:6], 0x7fffffff)
						case "oversized-row":
							binary.BigEndian.PutUint32(header[1:], 64*1024+5)
							body = nil
						case "extra-row":
							if _, err := front.Write(append(header[:], body...)); err != nil {
								return
							}
						}
					}
					if _, err := front.Write(append(header[:], body...)); err != nil {
						return
					}
				}
			})
			switch kind {
			case "query-timeout":
				check(t, cfg, health.Primary, health.Unknown, ErrQuery, context.DeadlineExceeded)
			case "oversized-row":
				// A long deadline separates the body limit from waiting for a missing body.
				cfg.Timeout = 5 * time.Second
				start := time.Now()
				check(t, cfg, health.Primary, health.Unknown, ErrQuery)
				if time.Since(start) > time.Second {
					t.Fatal("oversized row waited for its body")
				}
			default:
				check(t, cfg, health.Primary, health.Unknown, ErrQuery)
			}
		})
	}
	sql("ALTER ROLE monitor SET default_transaction_read_only=on;\n")
	readOnly := bouncer(16435, 15432, "example")
	t.Run("read-only-primary", func(t *testing.T) { check(t, readOnly, health.Primary, health.Unhealthy) })
	// Function ACLs are per database; revoke where the probe queries.
	sql("\\connect example\nREVOKE EXECUTE ON FUNCTION pg_catalog.pg_is_in_recovery() FROM PUBLIC;\n")
	t.Run("query-permission-error", func(t *testing.T) { check(t, primary, health.Primary, health.Unknown, ErrQuery) })
	stopPrimary()
	t.Run("backend-lost", func(t *testing.T) { check(t, primary, health.Primary, health.Unknown) })
}

type cancelOnClose struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error { c.cancel(); return c.Conn.Close() }
