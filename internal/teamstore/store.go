// The Postgres-backed team store and the snapshot cache that makes it viable
// on the request path (Tier 1, Step 2.3).
package teamstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// DefaultRefreshInterval is also the revocation window on any replica that did
// not make the change: a key revoked elsewhere keeps authenticating here until
// the next successful refresh. Stated plainly because there is no free answer —
// see DECISIONS.md.
const DefaultRefreshInterval = 30 * time.Second

// Store resolves API keys against Postgres without touching Postgres on the
// request path.
//
// Every team is held in an auth.Registry rebuilt wholesale on a ticker and
// swapped in atomically — the same copy-on-write discipline Registry already
// uses internally, one level up. That choice is what keeps Authenticate free of
// a context parameter and of I/O: proxy's auth middleware cannot tell that team
// storage moved, which was the requirement.
//
// Mutations are write-through: Postgres first, then the live snapshot, so an
// admin's change is durable before it is visible and is visible on the very
// next request rather than at the next refresh.
type Store struct {
	pool     *pgxpool.Pool
	interval time.Duration
	metrics  *telemetry.Metrics
	log      *slog.Logger

	current atomic.Pointer[snapshot]
}

// snapshot is one generation of the team table. degraded marks a generation the
// store failed to refresh — it is still served, deliberately.
type snapshot struct {
	registry *auth.Registry
	loadedAt time.Time
	degraded bool
}

// New loads the first snapshot and fails if it cannot.
//
// Deliberately fatal: an empty snapshot would reject every key, and there is no
// cached state yet to fall back on. Serving stale data is only defensible once
// there is data to be stale.
func New(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, m *telemetry.Metrics, log *slog.Logger) (*Store, error) {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	s := &Store{pool: pool, interval: interval, metrics: m, log: log}

	reg, err := s.loadRegistry(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading initial team snapshot: %w", err)
	}
	s.store(&snapshot{registry: reg, loadedAt: time.Now()}, "ok")
	return s, nil
}

// Run refreshes the snapshot until ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	telemetry.Supervise(ctx, s.log, s.metrics, "team-snapshot", func() { s.loop(ctx) })
}

func (s *Store) loop(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// The error is already logged and counted; a failed refresh is a
			// degraded state, not a reason to stop refreshing.
			_ = s.Refresh(ctx)
		}
	}
}

// Refresh rebuilds the snapshot from Postgres.
//
// On failure the existing snapshot is kept and marked degraded: a key already
// in it keeps working, a key that is not is still refused. This is the one
// place the gateway serves stale data on purpose — the alternative is a total
// authentication outage every time the database hiccups.
func (s *Store) Refresh(ctx context.Context) error {
	reg, err := s.loadRegistry(ctx)
	if err != nil {
		s.markDegraded(err)
		return err
	}
	s.store(&snapshot{registry: reg, loadedAt: time.Now()}, "ok")
	return nil
}

func (s *Store) store(next *snapshot, result string) {
	s.current.Store(next)
	if s.metrics == nil {
		return
	}
	s.metrics.TeamSnapshotRefreshTotal.WithLabelValues(result).Inc()
	s.metrics.TeamSnapshotTimestamp.Set(float64(next.loadedAt.Unix()))
	s.metrics.TeamStoreDegraded.Set(0)
}

func (s *Store) markDegraded(cause error) {
	prev := s.current.Load()
	s.current.Store(&snapshot{registry: prev.registry, loadedAt: prev.loadedAt, degraded: true})

	s.log.Error("team snapshot refresh failed, serving stale teams",
		slog.Time("snapshot_loaded_at", prev.loadedAt),
		slog.Duration("snapshot_age", time.Since(prev.loadedAt)),
		slog.Any("error", cause),
	)
	if s.metrics != nil {
		s.metrics.TeamSnapshotRefreshTotal.WithLabelValues("error").Inc()
		s.metrics.TeamStoreDegraded.Set(1)
	}
}

// Authenticate resolves a plaintext key against the current snapshot.
//
// Never fails open. An unknown key is rejected identically whether Postgres is
// healthy, slow, or unreachable — the snapshot is the whole answer, so there is
// no path here that consults anything else or that could time out into an
// allow.
func (s *Store) Authenticate(rawKey string) (*auth.Team, error) {
	team, err := s.current.Load().registry.Authenticate(rawKey)
	if s.metrics != nil {
		if err != nil {
			s.metrics.TeamLookupsTotal.WithLabelValues("unknown").Inc()
		} else {
			s.metrics.TeamLookupsTotal.WithLabelValues("hit").Inc()
		}
	}
	return team, err
}

func (s *Store) List() []auth.Team { return s.current.Load().registry.List() }

func (s *Store) Get(id string) (auth.Team, error) { return s.current.Load().registry.Get(id) }

// Degraded reports whether the current snapshot failed its last refresh, and
// when it was loaded. The System panel and /readyz read this.
func (s *Store) Degraded() (bool, time.Time) {
	snap := s.current.Load()
	return snap.degraded, snap.loadedAt
}

// Update persists a limit or budget change, then applies it to the snapshot.
//
// The merge is validated before anything is written, so an invalid patch is
// rejected without a database round trip and with the same error text the
// in-memory registry produced before teams moved to Postgres.
func (s *Store) Update(ctx context.Context, id string, patch auth.TeamPatch) (auth.Team, error) {
	current, err := s.Get(id)
	if err != nil {
		return auth.Team{}, err
	}
	updated, err := patch.Apply(current)
	if err != nil {
		return auth.Team{}, err
	}

	if err := s.exec(ctx, id,
		`UPDATE teams SET rpm = $2, tpm = $3, monthly_budget_micros = $4, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		id, updated.RateLimits.RPM, updated.RateLimits.TPM, updated.MonthlyBudgetMicros,
	); err != nil {
		return auth.Team{}, err
	}

	return s.writeThrough(id, func(r *auth.Registry) (auth.Team, error) { return r.Update(id, patch) })
}

// RotateKey persists a new key hash, then swaps it into the snapshot.
//
// Postgres first, on purpose: a key that authenticates but was never written
// would silently stop working at the next refresh, which is the exact class of
// bug this phase exists to remove.
func (s *Store) RotateKey(ctx context.Context, id, newHash, newMasked string) (auth.Team, error) {
	if newHash == "" {
		return auth.Team{}, fmt.Errorf("rotate %s: new key hash is empty", id)
	}

	if err := s.exec(ctx, id,
		`UPDATE teams SET key_hash = $2, key_source = $3, key_masked = $4,
		        key_created_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		id, newHash, auth.KeySourceRotated, newMasked,
	); err != nil {
		return auth.Team{}, err
	}

	return s.writeThrough(id, func(r *auth.Registry) (auth.Team, error) {
		return r.RotateKey(id, newHash, newMasked)
	})
}

// RevokeKey clears a team's key. The team stays; nothing authenticates as it
// until it is rotated a new one.
func (s *Store) RevokeKey(ctx context.Context, id string) (auth.Team, error) {
	if err := s.exec(ctx, id,
		`UPDATE teams SET key_hash = NULL, key_source = $2, key_masked = '',
		        key_created_at = NULL, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		id, auth.KeySourceRevoked,
	); err != nil {
		return auth.Team{}, err
	}

	return s.writeThrough(id, func(r *auth.Registry) (auth.Team, error) { return r.RevokeKey(id) })
}

// Postgres error codes Create branches on. Named rather than pulled from a
// dependency — two constants do not justify a new module.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

// Create persists a new team and indexes it, so its key authenticates on the
// very next request (Tier 1, Step 2.5).
//
// An ID already in the table — including a soft-deleted team's — is
// auth.ErrTeamExists. IDs are never reused, so a new team cannot inherit a
// deleted one's request history, or its Redis spend and rate-limit state.
func (s *Store) Create(ctx context.Context, t auth.Team) (auth.Team, error) {
	if err := t.Validate(); err != nil {
		return auth.Team{}, err
	}
	if t.OrganizationID == "" {
		t.OrganizationID = DefaultOrgID
	}
	// The caller that minted the key normally sets these; defaulting them keeps
	// a bare Create from tripping key_source's CHECK constraint.
	if t.KeySource == "" {
		t.KeySource = auth.KeySourceCreated
	}
	if t.KeyCreatedAt == nil {
		now := time.Now().UTC()
		t.KeyCreatedAt = &now
	}

	if err := insertTeam(ctx, s.pool, t); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == uniqueViolation && pgErr.ConstraintName == "teams_pkey":
				return auth.Team{}, auth.ErrTeamExists
			case pgErr.Code == foreignKeyViolation:
				return auth.Team{}, auth.ErrUnknownOrganization
			}
		}
		return auth.Team{}, fmt.Errorf("creating team %q: %w", t.ID, err)
	}

	return s.writeThrough(t.ID, func(r *auth.Registry) (auth.Team, error) {
		err := r.Add(t)
		if errors.Is(err, auth.ErrTeamExists) {
			// A refresh landed between the insert and here and already loaded
			// the team. Not a failure — the snapshot has it either way.
			return r.Get(t.ID)
		}
		return t, err
	})
}

// Delete soft-deletes a team. The row stays, so request-log and audit rows keep a
// real team to point at; deleted_at hides it from every snapshot; and the key is
// cleared in the same statement, so a deleted team can never authenticate even
// if something forgot to filter on deleted_at. The key stops working on the next
// request here, and at the next refresh on any other replica.
func (s *Store) Delete(ctx context.Context, id string) (auth.Team, error) {
	if err := s.exec(ctx, id,
		`UPDATE teams SET deleted_at = now(), key_hash = NULL, key_source = $2, key_masked = '',
		        key_created_at = NULL, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		id, auth.KeySourceRevoked,
	); err != nil {
		return auth.Team{}, err
	}

	return s.writeThrough(id, func(r *auth.Registry) (auth.Team, error) {
		team, err := r.Remove(id)
		if errors.Is(err, auth.ErrUnknownTeam) {
			// A refresh already dropped it. Gone is gone.
			return auth.Team{ID: id}, nil
		}
		return team, err
	})
}

// exec runs one team mutation and turns "matched no row" into ErrUnknownTeam,
// so a caller branches on identity rather than on an affected-row count.
func (s *Store) exec(ctx context.Context, id, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("updating team %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrUnknownTeam
	}
	return nil
}

// writeThrough applies an already-persisted change to the live snapshot.
//
// A failure here cannot lose the change — Postgres already has it — so it is
// logged rather than returned, and the next refresh reconciles. The team is
// re-read from the registry on success so the caller sees the same value a
// subsequent Get would.
func (s *Store) writeThrough(id string, apply func(*auth.Registry) (auth.Team, error)) (auth.Team, error) {
	team, err := apply(s.current.Load().registry)
	if err != nil {
		s.log.Error("team change persisted but not applied to the snapshot; it will appear at the next refresh",
			slog.String("team", id),
			slog.Duration("within", s.interval),
			slog.Any("error", err),
		)
		return s.Get(id)
	}
	return team, nil
}

const selectTeamsSQL = `
	SELECT id, organization_id, name, priority, rpm, tpm, monthly_budget_micros,
	       allowed_providers, allowed_models, is_admin,
	       key_hash, key_source, key_masked, key_created_at
	FROM teams
	WHERE deleted_at IS NULL
	ORDER BY id`

func (s *Store) loadRegistry(ctx context.Context) (*auth.Registry, error) {
	rows, err := s.pool.Query(ctx, selectTeamsSQL)
	if err != nil {
		return nil, fmt.Errorf("querying teams: %w", err)
	}
	defer rows.Close()

	var teams []auth.Team
	for rows.Next() {
		var t auth.Team
		var priority string
		var keyHash *string
		if err := rows.Scan(
			&t.ID, &t.OrganizationID, &t.Name, &priority, &t.RateLimits.RPM, &t.RateLimits.TPM,
			&t.MonthlyBudgetMicros, &t.AllowedProviders, &t.AllowedModels,
			&t.IsAdmin, &keyHash, &t.KeySource, &t.KeyMasked, &t.KeyCreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning team: %w", err)
		}
		t.Priority = auth.Priority(priority)
		if keyHash != nil {
			t.KeyHash = *keyHash
		}
		teams = append(teams, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading teams: %w", err)
	}
	if len(teams) == 0 {
		// Not an error the caller can act on differently, but worth naming: an
		// empty table rejects every key, and the cause is almost always that
		// cmd/migrate's seed never ran.
		return nil, errors.New("no teams in the database: has cmd/migrate run?")
	}

	reg, err := auth.NewRegistry(teams)
	if err != nil {
		return nil, fmt.Errorf("indexing teams: %w", err)
	}
	return reg, nil
}
