// Package health evaluates bounded service observations without changing routes.
package health

import "time"

// State distinguishes missing evidence from an explicit unhealthy result.
type State uint8

const (
	Unknown State = iota
	Unhealthy
	Healthy
)

// Role selects a Patroni endpoint with database-role semantics.
type Role string

const (
	Primary Role = "primary"
	Replica Role = "replica"
)

// Check retains request start time so slow responses consume their freshness budget.
type Check struct {
	State     State
	StartedAt time.Time
}

// Snapshot combines independent Patroni and SQL pooler observations.
type Snapshot struct {
	Primary Check
	Replica Check
	Pooler  Check
}

// Intent is eligibility, not evidence of an announcement or database fencing.
type Intent struct {
	Write bool
	Read  bool
}

// Decide fails closed on expired evidence and contradictory positive role checks.
func Decide(now time.Time, maxAge time.Duration, s Snapshot) Intent {
	if maxAge <= 0 || !fresh(now, maxAge, s.Pooler) {
		return Intent{}
	}
	primary, replica := fresh(now, maxAge, s.Primary), fresh(now, maxAge, s.Replica)
	return Intent{Write: primary && !replica, Read: replica && !primary}
}

func fresh(now time.Time, maxAge time.Duration, c Check) bool {
	return c.State == Healthy && !c.StartedAt.IsZero() && !now.Before(c.StartedAt) && now.Sub(c.StartedAt) < maxAge
}
