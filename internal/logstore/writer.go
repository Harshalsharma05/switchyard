// Buffered, non-blocking Postgres writer for the request log.
package logstore

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

const insertSQL = `
INSERT INTO requests (
	id, ts, team_id, requested_model, served_model, provider, status_code,
	input_tokens, output_tokens, cost_micros, latency_ms, overhead_ms,
	fallback, cache_hit, quality_score, trace_id, fallback_cost_delta_micros,
	routing_tier, routing_reason, routing_savings_micros
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
ON CONFLICT (id) DO NOTHING`

// Record is one request-log row. Empty strings are stored as NULL; CacheHit
// and QualityScore stay nil until Phases 7 and 9 fill them in.
type Record struct {
	ID             string
	Timestamp      time.Time
	TeamID         string
	RequestedModel string
	ServedModel    string
	Provider       string
	StatusCode     int
	InputTokens    int
	OutputTokens   int
	CostMicros     int64
	LatencyMS      float64
	OverheadMS     float64
	Fallback       bool
	CacheHit       *bool
	QualityScore   *float64
	TraceID        string

	// FallbackCostDeltaMicros is set only when Fallback is true: the served
	// model's real cost minus what the requested model would have cost for the
	// same token usage. Negative when the fallback was cheaper.
	FallbackCostDeltaMicros *int64

	// RoutingTier and RoutingReason are set only when Step 8.2's routing chose
	// the model. Both empty means the caller named a model and routing never
	// ran, which the NULL columns keep distinct from "ran and chose nothing".
	RoutingTier   string
	RoutingReason string

	// RoutingSavingsMicros is what routing avoided spending on this request:
	// the top tier's price for the same real usage, minus what was spent. Zero
	// when routing chose the top tier; nil when routing never ran.
	RoutingSavingsMicros *int64

	// QualitySampleReason is why Phase 9 sampled this request for scoring
	// ("downgraded", "near_threshold_cache_hit", ...). Empty until a score
	// is written, and always empty when QualityScore is nil.
	QualitySampleReason string
}

type Config struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	FlushTimeout  time.Duration
}

func (c Config) withDefaults() Config {
	if c.QueueSize <= 0 {
		c.QueueSize = 4096
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = time.Second
	}
	if c.FlushTimeout <= 0 {
		c.FlushTimeout = 5 * time.Second
	}
	return c
}

type Writer struct {
	pool    *pgxpool.Pool
	cfg     Config
	queue   chan Record
	done    chan struct{}
	dropped atomic.Int64
	metrics *telemetry.Metrics
	log     *slog.Logger
}

func NewWriter(pool *pgxpool.Pool, cfg Config, metrics *telemetry.Metrics, log *slog.Logger) *Writer {
	cfg = cfg.withDefaults()
	return &Writer{
		pool:    pool,
		cfg:     cfg,
		queue:   make(chan Record, cfg.QueueSize),
		done:    make(chan struct{}),
		metrics: metrics,
		log:     log,
	}
}

// Write enqueues a row. It never blocks and takes no context on purpose: the
// request's context is usually already cancelled by this point, and a full
// queue drops the row rather than slowing the request path.
func (w *Writer) Write(rec Record) {
	select {
	case w.queue <- rec:
	default:
		w.dropped.Add(1)
		w.count("dropped", 1)
	}
}

// Run flushes batches until ctx is cancelled, then drains whatever is still
// queued under its own deadline before returning.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]Record, 0, w.cfg.BatchSize)
	for {
		select {
		case rec := <-w.queue:
			batch = append(batch, rec)
			if len(batch) >= w.cfg.BatchSize {
				batch = w.flush(ctx, batch)
			}
		case <-ticker.C:
			batch = w.flush(ctx, batch)
			w.reportQueue()
		case <-ctx.Done():
			w.finalFlush(batch)
			return
		}
	}
}

// Wait blocks until Run's final flush has finished or timeout elapses.
func (w *Writer) Wait(timeout time.Duration) {
	select {
	case <-w.done:
	case <-time.After(timeout):
		w.log.Warn("request log writer did not finish flushing", slog.Duration("timeout", timeout))
	}
}

// finalFlush pulls everything left in the queue and writes it with a fresh
// context, since the one that triggered shutdown is already cancelled.
func (w *Writer) finalFlush(batch []Record) {
	for {
		select {
		case rec := <-w.queue:
			batch = append(batch, rec)
			continue
		default:
		}
		break
	}
	if len(batch) == 0 {
		return
	}
	w.flush(context.Background(), batch)
}

// flush writes batch in one round trip and returns it emptied for reuse. A
// failure is logged and counted, never returned: the response has already been
// delivered, so the log must fail open.
func (w *Writer) flush(ctx context.Context, batch []Record) []Record {
	if len(batch) == 0 {
		return batch
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.FlushTimeout)
	defer cancel()

	b := &pgx.Batch{}
	for _, rec := range batch {
		b.Queue(insertSQL,
			rec.ID, rec.Timestamp, rec.TeamID,
			nullable(rec.RequestedModel), nullable(rec.ServedModel), nullable(rec.Provider),
			rec.StatusCode, rec.InputTokens, rec.OutputTokens, rec.CostMicros,
			rec.LatencyMS, rec.OverheadMS, rec.Fallback,
			rec.CacheHit, rec.QualityScore, nullable(rec.TraceID),
			rec.FallbackCostDeltaMicros,
			nullable(rec.RoutingTier), nullable(rec.RoutingReason),
			rec.RoutingSavingsMicros,
		)
	}

	if err := w.pool.SendBatch(writeCtx, b).Close(); err != nil {
		w.log.Error("writing request log batch",
			slog.Int("rows", len(batch)), slog.Any("error", err))
		w.count("failed", len(batch))
		return batch[:0]
	}

	w.count("written", len(batch))
	return batch[:0]
}

func (w *Writer) reportQueue() {
	if n := w.dropped.Swap(0); n > 0 {
		w.log.Warn("request log queue full, rows dropped", slog.Int64("dropped", n))
	}
	if w.metrics != nil {
		w.metrics.RequestLogQueueDepth.Set(float64(len(w.queue)))
	}
}

func (w *Writer) count(outcome string, n int) {
	if w.metrics != nil {
		w.metrics.RequestLogRowsTotal.WithLabelValues(outcome).Add(float64(n))
	}
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
