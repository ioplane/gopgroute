package gobgp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	ErrOccupied  = errors.New("target prefix occupied")
	ErrRecovery  = errors.New("speaker recovery required")
	ErrUncertain = errors.New("mutation outcome uncertain")
)

type (
	Config struct {
		Write, Read        netip.Prefix
		NextHop            netip.Addr
		RPCTimeout, MaxAge time.Duration
	}
	Manager struct {
		mu               sync.Mutex
		cfg              Config
		client           api.GoBgpServiceClient
		state            *store
		journal          journal
		poisoned, closed bool
		connection       *grpc.ClientConn
	}
)

// Open accepts only a private service-owned Unix socket. Close preserves routes
// and their journal; runtime shutdown and crash handling belong to T3.
func Open(ctx context.Context, cfg Config, dir, socket string) (*Manager, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 107 {
		return nil, errors.New("invalid Unix socket path")
	}
	parent, err := os.Lstat(filepath.Dir(socket))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || !private(parent, 0o700) {
		return nil, errors.New("socket directory must be private and service-owned")
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 || !private(info, 0o600) {
		return nil, errors.New("socket must be private and service-owned")
	}
	connection, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		return nil, fmt.Errorf("connect GoBGP: %w", err)
	}
	m, err := newManager(ctx, cfg, dir, api.NewGoBgpServiceClient(connection))
	if err != nil {
		return nil, errors.Join(err, connection.Close())
	}
	m.connection = connection
	return m, nil
}

func newManager(ctx context.Context, cfg Config, dir string, c api.GoBgpServiceClient) (*Manager, error) {
	if !cfg.Write.IsValid() || !cfg.Read.IsValid() || !cfg.Write.Addr().Is4() || !cfg.Read.Addr().Is4() || !cfg.Write.Addr().IsGlobalUnicast() || !cfg.Read.Addr().IsGlobalUnicast() || cfg.Write.Bits() != 32 || cfg.Read.Bits() != 32 || cfg.Write == cfg.Read || !cfg.NextHop.Is4() || !cfg.NextHop.IsGlobalUnicast() || cfg.RPCTimeout <= 0 || cfg.MaxAge <= 0 || c == nil {
		return nil, errors.New("invalid route configuration")
	}
	state, err := openStore(dir)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	m := &Manager{cfg: cfg, client: c, state: state, journal: journal{Version: 1, Write: cfg.Write.String(), Read: cfg.Read.String(), NextHop: cfg.NextHop.String()}}
	stored, err := state.load()
	if errors.Is(err, os.ErrNotExist) {
		err = state.save(m.journal)
	} else if err == nil {
		if stored.Version != 1 || stored.Write != m.journal.Write || stored.Read != m.journal.Read || stored.NextHop != m.journal.NextHop {
			err = errors.New("journal identity differs from configuration")
		} else {
			m.journal = stored
		}
	}
	if err == nil {
		for _, e := range m.journal.Entries {
			if e.Phase != "" && e.Phase != "owned" {
				err = ErrRecovery
				break
			}
		}
	}
	if err == nil {
		for i, e := range m.journal.Entries {
			if e.Phase == "owned" {
				if err = m.withdraw(ctx, i); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		return nil, errors.Join(ErrRecovery, err, state.close())
	}
	return m, nil
}

// Reconcile consumes observations rather than cached booleans so freshness is
// evaluated again after read-only preflight and durable pending state.
// A manager belongs to one speaker incarnation; nil is not RIB attestation.
func (m *Manager) Reconcile(ctx context.Context, s health.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.poisoned {
		return ErrRecovery
	}
	intent := health.Decide(time.Now(), m.cfg.MaxAge, s)
	wanted := [2]bool{intent.Write, intent.Read}
	for i, e := range m.journal.Entries {
		if e.Phase == "owned" && !wanted[i] {
			if err := m.withdraw(ctx, i); err != nil {
				return err
			}
		}
	}
	for i := range wanted {
		intent = health.Decide(time.Now(), m.cfg.MaxAge, s)
		wanted = [2]bool{intent.Write, intent.Read}
		if m.journal.Entries[i].Phase == "owned" && !wanted[i] {
			if err := m.withdraw(ctx, i); err != nil {
				return err
			}
		}
		if wanted[i] && m.journal.Entries[i].Phase == "" {
			if err := m.advertise(ctx, i, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) fail(err error) error {
	m.poisoned = true
	return errors.Join(ErrRecovery, ErrUncertain, err)
}

func (m *Manager) save() error {
	if err := m.state.save(m.journal); err != nil {
		return m.fail(fmt.Errorf("persist ownership: %w", err))
	}
	return nil
}

func (m *Manager) withdraw(ctx context.Context, i int) error {
	uuid, err := decodeUUID(m.journal.Entries[i].UUID)
	if err != nil {
		return m.fail(err)
	}
	m.journal.Entries[i].Phase = "pending-delete"
	if err = m.save(); err != nil {
		return err
	}
	call, cancel := context.WithTimeout(ctx, m.cfg.RPCTimeout)
	defer cancel()
	if _, err = m.client.DeletePath(call, &api.DeletePathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Uuid: uuid}); err != nil {
		return m.fail(fmt.Errorf("delete owned UUID: %w", err))
	}
	m.journal.Entries[i] = entry{}
	return m.save()
}

func (m *Manager) advertise(ctx context.Context, i int, s health.Snapshot) error {
	prefix := [2]netip.Prefix{m.cfg.Write, m.cfg.Read}[i]
	call, cancel := context.WithTimeout(ctx, m.cfg.RPCTimeout)
	stream, err := m.client.ListPath(call, &api.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: ipv4Family(), Prefixes: []*api.TableLookupPrefix{{Prefix: prefix.String(), Type: api.TableLookupPrefix_TYPE_EXACT}}})
	if err == nil {
		var response *api.ListPathResponse
		response, err = stream.Recv()
		if err == nil {
			if response.GetDestination() == nil || response.Destination.Prefix != prefix.String() {
				err = errors.New("unexpected RIB response")
			} else if len(response.Destination.Paths) > 0 {
				err = ErrOccupied
			} else {
				_, err = stream.Recv()
				if err == nil {
					err = errors.New("unexpected extra RIB response")
				}
			}
		}
	}
	cancel()
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("target preflight: %w", err)
	}
	nlri, err := bgp.NewIPAddrPrefix(prefix)
	if err != nil {
		return err
	}
	hop, err := bgp.NewPathAttributeNextHop(m.cfg.NextHop)
	if err != nil {
		return err
	}
	path, err := apiutil.NewPath(bgp.RF_IPv4_UC, nlri, false, []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0), hop}, time.Now())
	if err != nil {
		return err
	}
	m.journal.Entries[i] = entry{Phase: "pending-add"}
	if err = m.save(); err != nil {
		return err
	}
	intent := health.Decide(time.Now(), m.cfg.MaxAge, s)
	if (i == 0 && !intent.Write) || (i == 1 && !intent.Read) || ctx.Err() != nil {
		m.journal.Entries[i] = entry{}
		return m.save()
	}
	role := s.Primary
	if i == 1 {
		role = s.Replica
	}
	deadline := role.StartedAt.Add(m.cfg.MaxAge)
	if pooler := s.Pooler.StartedAt.Add(m.cfg.MaxAge); pooler.Before(deadline) {
		deadline = pooler
	}
	call, cancel = context.WithDeadline(ctx, deadline)
	defer cancel()
	call, rpcCancel := context.WithTimeout(call, m.cfg.RPCTimeout)
	defer rpcCancel()
	response, err := m.client.AddPath(call, &api.AddPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Path: path})
	if err != nil {
		return m.fail(fmt.Errorf("add owned prefix: %w", err))
	}
	if response == nil || !validUUID(response.Uuid) {
		return m.fail(errors.New("AddPath returned invalid UUID"))
	}
	m.journal.Entries[i] = entry{Phase: "owned", UUID: hex.EncodeToString(response.Uuid)}
	if err = m.save(); err != nil {
		return err
	}
	intent = health.Decide(time.Now(), m.cfg.MaxAge, s)
	if (i == 0 && !intent.Write) || (i == 1 && !intent.Read) {
		return m.withdraw(ctx, i)
	}
	return nil
}

func ipv4Family() *api.Family {
	return &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	err := m.state.close()
	if m.connection != nil {
		err = errors.Join(err, m.connection.Close())
	}
	return err
}
