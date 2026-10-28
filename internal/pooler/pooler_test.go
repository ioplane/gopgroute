package pooler

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/jackc/pgx/v5/pgproto3"
)

func fixture() Config {
	return Config{Address: netip.MustParseAddrPort("127.0.0.1:6432"), Backend: netip.MustParseAddrPort("127.0.0.1:5432"), Database: "example", User: "monitor", Password: "synthetic-test-password", Timeout: 200 * time.Millisecond}
}

func TestConfigurationBoundary(t *testing.T) {
	for _, kind := range []string{"remote", "mapped", "port", "backend", "direct", "database", "admin", "user", "password", "NUL", "timeout", "environment"} {
		t.Run(kind, func(t *testing.T) {
			c := fixture()
			switch kind {
			case "remote":
				c.Address = netip.MustParseAddrPort("192.0.2.1:6432")
			case "mapped":
				c.Address = netip.MustParseAddrPort("[::ffff:127.0.0.1]:6432")
			case "port":
				c.Address = netip.MustParseAddrPort("127.0.0.1:0")
			case "backend":
				c.Backend = netip.MustParseAddrPort("192.0.2.1:5432")
			case "direct":
				c.Address = c.Backend
			case "database":
				c.Database = ""
			case "admin":
				c.Database = "pgbouncer"
			case "user":
				c.User = ""
			case "password":
				c.Password = ""
			case "NUL":
				c.Password = "not\x00valid"
			case "timeout":
				c.Timeout = 0
			case "environment":
				t.Setenv("PGSERVICE", "untrusted")
			}
			if p, err := New(c); p != nil || err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

func listener(t *testing.T, handler func(net.Conn)) netip.AddrPort {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() {
			if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		}()
		handler(c)
	}()
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("protocol fixture did not stop")
		}
	})
	return l.Addr().(*net.TCPAddr).AddrPort()
}

func TestUnknownOnProtocolErrorAndNoCredentialDisclosure(t *testing.T) {
	marker := "synthetic-server-error-password"
	c := fixture()
	c.Address = listener(t, func(conn net.Conn) {
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "28000", Message: marker})
		if err := backend.Flush(); err != nil {
			t.Error(err)
		}
	})
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	o, err := p.Check(t.Context(), health.Primary)
	if err == nil || o.Check.State != health.Unknown || o.Role != health.Primary || o.Check.StartedAt.IsZero() {
		t.Fatalf("invalid error observation: %+v %v", o, err)
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), c.Password) {
		t.Fatal("untrusted error disclosed")
	}
}

func TestRejectsAuthenticationDowngradeBeforePassword(t *testing.T) {
	for _, method := range []struct {
		name    string
		message pgproto3.BackendMessage
	}{{"cleartext", &pgproto3.AuthenticationCleartextPassword{}}, {"md5", &pgproto3.AuthenticationMD5Password{}}, {"trust", &pgproto3.AuthenticationOk{}}} {
		t.Run(method.name, func(t *testing.T) {
			c := fixture()
			c.Address = listener(t, func(conn net.Conn) {
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(method.message)
				if err := backend.Flush(); err != nil {
					return
				}
				message, _ := backend.Receive()
				if _, ok := message.(*pgproto3.PasswordMessage); ok {
					t.Error("password sent to downgraded authentication")
				}
			})
			p, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			o, err := p.Check(t.Context(), health.Primary)
			if err == nil || o.Check.State != health.Unknown {
				t.Fatal("auth downgrade accepted")
			}
		})
	}
}

func TestOversizedProtocolHeaderRejectedBeforeBody(t *testing.T) {
	cfg := fixture()
	cfg.Timeout = 2 * time.Second
	cfg.Address = listener(t, func(conn net.Conn) {
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		// Pass the SASL gate so only the driver body limit can reject the next header.
		backend.Send(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}})
		if err := backend.Flush(); err != nil {
			return
		}
		var data [512]byte
		if _, err := conn.Read(data[:]); err != nil {
			return
		}
		if _, err := conn.Write([]byte{'R', 0, 1, 0, 5}); err != nil {
			t.Error(err)
			return
		}
		_, _ = conn.Read(data[:])
	})
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	o, err := p.Check(t.Context(), health.Primary)
	if err == nil || o.Check.State != health.Unknown || time.Since(start) > time.Second {
		t.Fatal("oversized body waited for payload or authorized health")
	}
}

func TestDeadlineAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[cancelled], func(t *testing.T) {
			c := fixture()
			c.Timeout = 40 * time.Millisecond
			c.Address = listener(t, func(conn net.Conn) {
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			})
			p, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			start := time.Now()
			o, err := p.Check(ctx, health.Replica)
			if err == nil || o.Check.State != health.Unknown || time.Since(start) > time.Second {
				t.Fatalf("unbounded/positive failure: %v %+v", err, o)
			}
		})
	}
}
