package lab

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	addresses = map[int]string{1: "192.0.2.10", 2: "198.51.100.10", 3: "203.0.113.10"}
	transit   = map[int]string{1: "198.18.0.11", 2: "198.18.0.12", 3: "198.18.0.13"}
)

type outcome struct {
	Scenario        scenario         `json:"scenario"`
	Verdict         string           `json:"verdict"`
	Error           string           `json:"error,omitempty"`
	Before          *snapshot        `json:"before,omitempty"`
	During          map[string]probe `json:"during,omitempty"`
	After           *snapshot        `json:"after,omitempty"`
	FaultSeconds    float64          `json:"fault_seconds"`
	RecoverySeconds float64          `json:"recovery_seconds"`
	Restored        bool             `json:"restored"`
	Commands        []commandResult  `json:"commands"`
}

type undo func(context.Context) error

func (c *controller) signal(ctx context.Context, service string, pid int, start, name string) error {
	_, e := c.execute(ctx, service, "python3", "/lab/fault.py", "signal", strconv.Itoa(pid), start, name)
	return e
}

func nodeIdentity(s snapshot, node int, actor string) (int, string, error) {
	n := s.Nodes[strconv.Itoa(node)]
	pid, e := n.number("process_ids", actor)
	if e != nil {
		return 0, "", e
	}
	start, e := n.text("process_start_ids", actor)
	return pid, start, e
}

func routeIdentity(s snapshot, node int, actor string) (int, string, error) {
	n := s.Routes[strconv.Itoa(node)]
	pid, e := n.number("processes", actor, "pid")
	if e != nil {
		return 0, "", e
	}
	start, e := n.text("processes", actor, "start_time")
	return pid, start, e
}

func (c *controller) origin(ctx context.Context, node int) (bool, probe, error) {
	p, e := c.readProbe(ctx, fmt.Sprintf("leaf%d", node), "leaf")
	if e != nil {
		return false, p, e
	}
	rows, e := p.list("peers")
	if e != nil {
		return false, p, e
	}
	for _, v := range rows {
		row, e := object(v)
		if e != nil {
			return false, p, e
		}
		address, e := row.text("address")
		if e != nil {
			return false, p, e
		}
		if address != addresses[node] {
			continue
		}
		count, e := row.number("accepted_prefixes")
		if e != nil {
			return false, p, e
		}
		return count > 0, p, nil
	}
	return false, p, errors.New("missing local BGP neighbor")
}

func (c *controller) observeOrigin(ctx context.Context, node int, want bool, r *outcome) error {
	return poll(ctx, 25*time.Second, func(ctx context.Context) (bool, error) {
		yes, p, e := c.origin(ctx, node)
		if p != nil {
			r.During["origin"] = p
		}
		return yes == want, e
	})
}

func (c *controller) changedLeader(ctx context.Context, old int, r *outcome) error {
	reader := old%3 + 1
	return poll(ctx, 45*time.Second, func(ctx context.Context) (bool, error) {
		v, e := c.fault(ctx, reader, "cluster")
		if e != nil {
			return false, e
		}
		var p probe
		if e = json.Unmarshal([]byte(v.Stdout), &p); e != nil {
			return false, e
		}
		r.During["cluster"] = p
		rows, e := p.list("members")
		if e != nil {
			return false, e
		}
		leaders := 0
		target := ""
		for _, v := range rows {
			m, e := object(v)
			if e != nil {
				return false, e
			}
			role, e := m.text("role")
			if e != nil {
				return false, e
			}
			if role == "leader" || role == "primary" {
				leaders++
				target, e = m.text("name")
				if e != nil {
					return false, e
				}
			}
		}
		return leaders == 1 && target != fmt.Sprintf("db%d", old), nil
	})
}

func (c *controller) available(ctx context.Context, r *outcome) error {
	return poll(ctx, 35*time.Second, func(ctx context.Context) (bool, error) {
		p, e := c.readProbe(ctx, "client", "client")
		r.During["client"] = p
		if e != nil {
			return false, e
		}
		return p.boolean("ok")
	})
}

func fencingConfirmed(p probe) (bool, error) {
	state, e := p.text("state")
	if e != nil {
		return false, e
	}
	switch state {
	case "demoted", "refused":
		return true, nil
	case "primary", "unknown":
		return false, fmt.Errorf("old primary fencing %s", state)
	default:
		return false, errors.New("unknown fencing observation state")
	}
}

func (c *controller) fencingObservation(ctx context.Context, node int) (probe, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if end, ok := ctx.Deadline(); ok && time.Until(end) < podmanCommandTimeout {
		return nil, errors.New("insufficient budget for a complete fencing observation; no worker launched")
	}
	p, e := c.readProbe(ctx, fmt.Sprintf("db%d", node), "fencing")
	stopped, stopErr := p.boolean("execution_stopped")
	if e != nil || stopErr != nil || !stopped {
		c.mu.Lock()
		c.uncertain = true
		c.mu.Unlock()
		return p, errors.Join(e, stopErr, errors.New("fencing observation worker completion unconfirmed; no replacement probe"))
	}
	return p, nil
}

func (c *controller) fenced(ctx context.Context, node int, r *outcome) error {
	var terminal error
	e := poll(ctx, 30*time.Second, func(ctx context.Context) (bool, error) {
		p, e := c.fencingObservation(ctx, node)
		if p != nil {
			r.During["old_primary"] = p
		}
		if e != nil {
			terminal = e
			return true, nil
		}
		// A fresh read may replace a completed observation, never an unresolved worker or effect.
		return fencingConfirmed(p)
	})
	return errors.Join(e, terminal)
}

func (c *controller) window(ctx context.Context, d time.Duration, check func() error) error {
	end := time.NewTimer(d)
	defer end.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if e := check(); e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-end.C:
			return nil
		case <-tick.C:
		}
	}
}

func (c *controller) caseRun(ctx context.Context, sc scenario, before snapshot, commit func(context.Context) (probe, error)) (r outcome) {
	r = outcome{Scenario: sc, Verdict: "PASS", Before: &before, During: map[string]probe{}}
	startCommands := len(c.commands)
	defer func() { r.Commands = append([]commandResult(nil), c.commands[startCommands:]...) }()
	var undos []undo
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Duration(c.cfg.TimeoutSeconds+30)*time.Second)
		defer cancel()
		var cleanupErr error
		t0 := time.Now()
		for i := len(undos) - 1; i >= 0; i-- {
			cleanupErr = errors.Join(cleanupErr, undos[i](cleanup))
		}
		after, e := c.restored(cleanup)
		r.RecoverySeconds = time.Since(t0).Seconds()
		r.After = &after
		if e = errors.Join(cleanupErr, e); e != nil {
			r.Verdict = "RECOVERY_FAILED"
			r.Error = errors.Join(errors.New(r.Error), e).Error()
			return
		}
		r.Restored = true
		if e = preserved(before, after); e != nil {
			r.Verdict = "FAIL"
			r.Error = e.Error()
			r.Restored = false
		}
	}()
	fail := func(e error) { r.Verdict = "FAIL"; r.Error = e.Error() }
	primary, replicas, e := before.roles()
	if e != nil {
		fail(e)
		return r
	}
	node := sc.Node
	if sc.Role == "primary" {
		node = primary
	}
	if sc.Role == "replica" {
		node = replicas[0]
	}
	if sc.Role == "replica2" {
		node = replicas[1]
	}
	r.Scenario.Node = node
	started := time.Now()
	defer func() { r.FaultSeconds = time.Since(started).Seconds() }()
	db, route := fmt.Sprintf("db%d", node), fmt.Sprintf("route%d", node)
	defer func() {
		if r.Verdict != "PASS" && r.Verdict != "EXPECTED_LIMITATION" {
			return
		}
		if sc.Kind == "lag" || sc.Kind == "paired" || sc.Kind == "dcs" && sc.Other != 0 || sc.Kind == "leaf" && node == 1 || sc.Kind == "pooler" && node == primary {
			return
		}
		if e := c.available(ctx, &r); e != nil {
			fail(e)
			return
		}
		p, e := commit(ctx)
		r.During["transaction"] = p
		if e != nil {
			fail(e)
		}
	}()
	switch sc.Kind {
	case "baseline":
		return r
	case "switch":
		undos = append(undos, func(ctx context.Context) error { return c.switchTo(ctx, primary) })
		if e = c.switchTo(ctx, node); e != nil {
			fail(e)
			return r
		}
		state, e := c.restored(ctx)
		if e != nil {
			fail(e)
			return r
		}
		p, _, e := state.roles()
		if e != nil || p != node {
			fail(errors.Join(e, errors.New("switchover target not sole primary")))
			return r
		}
		r.During["client"] = state.Client
	case "route", "paired":
		actor := sc.Actor
		if sc.Kind == "paired" {
			actor = "adapter"
			sc.Signal = "STOP"
		}
		pid, birth, e := routeIdentity(before, node, actor)
		if e != nil {
			fail(e)
			return r
		}
		if sc.Signal == "STOP" {
			undos = append(undos, func(ctx context.Context) error { return c.signal(ctx, route, pid, birth, "CONT") })
		}
		if sc.Kind == "paired" {
			g, start, e := routeIdentity(before, node, "guardian")
			if e != nil {
				fail(e)
				return r
			}
			undos = append(undos, func(ctx context.Context) error { return c.signal(ctx, route, g, start, "CONT") })
			if e = c.signal(ctx, route, g, start, "STOP"); e != nil {
				fail(e)
				return r
			}
		}
		var signalToken [16]byte
		if _, e = rand.Read(signalToken[:]); e != nil {
			fail(e)
			return r
		}
		signalNonce := fmt.Sprintf("%x", signalToken[:])
		_, injectionErr := c.execute(ctx, route, "python3", "/lab/fault.py", "signal-token", signalNonce, strconv.Itoa(pid), birth, sc.Signal)
		if injectionErr != nil && sc.Signal != "KILL" {
			fail(injectionErr)
			return r
		}
		if actor == "guardian" && sc.Signal == "STOP" || sc.Kind == "paired" {
			e = c.window(ctx, 15*time.Second, func() error {
				yes, p, e := c.origin(ctx, node)
				r.During["origin"] = p
				if e != nil {
					return e
				}
				if !yes {
					return errors.New("stalled-guardian boundary did not retain route")
				}
				return nil
			})
			if e == nil && sc.Kind == "paired" {
				r.Verdict = "EXPECTED_LIMITATION"
				r.Error = "Frozen guardian and adapter cannot bound stale speaker advertisements"
			}
		} else {
			// A brief withdrawal may finish between probes. Prove a new generation and fresh UUID instead.
			e = poll(ctx, 35*time.Second, func(ctx context.Context) (bool, error) {
				p, e := c.readProbe(ctx, route, "route")
				if e != nil {
					return false, e
				}
				r.During["route"] = p
				changed, err := restarted(before.Routes[strconv.Itoa(node)], p)
				if err != nil || !changed || injectionErr == nil {
					return changed, err
				}
				v, err := c.execute(ctx, route, "python3", "/lab/fault.py", "signal-status", signalNonce)
				if err != nil {
					return false, err
				}
				var status probe
				if err = json.Unmarshal([]byte(v.Stdout), &status); err != nil {
					return false, err
				}
				r.During["injector"] = status
				return quiescentSignal(status, pid, birth, sc.Signal)
			})
		}
		if e != nil {
			fail(e)
		} else if injectionErr != nil {
			// fail-silent-ok: no retry; exact injector is quiescent and a new guardian, journal generation and UUID are observed.
			c.mu.Lock()
			c.uncertain = false
			c.mu.Unlock()
			r.During["injector"]["lost_reply_diagnostic"] = injectionErr.Error()
		}
	case "pooler", "patroni":
		actor := "pgbouncer"
		if sc.Kind == "patroni" {
			actor = "patroni"
		}
		pid, birth, e := nodeIdentity(before, node, actor)
		if e != nil {
			fail(e)
			return r
		}
		undos = append(undos, func(ctx context.Context) error { return c.signal(ctx, db, pid, birth, "CONT") })
		if e = c.signal(ctx, db, pid, birth, "STOP"); e != nil {
			fail(e)
			return r
		}
		if e = c.observeOrigin(ctx, node, false, &r); e != nil {
			fail(e)
			return r
		}
		if sc.Kind == "patroni" && node == primary {
			if e = c.changedLeader(ctx, node, &r); e != nil {
				fail(e)
				return r
			}
			p, e := c.fencingObservation(ctx, node)
			if e != nil {
				fail(e)
				return r
			}
			r.During["old_primary"] = p
			state, e := p.text("state")
			if e != nil {
				fail(e)
				return r
			}
			if state == "primary" {
				r.Verdict = "EXPECTED_LIMITATION"
				r.Error = "watchdog=off: old primary still accepts SQL; no claim of two acknowledged writers"
			} else if state != "demoted" && state != "refused" {
				fail(errors.New("old primary fencing UNKNOWN"))
			}
		}
		if sc.Kind == "pooler" && node == primary {
			e = poll(ctx, 20*time.Second, func(ctx context.Context) (bool, error) {
				p, e := c.readProbe(ctx, "client", "client")
				r.During["client"] = p
				if e != nil {
					return false, e
				}
				w, e := p.boolean("checks", "write", "ok")
				if e != nil {
					return false, e
				}
				rd, e := p.boolean("checks", "read", "ok")
				if e != nil {
					return false, e
				}
				dns, e := p.boolean("checks", "dns", "ok")
				return !w && rd && !dns, e
			})
			if e != nil {
				fail(e)
			} else {
				r.Verdict = "EXPECTED_LIMITATION"
				r.Error = "Suspended primary PgBouncer withdraws the only write origin; database leadership alone cannot restore the pooler"
			}
		}
	case "pg-tree":
		var token [16]byte
		if _, e := rand.Read(token[:]); e != nil {
			fail(e)
			return r
		}
		pauseNonce := fmt.Sprintf("%x", token[:])
		undos = append(undos, func(ctx context.Context) error { _, e := c.fault(ctx, node, "pg-resume-token", pauseNonce); return e })
		_, e := c.fault(ctx, node, "pg-stop", pauseNonce)
		if e != nil {
			fail(e)
			return r
		}
		if e = c.observeOrigin(ctx, node, false, &r); e != nil {
			fail(e)
		}
	case "db-loss":
		undos = append(undos, func(ctx context.Context) error {
			_, a := c.podman(ctx, "start", c.name(db))
			_, b := c.podman(ctx, "start", c.name(route))
			return errors.Join(a, b)
		})
		if _, e = c.podman(ctx, "stop", "--time", "10", c.name(route)); e != nil {
			fail(e)
			return r
		}
		if _, e = c.podman(ctx, "kill", "--signal", "KILL", c.name(db)); e != nil {
			fail(e)
			return r
		}
		if node == primary {
			if e = c.changedLeader(ctx, node, &r); e != nil {
				fail(e)
			}
		} else {
			if e = c.observeOrigin(ctx, node, false, &r); e != nil {
				fail(e)
			}
		}
	case "dcs":
		nodes := []int{sc.Node}
		if sc.Other != 0 {
			nodes = append(nodes, sc.Other)
		}
		for _, n := range nodes {
			pid, birth, e := nodeIdentity(before, n, "etcd")
			if e != nil {
				fail(e)
				return r
			}
			service := fmt.Sprintf("db%d", n)
			undos = append(undos, func(ctx context.Context) error { return c.signal(ctx, service, pid, birth, "CONT") })
			if e = c.signal(ctx, service, pid, birth, "STOP"); e != nil {
				fail(e)
				return r
			}
		}
		if len(nodes) == 1 {
			e = c.window(ctx, 25*time.Second, func() error {
				p, e := c.readProbe(ctx, "client", "client")
				r.During["client"] = p
				if e != nil {
					return e
				}
				ok, e := p.boolean("ok")
				if e != nil {
					return e
				}
				if !ok {
					return errors.New("single-voter outage lost client availability")
				}
				return nil
			})
		} else {
			e = poll(ctx, 35*time.Second, func(ctx context.Context) (bool, error) {
				for n := 1; n <= 3; n++ {
					name := fmt.Sprintf("leaf%d", n)
					p, e := c.readProbe(ctx, name, "leaf")
					r.During[name] = p
					if e != nil {
						return false, e
					}
					yes, e := p.boolean("vips", "write", "installed")
					if e != nil || yes {
						return false, e
					}
				}
				return true, nil
			})
			if e == nil {
				e = c.fenced(ctx, primary, &r)
			}
		}
		if e != nil {
			fail(e)
		}
	case "leaf":
		service := fmt.Sprintf("leaf%d", node)
		undos = append(undos, func(ctx context.Context) error { _, e := c.podman(ctx, "start", c.name(service)); return e })
		if _, e = c.podman(ctx, "stop", "--time", "10", c.name(service)); e != nil {
			fail(e)
			return r
		}
		if node == primary {
			if e = c.changedLeader(ctx, node, &r); e != nil {
				fail(e)
				return r
			}
			if e = c.fenced(ctx, node, &r); e != nil {
				fail(e)
				return r
			}
		}
		p, e := c.readProbe(ctx, "client", "client")
		r.During["client"] = p
		if e != nil {
			fail(e)
			return r
		}
		ok, e := p.boolean("ok")
		if e != nil {
			fail(e)
			return r
		}
		if node == 1 {
			w, e1 := p.boolean("checks", "write", "ok")
			rd, e2 := p.boolean("checks", "read", "ok")
			dns, e3 := p.boolean("checks", "dns", "ok")
			if ok || e1 != nil || e2 != nil || e3 != nil || w || rd || dns {
				fail(errors.New("single-ingress leaf loss did not affect client"))
				return r
			}
			r.Verdict = "EXPECTED_LIMITATION"
			r.Error = "leaf1 is the only client ingress; surviving DB quorum is not client reachability"
		} else if !ok {
			e = poll(ctx, 40*time.Second, func(ctx context.Context) (bool, error) {
				p, e := c.readProbe(ctx, "client", "client")
				r.During["client"] = p
				if e != nil {
					return false, e
				}
				return p.boolean("ok")
			})
			if e != nil {
				fail(e)
			}
		}
	case "edge":
		service := fmt.Sprintf("leaf%d", node)
		base := []string{"vtysh", "-c", "configure terminal", "-c", fmt.Sprintf("router bgp %d", 64510+node), "-c"}
		peer := transit[sc.Other]
		undos = append(undos, func(ctx context.Context) error {
			_, e := c.execute(ctx, service, append(append([]string(nil), base...), "no neighbor "+peer+" shutdown")...)
			return e
		})
		if _, e = c.execute(ctx, service, append(base, "neighbor "+peer+" shutdown")...); e != nil {
			fail(e)
			return r
		}
		e = poll(ctx, 20*time.Second, func(ctx context.Context) (bool, error) {
			l, e := c.readProbe(ctx, service, "leaf")
			if e != nil {
				return false, e
			}
			peers, e := l.list("peers")
			if e != nil {
				return false, e
			}
			down := false
			for _, v := range peers {
				p, e := object(v)
				if e != nil {
					return false, e
				}
				address, e := p.text("address")
				if e != nil {
					return false, e
				}
				if address == peer {
					state, e := p.text("state")
					if e != nil {
						return false, e
					}
					down = state != "Established"
				}
			}
			if !down {
				return false, nil
			}
			r.During["edge"] = l
			p, e := c.readProbe(ctx, "client", "client")
			r.During["client"] = p
			if e != nil {
				return false, e
			}
			return p.boolean("ok")
		})
		if e != nil {
			fail(e)
		}
	case "dcs-client":
		for _, device := range []string{"eth0", "lo"} {
			v, e := c.execute(ctx, db, "tc", "-j", "qdisc", "show", "dev", device)
			if e != nil {
				fail(e)
				return r
			}
			var q []map[string]any
			if e = json.Unmarshal([]byte(v.Stdout), &q); e != nil {
				fail(e)
				return r
			}
			for _, item := range q {
				if item["kind"] == "clsact" {
					fail(errors.New("pre-existing clsact; refuse to alter foreign filters"))
					return r
				}
			}
			dev := device
			undos = append(undos, func(ctx context.Context) error {
				v, e := c.execute(ctx, db, "tc", "-j", "qdisc", "show", "dev", dev)
				if e != nil {
					return e
				}
				var q []map[string]any
				if e = json.Unmarshal([]byte(v.Stdout), &q); e != nil {
					return e
				}
				for _, item := range q {
					if item["kind"] == "clsact" {
						_, e = c.execute(ctx, db, "tc", "qdisc", "del", "dev", dev, "clsact")
						return e
					}
				}
				return nil
			})
			if _, e = c.execute(ctx, db, "tc", "qdisc", "add", "dev", device, "clsact"); e != nil {
				fail(e)
				return r
			}
			if _, e = c.execute(ctx, db, "tc", "filter", "add", "dev", device, "egress", "protocol", "ip", "pref", "49150", "flower", "ip_proto", "tcp", "dst_port", "2379", "action", "drop"); e != nil {
				fail(e)
				return r
			}
		}
		if e = c.changedLeader(ctx, node, &r); e != nil {
			fail(e)
		} else if e = c.fenced(ctx, node, &r); e != nil {
			fail(e)
		}
	case "lag":
		pid, birth, e := nodeIdentity(before, node, "startup")
		if e != nil {
			fail(e)
			return r
		}
		var token [16]byte
		if _, e = rand.Read(token[:]); e != nil {
			fail(e)
			return r
		}
		nonce := fmt.Sprintf("%x", token[:])
		waitInjector := func(ctx context.Context) error {
			return poll(ctx, 15*time.Second, func(ctx context.Context) (bool, error) {
				v, e := c.fault(ctx, node, "signal-status", nonce)
				if e != nil {
					return false, e
				}
				var p probe
				if e = json.Unmarshal([]byte(v.Stdout), &p); e != nil {
					return false, e
				}
				r.During["injector"] = p
				return quiescentSignal(p, pid, birth, "STOP")
			})
		}
		undos = append(undos, func(ctx context.Context) error {
			if e := waitInjector(ctx); e != nil {
				return e
			}
			return c.signal(ctx, db, pid, birth, "CONT")
		})
		if _, e = c.fault(ctx, node, "signal-token", nonce, strconv.Itoa(pid), birth, "STOP"); e != nil {
			fail(e)
			return r
		}
		if e = waitInjector(ctx); e != nil {
			fail(e)
			return r
		}
		checkStopped := func(ctx context.Context) error {
			p, e := c.readProbe(ctx, db, "startup")
			r.During["startup"] = p
			if e != nil {
				return e
			}
			ok, e := startupStopped(p, before.Nodes[strconv.Itoa(node)])
			if e != nil || !ok {
				return errors.Join(e, errors.New("startup STOP not confirmed"))
			}
			return nil
		}
		if e = checkStopped(ctx); e != nil {
			fail(e)
			return r
		}
		frozen, e := c.readProbe(ctx, fmt.Sprintf("db%d", primary), "node")
		r.During["lag_frozen_primary"] = frozen
		if e != nil {
			fail(e)
			return r
		}
		if _, _, e = sameReplication(before.Nodes[strconv.Itoa(primary)], frozen, node); e != nil {
			fail(e)
			return r
		}
		for n := 0; n < 2; n++ {
			if _, e = c.fault(ctx, primary, "wal"); e != nil {
				fail(e)
				return r
			}
		}
		if e = poll(ctx, 20*time.Second, func(ctx context.Context) (bool, error) {
			p, e := c.readProbe(ctx, fmt.Sprintf("db%d", primary), "node")
			if e != nil {
				return false, e
			}
			r.During["lag_proof"] = p
			return lagConfirmed(frozen, p, node)
		}); e != nil {
			fail(e)
			return r
		}
		if e = c.observeOrigin(ctx, node, false, &r); e != nil {
			fail(e)
			return r
		}
		if e = c.available(ctx, &r); e != nil {
			fail(e)
			return r
		}
		p, e := commit(ctx)
		r.During["transaction"] = p
		if e != nil {
			fail(e)
			return r
		}
		e = poll(ctx, 20*time.Second, func(ctx context.Context) (bool, error) {
			p, e := c.readProbe(ctx, fmt.Sprintf("db%d", primary), "node")
			r.During["lag_after_ack"] = p
			if e != nil {
				return false, e
			}
			return lagConfirmed(frozen, p, node)
		})
		if e = errors.Join(e, checkStopped(ctx)); e != nil {
			fail(e)
		}
	default:
		fail(errors.New("unknown modeled scenario"))
	}
	return r
}

func activeUUID(p probe) (string, error) {
	active, e := p.text("active_generation")
	if e != nil {
		return "", e
	}
	rows, e := p.list("generations")
	if e != nil {
		return "", e
	}
	for _, v := range rows {
		g, e := object(v)
		if e != nil {
			return "", e
		}
		name, e := g.text("name")
		if e != nil {
			return "", e
		}
		if name != active {
			continue
		}
		ids, e := g.list("uuid_sha256s")
		if e != nil {
			return "", e
		}
		found := ""
		for _, id := range ids {
			if id == nil {
				continue
			}
			s, ok := id.(string)
			if !ok || s == "" || found != "" {
				return "", errors.New("invalid active UUID set")
			}
			found = s
		}
		return found, nil
	}
	return "", errors.New("active generation missing")
}

func quiescentSignal(p probe, pid int, birth, name string) (bool, error) {
	present, e := p.boolean("present")
	if e != nil || !present {
		return false, e
	}
	gotPID, e := p.number("target", "pid")
	if e != nil {
		return false, e
	}
	gotBirth, e := p.text("target", "start_time")
	if e != nil {
		return false, e
	}
	gotSignal, e := p.text("target", "signal")
	if e != nil {
		return false, e
	}
	if gotPID != pid || gotBirth != birth || gotSignal != "SIG"+name {
		return false, errors.New("signal receipt request mismatch")
	}
	return p.boolean("injector_quiescent")
}

func restarted(before, after probe) (bool, error) {
	paths := [][]string{{"active_generation"}, {"processes", "guardian", "start_time"}}
	for _, path := range paths {
		a, e := before.text(path...)
		if e != nil {
			return false, e
		}
		b, e := after.text(path...)
		if e != nil {
			return false, e
		}
		if a == "" || b == "" || a == b {
			return false, nil
		}
	}
	a, e := activeUUID(before)
	if e != nil {
		return false, e
	}
	b, e := activeUUID(after)
	if e != nil {
		return false, e
	}
	return a != "" && b != "" && a != b, nil
}

func preserved(before, after snapshot) error {
	for node, route := range before.Routes {
		active, e := route.text("active_generation")
		if e != nil {
			return e
		}
		prior, e := route.list("generations")
		if e != nil {
			return e
		}
		next, e := after.Routes[node].list("generations")
		if e != nil {
			return e
		}
		byName := map[string]probe{}
		for _, v := range next {
			p, e := object(v)
			if e != nil {
				return e
			}
			name, e := p.text("name")
			if e != nil {
				return e
			}
			byName[name] = p
		}
		for _, v := range prior {
			p, e := object(v)
			if e != nil {
				return e
			}
			name, e := p.text("name")
			if e != nil {
				return e
			}
			n, ok := byName[name]
			if !ok {
				return errors.New("retired journal directory disappeared")
			}
			if name == active {
				continue
			}
			a, e := json.Marshal(p)
			if e != nil {
				return e
			}
			b, e := json.Marshal(n)
			if e != nil {
				return e
			}
			if string(a) != string(b) {
				return errors.New("retired journal changed")
			}
		}
	}
	return nil
}
