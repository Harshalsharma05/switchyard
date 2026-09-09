// System info for GET /admin/system (Part 2, Step 6.4).
//
// Like reload.go, this is wiring: it reaches across the redis client, the pgx
// pool, and the summary service to answer "is this dependency up right now" —
// collaborators only main.go holds. admin.SystemReporter is the narrow view the
// handler needs.
package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/admin"
	"github.com/Harshalsharma05/switchyard/internal/summary"
)

// probeTimeout bounds each dependency check. Short on purpose: the System panel
// polls, and a probe that hangs would make the whole panel look stuck. A
// dependency that cannot answer in this long reads as "down", which for an
// operations view is the right call.
const probeTimeout = 500 * time.Millisecond

type systemInfo struct {
	version    string
	startedAt  time.Time
	configHash interface{ ConfigHash() string } // the configStore
	redis      *redis.Client
	db         *pgxpool.Pool
	teams      interface{ Degraded() (bool, time.Time) } // the team store
	summary    *summary.Service
}

var _ admin.SystemReporter = (*systemInfo)(nil)

func (s *systemInfo) Version() string       { return s.version }
func (s *systemInfo) Uptime() time.Duration { return time.Since(s.startedAt) }
func (s *systemInfo) ConfigHash() string    { return s.configHash.ConfigHash() }

func (s *systemInfo) Dependencies(ctx context.Context) map[string]string {
	return map[string]string{
		"redis":      s.probeRedis(ctx),
		"postgres":   s.probePostgres(ctx),
		"prometheus": s.probePrometheus(ctx),
		"team_store": s.probeTeams(),
	}
}

func (s *systemInfo) probeRedis(ctx context.Context) string {
	if s.redis == nil {
		return "not_configured"
	}
	c, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := s.redis.Ping(c).Err(); err != nil {
		return "down"
	}
	return "up"
}

// probeTeams reports the team snapshot rather than a connection: "degraded"
// means auth is still working, from a snapshot the store could not refresh.
// That is a distinct state from Postgres being down — the database can come
// back before the next refresh, and requests never stopped either way.
func (s *systemInfo) probeTeams() string {
	if s.teams == nil {
		return "not_configured"
	}
	if degraded, _ := s.teams.Degraded(); degraded {
		return "degraded"
	}
	return "up"
}

func (s *systemInfo) probePostgres(ctx context.Context) string {
	if s.db == nil {
		return "not_configured"
	}
	c, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := s.db.Ping(c); err != nil {
		return "down"
	}
	return "up"
}

func (s *systemInfo) probePrometheus(ctx context.Context) string {
	c, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	up, configured := s.summary.Reachable(c)
	switch {
	case !configured:
		return "not_configured"
	case up:
		return "up"
	default:
		return "down"
	}
}
