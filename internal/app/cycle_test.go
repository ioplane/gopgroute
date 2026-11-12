//go:build linux

package app

import (
	"testing"
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/ioplane/gopgroute/internal/pooler"
)

func TestSQLRoleBindingExhaustive(t *testing.T) {
	now := time.Now()
	count := 0
	for _, p := range []health.State{health.Unknown, health.Unhealthy, health.Healthy} {
		for _, r := range []health.State{health.Unknown, health.Unhealthy, health.Healthy} {
			for _, b := range []health.State{health.Unknown, health.Unhealthy, health.Healthy} {
				for _, role := range []health.Role{health.Primary, health.Replica, "invalid"} {
					for _, age := range []time.Duration{0, time.Second, -time.Second} {
						primary := health.Check{State: p, StartedAt: now}
						replica := health.Check{State: r, StartedAt: now}
						sql := pooler.Observation{Role: role, Check: health.Check{State: b, StartedAt: now.Add(-age)}}
						got := health.Decide(now, time.Second, sqlSnapshot(now, time.Second, primary, replica, sql))
						write := p == health.Healthy && r != health.Healthy && b == health.Healthy && role == health.Primary && age == 0
						read := r == health.Healthy && p != health.Healthy && b == health.Healthy && role == health.Replica && age == 0
						if got.Write != write || got.Read != read {
							t.Fatalf("P=%v R=%v SQL=%v role=%v age=%v: got %+v want write=%v read=%v", p, r, b, role, age, got, write, read)
						}
						count++
					}
				}
			}
		}
	}
	t.Logf("verified %d combinations", count)
}
