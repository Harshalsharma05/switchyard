// Retention: roll expired request-log rows into the daily summary, then drop
// them. Bounded batches on a ticker, never one unbounded DELETE.
package logstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// retentionLockKey guards a sweep so two replicas cannot both roll the same
// rows up. Distinct from the migration lock; the value is arbitrary but must
// stay stable.
const retentionLockKey int64 = 0x5359_4152_4401 // "SYARD\1"

// sweepSQL rolls one batch of expired rows into requests_daily and deletes the
// same rows in a single statement.
//
// Both data-modifying CTEs read the one `doomed` snapshot and Postgres runs
// each exactly once, so a row is summed and removed atomically. That is what
// makes an interrupted sweep safe to resume: a row is either still detail, or
// counted once in the summary, never both and never neither.
const sweepSQL = `
WITH doomed AS (
    SELECT id, ts, team_id, provider, served_model, status_code,
           input_tokens, output_tokens, cost_micros, fallback_cost_delta_micros
    FROM requests
    WHERE ts < $1
    ORDER BY ts
    LIMIT $2
),
rolled AS (
    INSERT INTO requests_daily AS d (
        day, team_id, provider, served_model,
        requests, errors, input_tokens, output_tokens, cost_micros,
        fallback_cost_delta_micros
    )
    SELECT (ts AT TIME ZONE 'UTC')::date, team_id,
           COALESCE(provider, ''), COALESCE(served_model, ''),
           count(*), count(*) FILTER (WHERE status_code >= 400),
           COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0),
           COALESCE(sum(cost_micros), 0),
           COALESCE(sum(fallback_cost_delta_micros), 0)
    FROM doomed
    GROUP BY 1, 2, 3, 4
    ON CONFLICT (day, team_id, provider, served_model) DO UPDATE SET
        requests      = d.requests      + EXCLUDED.requests,
        errors        = d.errors        + EXCLUDED.errors,
        input_tokens  = d.input_tokens  + EXCLUDED.input_tokens,
        output_tokens = d.output_tokens + EXCLUDED.output_tokens,
        cost_micros   = d.cost_micros   + EXCLUDED.cost_micros,
        fallback_cost_delta_micros =
            d.fallback_cost_delta_micros + EXCLUDED.fallback_cost_delta_micros
    RETURNING 1
),
removed AS (
    DELETE FROM requests WHERE id IN (SELECT id FROM doomed) RETURNING 1
)
SELECT count(*) FROM removed`

type RetentionConfig struct {
	// Window is how long detail rows are kept. Zero disables retention
	// entirely, leaving every row in place.
	Window time.Duration

	Interval  time.Duration
	BatchSize int

	// MaxBatches bounds one sweep so a large backlog is worked down over
	// several ticks instead of monopolising a connection.
	MaxBatches int
}

func (c RetentionConfig) withDefaults() RetentionConfig {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 5000
	}
	if c.MaxBatches <= 0 {
		c.MaxBatches = 200
	}
	return c
}

type Retainer struct {
	pool    *pgxpool.Pool
	cfg     RetentionConfig
	metrics *telemetry.Metrics
	log     *slog.Logger
}

func NewRetainer(pool *pgxpool.Pool, cfg RetentionConfig, metrics *telemetry.Metrics, log *slog.Logger) *Retainer {
	return &Retainer{pool: pool, cfg: cfg.withDefaults(), metrics: metrics, log: log}
}

// Run sweeps on a ticker until ctx is cancelled. A failed sweep is logged and
// retried on the next tick: retention falling behind is an operational
// problem, never a reason to fail a request.
func (r *Retainer) Run(ctx context.Context) {
	if r.cfg.Window <= 0 {
		r.log.Info("request log retention disabled")
		return
	}

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	for {
		if _, err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("request log retention sweep", slog.Any("error", err))
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// Sweep runs one full pass and reports how many detail rows it removed. It is
// exported so a test can drive a pass without waiting on the ticker.
func (r *Retainer) Sweep(ctx context.Context) (int64, error) {
	if r.cfg.Window <= 0 {
		return 0, nil
	}

	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquiring connection for retention: %w", err)
	}
	defer conn.Release()

	// try, not wait: if another replica is already sweeping there is nothing
	// useful to do but come back next tick.
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", retentionLockKey).Scan(&got); err != nil {
		return 0, fmt.Errorf("acquiring retention lock: %w", err)
	}
	if !got {
		return 0, nil
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", retentionLockKey)

	cutoff := time.Now().UTC().Add(-r.cfg.Window)

	var total int64
	for range r.cfg.MaxBatches {
		var n int64
		if err := conn.QueryRow(ctx, sweepSQL, cutoff, r.cfg.BatchSize).Scan(&n); err != nil {
			return total, fmt.Errorf("sweeping expired request log rows: %w", err)
		}
		total += n
		if n < int64(r.cfg.BatchSize) {
			break
		}
	}

	if r.metrics != nil {
		r.metrics.RetentionRowsDeletedTotal.Add(float64(total))
		r.metrics.RetentionLastSweepTimestamp.Set(float64(time.Now().Unix()))
	}
	if total > 0 {
		r.log.Info("request log retention swept",
			slog.Int64("rows", total), slog.Time("cutoff", cutoff))
	}
	return total, nil
}
