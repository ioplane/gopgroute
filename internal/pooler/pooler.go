// Package pooler observes SQL eligibility through the local PgBouncer.
package pooler

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type (
	Config struct {
		Address, Backend         netip.AddrPort
		Database, User, Password string
		Timeout                  time.Duration
	}
	Observation struct {
		Check health.Check
		Role  health.Role
	}
	Probe struct {
		cfg        Config
		connection *pgx.ConnConfig
	}
)

var (
	ErrConfig  = errors.New("invalid pooler configuration")
	ErrConnect = errors.New("pooler connection failed")
	ErrQuery   = errors.New("pooler query failed")
	ErrClose   = errors.New("pooler close failed")
)

func local(address netip.AddrPort) bool {
	// A zone would make equal sockets compare unequal and bypass the pooler check.
	return address.IsValid() && address.Port() != 0 && address.Addr().IsLoopback() && !address.Addr().Is4In6() && address.Addr().Zone() == ""
}

func text(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func New(cfg Config) (*Probe, error) {
	if !local(cfg.Address) || !local(cfg.Backend) || cfg.Address == cfg.Backend || !text(cfg.Database, 63) || cfg.Database == "pgbouncer" || !text(cfg.User, 63) || !text(cfg.Password, 4096) || cfg.Timeout <= 0 {
		return nil, ErrConfig
	}
	for _, value := range os.Environ() {
		key, value, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "PG") && value != "" {
			return nil, errors.New("PG environment configuration is forbidden")
		}
	}
	// A fixed nonsecret seed avoids interpolating credentials or driver options.
	connection, err := pgx.ParseConfig("host=127.0.0.1 port=1 database=gopgroute user=gopgroute sslmode=disable passfile=/dev/null target_session_attrs=any require_auth=scram-sha-256 channel_binding=disable")
	if err != nil {
		return nil, ErrConfig
	}
	connection.Host = cfg.Address.Addr().String()
	connection.Port = cfg.Address.Port()
	connection.Database = cfg.Database
	connection.User = cfg.User
	connection.Password = cfg.Password
	connection.ConnectTimeout = cfg.Timeout
	connection.DialFunc = (&net.Dialer{}).DialContext
	connection.RuntimeParams = map[string]string{"application_name": "gopgroute", "client_encoding": "UTF8"}
	connection.DefaultQueryExecMode = pgx.QueryExecModeExec
	connection.StatementCacheCapacity = 0
	connection.DescriptionCacheCapacity = 0
	connection.MaxProtocolMessageBodyLen = 64 * 1024
	connection.AfterNetConnect = func(_ context.Context, _ *pgconn.Config, conn net.Conn) (net.Conn, error) {
		return &scramConnection{Conn: conn}, nil
	}
	return &Probe{cfg: cfg, connection: connection}, nil
}

const query = `SELECT pg_catalog.current_database(), current_user::pg_catalog.text,
pg_catalog.host(pg_catalog.inet_server_addr()), pg_catalog.inet_server_port(),
pg_catalog.pg_is_in_recovery(), pg_catalog.current_setting('transaction_read_only')::pg_catalog.bool`

// Check returns evidence only for its requested role; callers must retain that
// role when combining it with Patroni observations and recheck freshness.
func (p *Probe) Check(ctx context.Context, role health.Role) (observation Observation, err error) {
	observation = Observation{Check: health.Check{StartedAt: time.Now()}, Role: role}
	if role != health.Primary && role != health.Replica {
		return observation, ErrConfig
	}
	call, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	connection, err := pgx.ConnectConfig(call, p.connection.Copy())
	if err != nil {
		return observation, errors.Join(ErrConnect, call.Err())
	}
	defer func() {
		if closeErr := connection.Close(call); closeErr != nil || call.Err() != nil {
			observation.Check.State = health.Unknown
			err = errors.Join(err, ErrClose, call.Err())
		}
	}()
	rows, err := connection.Query(call, query)
	if err != nil {
		return observation, errors.Join(ErrQuery, call.Err())
	}
	defer rows.Close()
	if !rows.Next() {
		return observation, errors.Join(ErrQuery, call.Err())
	}
	var database, user string
	var address *string
	var port *int
	var recovery, readOnly bool
	if scanErr := rows.Scan(&database, &user, &address, &port, &recovery, &readOnly); scanErr != nil {
		return observation, errors.Join(ErrQuery, call.Err())
	}
	if rows.Next() || rows.Err() != nil {
		return observation, errors.Join(ErrQuery, call.Err())
	}
	observation.Check.State = health.Unhealthy
	if database != p.cfg.Database || user != p.cfg.User || address == nil || port == nil {
		return observation, nil
	}
	parsed, parseErr := netip.ParseAddr(*address)
	if parseErr != nil || parsed != p.cfg.Backend.Addr() || *port != int(p.cfg.Backend.Port()) {
		return observation, nil
	}
	if recovery == (role == health.Replica) && readOnly == recovery {
		observation.Check.State = health.Healthy
	}
	return observation, nil
}
