package lab

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func walPosition(value string) (uint64, error) {
	high, low, ok := strings.Cut(value, "/")
	if !ok || high == "" || low == "" || len(high) > 8 || len(low) > 8 {
		return 0, errors.New("invalid WAL position")
	}
	a, e := strconv.ParseUint(high, 16, 32)
	if e != nil {
		return 0, fmt.Errorf("WAL high word: %w", e)
	}
	b, e := strconv.ParseUint(low, 16, 32)
	if e != nil {
		return 0, fmt.Errorf("WAL low word: %w", e)
	}
	return a<<32 | b, nil
}

func replicationRow(p probe, node int) (probe, error) {
	rows, e := p.list("postgres", "replication")
	if e != nil {
		return nil, e
	}
	var target probe
	for _, value := range rows {
		row, e := object(value)
		if e != nil {
			return nil, e
		}
		name, e := row.text("application_name")
		if e != nil {
			return nil, e
		}
		if name == fmt.Sprintf("db%d", node) {
			if target != nil {
				return nil, errors.New("duplicate replication identity")
			}
			target = row
		}
	}
	if target == nil {
		return nil, errors.New("missing lagged replica")
	}
	return target, nil
}

func sameReplication(before, after probe, node int) (probe, probe, error) {
	for _, key := range []string{"postmaster_started", "synchronous_commit"} {
		a, e := before.text("postgres", key)
		b, other := after.text("postgres", key)
		if e != nil || other != nil || a != b || key == "synchronous_commit" && a != "on" {
			return nil, nil, errors.Join(e, other, errors.New("primary identity or synchronous commit changed"))
		}
	}
	for _, p := range []probe{before, after} {
		recovery, e := p.boolean("postgres", "recovery")
		if e != nil || recovery {
			return nil, nil, errors.Join(e, errors.New("lag observation is not from primary"))
		}
	}
	a, e := replicationRow(before, node)
	b, other := replicationRow(after, node)
	if e != nil || other != nil {
		return nil, nil, errors.Join(e, other)
	}
	pid, e := a.number("pid")
	current, other := b.number("pid")
	if e != nil || other != nil || pid <= 1 || pid != current {
		return nil, nil, errors.Join(e, other, errors.New("replication connection changed"))
	}
	state, e := a.text("sync_state")
	latest, other := b.text("sync_state")
	if e != nil || other != nil || state != latest || state != "sync" && state != "quorum" && state != "async" && state != "potential" {
		return nil, nil, errors.Join(e, other, errors.New("synchronous membership changed"))
	}
	for _, row := range []probe{a, b} {
		state, e := row.text("state")
		if e != nil || state != "streaming" {
			return nil, nil, errors.Join(e, errors.New("replication stream not active"))
		}
	}
	return a, b, nil
}

func lagConfirmed(frozen, observed probe, node int) (bool, error) {
	a, b, e := sameReplication(frozen, observed, node)
	if e != nil {
		return false, e
	}
	var positions [2][3]uint64
	for index, row := range []probe{a, b} {
		for kind, key := range []string{"write_lsn", "flush_lsn", "replay_lsn"} {
			value, e := row.text(key)
			if e != nil {
				return false, e
			}
			position, e := walPosition(value)
			if e != nil {
				return false, e
			}
			positions[index][kind] = position
		}
	}
	lag, e := b.number("replay_lag_bytes")
	flushLag, other := b.number("flush_lag_bytes")
	if e != nil || other != nil {
		return false, errors.Join(e, other)
	}
	write, flush, replay := positions[1][0], positions[1][1], positions[1][2]
	return write >= flush && flush >= replay && flush-replay > 1048576 && lag > 1048576 && flushLag == 0 &&
		replay == positions[0][2] && write > positions[0][0] && flush > positions[0][1], nil
}

func startupStopped(p, before probe) (bool, error) {
	for _, pair := range [][2]string{{"pid", "startup"}, {"postmaster_pid", "postgres"}} {
		actual, e := p.number(pair[0])
		wanted, other := before.number("process_ids", pair[1])
		if e != nil || other != nil || actual != wanted || actual <= 1 {
			return false, errors.Join(e, other, errors.New("startup process identity changed"))
		}
	}
	for _, pair := range [][2]string{{"start_time", "startup"}, {"postmaster_start_time", "postgres"}} {
		actual, e := p.text(pair[0])
		wanted, other := before.text("process_start_ids", pair[1])
		if e != nil || other != nil || actual != wanted || actual == "" {
			return false, errors.Join(e, other, errors.New("startup process incarnation changed"))
		}
	}
	namespace, e := p.text("pid_namespace")
	wanted, other := before.text("pid_namespace")
	if e != nil || other != nil || namespace != wanted || namespace == "" {
		return false, errors.Join(e, other, errors.New("startup PID namespace changed"))
	}
	state, e := p.text("state")
	return state == "T", e
}
