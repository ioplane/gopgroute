package lab

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type scenario struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Role   string `json:"role,omitempty"`
	Node   int    `json:"node,omitempty"`
	Other  int    `json:"other,omitempty"`
	Actor  string `json:"actor,omitempty"`
	Signal string `json:"signal,omitempty"`
}

func catalog() []scenario {
	r := []scenario{{ID: "baseline", Kind: "baseline"}}
	for i := 1; i <= 3; i++ {
		r = append(r, scenario{ID: fmt.Sprintf("switchover-db%d", i), Kind: "switch", Node: i})
	}
	for _, role := range []string{"primary", "replica"} {
		for _, actor := range []string{"guardian", "adapter", "speaker"} {
			for _, sig := range []string{"KILL", "STOP"} {
				r = append(r, scenario{ID: strings.ToLower(role + "-" + actor + "-" + sig), Kind: "route", Role: role, Actor: actor, Signal: sig})
			}
		}
		for _, kind := range []string{"pooler", "pg-tree", "patroni"} {
			r = append(r, scenario{ID: role + "-" + kind + "-stop", Kind: kind, Role: role})
		}
	}
	for _, role := range []string{"primary", "replica", "replica2"} {
		r = append(r, scenario{ID: role + "-db-loss", Kind: "db-loss", Role: role})
	}
	for i := 1; i <= 3; i++ {
		r = append(r, scenario{ID: fmt.Sprintf("dcs-voter-%d-stop", i), Kind: "dcs", Node: i})
		r = append(r, scenario{ID: fmt.Sprintf("leaf-%d-loss", i), Kind: "leaf", Node: i})
	}
	for i := 1; i <= 3; i++ {
		for j := i + 1; j <= 3; j++ {
			r = append(r, scenario{ID: fmt.Sprintf("dcs-quorum-%d-%d-loss", i, j), Kind: "dcs", Node: i, Other: j})
			r = append(r, scenario{ID: fmt.Sprintf("bgp-edge-%d-%d-loss", i, j), Kind: "edge", Node: i, Other: j})
		}
	}
	r = append(r, scenario{ID: "primary-dcs-client-partition", Kind: "dcs-client", Role: "primary"}, scenario{ID: "replica-replay-lag", Kind: "lag", Role: "replica"}, scenario{ID: "replica2-replay-lag", Kind: "lag", Role: "replica2"}, scenario{ID: "guardian-adapter-stall-boundary", Kind: "paired", Role: "primary"})
	for _, id := range []string{"second-guardian", "runtime-root-change", "journal-root-change", "speaker-lease-exec", "adapter-lease-inheritance", "legacy-root-refusal", "uncertain-generation-retirement"} {
		r = append(r, scenario{ID: "native-" + id, Kind: "native"})
	}
	return r
}

type snapshot struct {
	Nodes  map[string]probe `json:"nodes"`
	Routes map[string]probe `json:"routes"`
	Leaves map[string]probe `json:"leaves"`
	Client probe            `json:"client"`
}

func (c *controller) snapshot(ctx context.Context) (snapshot, error) {
	s := snapshot{Nodes: map[string]probe{}, Routes: map[string]probe{}, Leaves: map[string]probe{}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var result error
	for _, kind := range []string{"db", "route", "leaf"} {
		for i := 1; i <= 3; i++ {
			wg.Add(1)
			go func(kind string, i int) {
				defer wg.Done()
				mode := kind
				if kind == "db" {
					mode = "node"
				}
				p, e := c.readProbe(ctx, fmt.Sprintf("%s%d", kind, i), mode)
				mu.Lock()
				defer mu.Unlock()
				if e != nil {
					result = errors.Join(result, e)
					return
				}
				target := s.Nodes
				if kind == "route" {
					target = s.Routes
				}
				if kind == "leaf" {
					target = s.Leaves
				}
				target[strconv.Itoa(i)] = p
			}(kind, i)
		}
		wg.Wait()
		if result != nil {
			return s, result
		}
	}
	p, e := c.readProbe(ctx, "client", "client")
	s.Client = p
	return s, e
}

func (s snapshot) roles() (int, []int, error) {
	primary := 0
	var replicas []int
	for i := 1; i <= 3; i++ {
		p, ok := s.Nodes[strconv.Itoa(i)]
		if !ok {
			return 0, nil, errors.New("missing node")
		}
		recovery, e := p.boolean("postgres", "recovery")
		if e != nil {
			return 0, nil, e
		}
		role, e := p.text("patroni", "role")
		if e != nil {
			return 0, nil, e
		}
		if recovery {
			replicas = append(replicas, i)
		} else {
			if primary != 0 || role != "primary" {
				return 0, nil, errors.New("multiple or inconsistent primaries")
			}
			primary = i
		}
	}
	if primary == 0 || len(replicas) != 2 {
		return 0, nil, errors.New("incomplete PostgreSQL topology")
	}
	return primary, replicas, nil
}

func (s snapshot) healthy() error {
	p, _, e := s.roles()
	if e != nil {
		return e
	}
	for _, n := range s.Nodes {
		ok, e := n.boolean("etcd", "health")
		if e != nil || !ok {
			return errors.Join(e, errors.New("DCS unhealthy"))
		}
		codes, e := n.number("patroni", "http_codes", "patroni")
		if e != nil || codes != 200 {
			return errors.Join(e, errors.New("patroni not ready"))
		}
	}
	replication, e := s.Nodes[strconv.Itoa(p)].list("postgres", "replication")
	if e != nil || len(replication) != 2 {
		return errors.Join(e, errors.New("two streaming replicas required"))
	}
	synchronous := 0
	for _, v := range replication {
		r, e := object(v)
		if e != nil {
			return e
		}
		state, e := r.text("state")
		if e != nil || state != "streaming" {
			return errors.Join(e, errors.New("replica not streaming"))
		}
		ssl, e := r.boolean("ssl")
		if e != nil || !ssl {
			return errors.Join(e, errors.New("replication TLS required"))
		}
		state, e = r.text("sync_state")
		if e != nil {
			return e
		}
		if state == "sync" || state == "quorum" {
			synchronous++
		}
		lag, e := r.number("replay_lag_bytes")
		if e != nil || lag != 0 {
			return errors.Join(e, errors.New("replica not caught up"))
		}
	}
	if synchronous != 1 {
		return errors.New("effective synchronous standby count is not one")
	}
	for _, l := range s.Leaves {
		peers, e := l.list("peers")
		if e != nil || len(peers) != 3 {
			return errors.Join(e, errors.New("three peers per leaf required"))
		}
		for _, v := range peers {
			peer, e := object(v)
			if e != nil {
				return e
			}
			state, e := peer.text("state")
			if e != nil || state != "Established" {
				return errors.Join(e, errors.New("BGP not established"))
			}
			gr, e := peer.text("local_gr_mode")
			if e != nil || gr != "Disable" {
				return errors.Join(e, errors.New("GR must be disabled"))
			}
		}
		for _, vip := range []string{"write", "read"} {
			ok, e := l.boolean("vips", vip, "installed")
			if e != nil || !ok {
				return errors.Join(e, errors.New("VIP absent from FIB"))
			}
		}
	}
	ok, e := s.Client.boolean("ok")
	if e != nil || !ok {
		return errors.Join(e, errors.New("client unhealthy"))
	}
	if len(s.Routes) != 3 {
		return errors.New("incomplete route observations")
	}
	return nil
}

func (c *controller) restored(ctx context.Context) (snapshot, error) {
	var s snapshot
	err := poll(ctx, time.Duration(c.cfg.TimeoutSeconds)*time.Second, func(ctx context.Context) (bool, error) {
		v, e := c.snapshot(ctx)
		s = v
		if e != nil {
			return false, e
		}
		e = s.healthy()
		return e == nil, e
	})
	return s, err
}

func (c *controller) fault(ctx context.Context, node int, args ...string) (commandResult, error) {
	return c.execute(ctx, fmt.Sprintf("db%d", node), append([]string{"python3", "/lab/fault.py"}, args...)...)
}

func (c *controller) switchTo(ctx context.Context, target int) (result error) {
	s, e := c.snapshot(ctx)
	if e != nil {
		return e
	}
	primary, _, e := s.roles()
	if e != nil || primary == target {
		return e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Either endpoint can PATCH the same DCS setting. Never replay the switchover.
		_, first := c.fault(cleanup, target, "sync-count", "1")
		if first != nil {
			_, second := c.fault(cleanup, primary, "sync-count", "1")
			if second != nil {
				result = errors.Join(result, first, second)
			}
		}
	}()
	// Admit both caught-up replicas as synchronous candidates before selection.
	if _, e = c.fault(ctx, primary, "sync-count", "2"); e != nil {
		return e
	}
	e = poll(ctx, 30*time.Second, func(ctx context.Context) (bool, error) {
		n, e := c.readProbe(ctx, fmt.Sprintf("db%d", primary), "node")
		if e != nil {
			return false, e
		}
		rows, e := n.list("postgres", "replication")
		if e != nil {
			return false, e
		}
		count := 0
		for _, v := range rows {
			r, e := object(v)
			if e != nil {
				return false, e
			}
			state, e := r.text("sync_state")
			if e != nil {
				return false, e
			}
			if state == "sync" || state == "quorum" {
				count++
			}
		}
		return count == 2, nil
	})
	if e == nil {
		_, e = c.fault(ctx, primary, "switch", fmt.Sprintf("db%d", primary), fmt.Sprintf("db%d", target))
	}
	if e != nil {
		return e
	}
	return nil
}

func (c *controller) refreshRoute(ctx context.Context, node int) (result error) {
	service := fmt.Sprintf("route%d", node)
	defer func() {
		if result == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		result = errors.Join(result, c.composeRoute(cleanup, service))
	}()
	if _, e := c.podman(ctx, "stop", "--time", "10", c.name(service)); e != nil {
		return e
	}
	if _, e := c.podman(ctx, "rm", c.name(service)); e != nil {
		return e
	}
	return c.composeRoute(ctx, service)
}

func (c *controller) composeRoute(ctx context.Context, service string) error {
	_, e := c.command(ctx, 30*time.Second, "podman-compose", "--in-pod", "false", "--podman-run-args=--image-volume=ignore", "--env-file", c.cfg.Root+"/lab.env", "-f", c.cfg.Root+"/compose.yaml", "-p", c.cfg.Project, "up", "-d", "--no-deps", service)
	return e
}
