package health

import (
	"testing"
	"time"
)

func TestDecideRoleMatrix(t *testing.T) {
	t.Parallel()
	now := time.Now()
	roles := []struct {
		name             string
		primary, replica State
		want             Intent
	}{
		{"unknown_unknown", Unknown, Unknown, Intent{}},
		{"unknown_unhealthy", Unknown, Unhealthy, Intent{}},
		{"unknown_replica", Unknown, Healthy, Intent{Read: true}},
		{"unhealthy_unknown", Unhealthy, Unknown, Intent{}},
		{"unhealthy_unhealthy", Unhealthy, Unhealthy, Intent{}},
		{"unhealthy_replica", Unhealthy, Healthy, Intent{Read: true}},
		{"primary_unknown", Healthy, Unknown, Intent{Write: true}},
		{"primary_unhealthy", Healthy, Unhealthy, Intent{Write: true}},
		{"contradictory_roles", Healthy, Healthy, Intent{}},
	}
	for _, r := range roles {
		t.Run(r.name, func(t *testing.T) {
			for _, p := range []State{Unknown, Unhealthy, Healthy} {
				snapshot := Snapshot{Primary: Check{State: r.primary, StartedAt: now}, Replica: Check{State: r.replica, StartedAt: now}, Pooler: Check{State: p, StartedAt: now}}
				want := Intent{}
				if p == Healthy {
					want = r.want
				}
				if got := Decide(now, time.Second, snapshot); got != want {
					t.Errorf("pooler=%d: got %+v, want %+v", p, got, want)
				}
			}
		})
	}
}

func TestDecideFreshness(t *testing.T) {
	t.Parallel()
	now := time.Now()
	good := Check{State: Healthy, StartedAt: now}
	cases := []struct {
		name            string
		age             time.Duration
		primary, pooler Check
		want            Intent
	}{
		{"fresh", time.Second, good, good, Intent{Write: true}},
		{"just_before_expiry", time.Second, Check{State: Healthy, StartedAt: now.Add(-time.Second + time.Nanosecond)}, good, Intent{Write: true}},
		{"expired_primary", time.Second, Check{State: Healthy, StartedAt: now.Add(-time.Second)}, good, Intent{}},
		{"expired_pooler", time.Second, good, Check{State: Healthy, StartedAt: now.Add(-time.Second)}, Intent{}},
		{"zero_timestamp", time.Second, Check{State: Healthy}, good, Intent{}},
		{"future_timestamp", time.Second, Check{State: Healthy, StartedAt: now.Add(time.Nanosecond)}, good, Intent{}},
		{"zero_age", 0, good, good, Intent{}},
		{"negative_age", -time.Second, good, good, Intent{}},
		{"invalid_state", time.Second, Check{State: State(255), StartedAt: now}, good, Intent{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(now, c.age, Snapshot{Primary: c.primary, Pooler: c.pooler})
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
