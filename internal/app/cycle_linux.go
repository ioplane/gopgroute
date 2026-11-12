//go:build linux

package app

import (
	"time"

	"github.com/ioplane/gopgroute/internal/health"
	"github.com/ioplane/gopgroute/internal/pooler"
)

func sqlSnapshot(now time.Time, maxAge time.Duration, primary, replica health.Check, sql pooler.Observation) health.Snapshot {
	s := health.Snapshot{Primary: primary, Replica: replica, Pooler: sql.Check}
	intent := health.Decide(now, maxAge, s)
	if (sql.Role == health.Primary && intent.Write) || (sql.Role == health.Replica && intent.Read) {
		return s
	}
	s.Pooler = health.Check{}
	return s
}
