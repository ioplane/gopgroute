package pooler

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/jackc/pgx/v5/pgproto3"
)

func TestRejectsReadyBeforeAuthentication(t *testing.T) {
	cfg := fixture()
	cfg.Address = listener(t, func(conn net.Conn) {
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			return
		}
		for {
			message, err := backend.Receive()
			if err != nil {
				return
			}
			if _, ok := message.(*pgproto3.Sync); ok {
				break
			}
		}
		fields := []pgproto3.FieldDescription{
			{Name: []byte("database"), DataTypeOID: 25, DataTypeSize: -1},
			{Name: []byte("user"), DataTypeOID: 25, DataTypeSize: -1},
			{Name: []byte("address"), DataTypeOID: 25, DataTypeSize: -1},
			{Name: []byte("port"), DataTypeOID: 23, DataTypeSize: 4},
			{Name: []byte("recovery"), DataTypeOID: 16, DataTypeSize: 1},
			{Name: []byte("read_only"), DataTypeOID: 16, DataTypeSize: 1},
		}
		backend.Send(&pgproto3.ParseComplete{})
		backend.Send(&pgproto3.BindComplete{})
		backend.Send(&pgproto3.RowDescription{Fields: fields})
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(cfg.Database), []byte(cfg.User), []byte("127.0.0.1"), []byte("5432"), []byte("f"), []byte("f")}})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			return
		}
		_, _ = backend.Receive()
	})
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := p.Check(t.Context(), health.Primary)
	if observation.Check.State != health.Unknown || err == nil {
		t.Fatalf("unauthenticated server authorized health: state=%v error=%v", observation.Check.State, err)
	}
}

func TestRejectsMalformedFirstAuthenticationRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"short", []byte{'R', 0, 0, 0, 7, 0, 0, 0}},
		{"oversized", []byte{'R', 0, 1, 0, 5, 0, 0, 0, 10}},
		{"cleartext", []byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}},
		{"truncated", []byte{'R', 0, 0, 0, 23, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixture()
			cfg.Timeout = 2 * time.Second
			cfg.Address = listener(t, func(conn net.Conn) {
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				if _, err := conn.Write(tc.data); err != nil {
					return
				}
				if tc.name == "truncated" {
					return
				}
				var data [1]byte
				_, _ = conn.Read(data[:])
			})
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			o, err := p.Check(t.Context(), health.Primary)
			if o.Check.State != health.Unknown || !errors.Is(err, ErrConnect) || time.Since(start) > time.Second {
				t.Fatalf("malformed authentication request: state=%v error=%v", o.Check.State, err)
			}
		})
	}
}
