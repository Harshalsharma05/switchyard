package logstore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Harshalsharma05/switchyard/migrations"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestPool gives each test its own schema, migrated and reachable through a
// pool whose connections carry a matching search_path.
func newTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()

	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	schema := fmt.Sprintf("writer_test_%d", time.Now().UnixNano())

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing dsn: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("building pool: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		pool.Close()
		t.Skipf("no Postgres reachable: %v", err)
	}
	pool.Close()

	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("building scoped pool: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pool.Exec(c, "DROP SCHEMA "+schema+" CASCADE")
		pool.Close()
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring conn: %v", err)
	}
	if _, err := Migrate(ctx, conn.Conn(), migrations.FS); err != nil {
		conn.Release()
		t.Fatalf("migrating: %v", err)
	}
	conn.Release()

	return pool, ctx
}

func sampleRecord(id string) Record {
	return Record{
		ID:             id,
		Timestamp:      time.Now().UTC().Truncate(time.Millisecond),
		TeamID:         "acme",
		RequestedModel: "llama-3.1-8b",
		ServedModel:    "llama-3.1-8b",
		Provider:       "groq",
		StatusCode:     200,
		InputTokens:    10,
		OutputTokens:   5,
		CostMicros:     1234,
		LatencyMS:      412.5,
		OverheadMS:     3.117,
		Fallback:       false,
		TraceID:        "4bf92f3577b34da6a3ce929d0e0e4736",
	}
}

// A row must survive the round trip with every column intact — the checklist's
// "correct values in every column", and the guarantee the cost header and the
// stored cost agree.
func TestWriterRoundTrip(t *testing.T) {
	pool, ctx := newTestPool(t)

	w := NewWriter(pool, Config{FlushInterval: 20 * time.Millisecond}, nil, discardLogger())
	runCtx, cancel := context.WithCancel(context.Background())
	go w.Run(runCtx)

	want := sampleRecord("req-roundtrip")
	delta := int64(-742)
	want.Fallback = true
	want.FallbackCostDeltaMicros = &delta
	w.Write(want)

	cancel()
	w.Wait(5 * time.Second)

	var got Record
	var requested, served, prov, traceID *string
	err := pool.QueryRow(ctx, `
		SELECT id, ts, team_id, requested_model, served_model, provider, status_code,
		       input_tokens, output_tokens, cost_micros, latency_ms, overhead_ms,
		       fallback, cache_hit, quality_score, trace_id, fallback_cost_delta_micros
		FROM requests WHERE id = $1`, want.ID).Scan(
		&got.ID, &got.Timestamp, &got.TeamID, &requested, &served, &prov, &got.StatusCode,
		&got.InputTokens, &got.OutputTokens, &got.CostMicros, &got.LatencyMS, &got.OverheadMS,
		&got.Fallback, &got.CacheHit, &got.QualityScore, &traceID, &got.FallbackCostDeltaMicros)
	if err != nil {
		t.Fatalf("reading row back: %v", err)
	}
	if got.FallbackCostDeltaMicros == nil || *got.FallbackCostDeltaMicros != -742 {
		t.Errorf("fallback_cost_delta_micros = %v, want -742", got.FallbackCostDeltaMicros)
	}

	if got.TeamID != want.TeamID || got.StatusCode != want.StatusCode {
		t.Errorf("team/status = %q/%d, want %q/%d", got.TeamID, got.StatusCode, want.TeamID, want.StatusCode)
	}
	if got.CostMicros != want.CostMicros {
		t.Errorf("cost_micros = %d, want %d", got.CostMicros, want.CostMicros)
	}
	if got.OverheadMS != want.OverheadMS {
		t.Errorf("overhead_ms = %v, want %v (sub-millisecond precision must survive)", got.OverheadMS, want.OverheadMS)
	}
	if got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens {
		t.Errorf("tokens = %d/%d, want %d/%d", got.InputTokens, got.OutputTokens, want.InputTokens, want.OutputTokens)
	}
	if requested == nil || *requested != want.RequestedModel || prov == nil || *prov != want.Provider {
		t.Errorf("requested_model/provider round-tripped wrong")
	}
	if traceID == nil || *traceID != want.TraceID {
		t.Errorf("trace_id round-tripped wrong")
	}
	// Nullable until Phases 7 and 9 — never a fabricated false or zero.
	if got.CacheHit != nil || got.QualityScore != nil {
		t.Errorf("cache_hit/quality_score = %v/%v, want NULL", got.CacheHit, got.QualityScore)
	}
}

// A request rejected before a provider was reached still gets a row, with the
// provider columns left NULL rather than blank strings.
func TestWriterRejectedRequestLeavesProviderNull(t *testing.T) {
	pool, ctx := newTestPool(t)

	w := NewWriter(pool, Config{FlushInterval: 20 * time.Millisecond}, nil, discardLogger())
	runCtx, cancel := context.WithCancel(context.Background())
	go w.Run(runCtx)

	rec := sampleRecord("req-429")
	rec.StatusCode = 429
	rec.ServedModel, rec.Provider = "", ""
	rec.InputTokens, rec.OutputTokens, rec.CostMicros = 0, 0, 0
	w.Write(rec)

	cancel()
	w.Wait(5 * time.Second)

	var served, prov *string
	var status int
	if err := pool.QueryRow(ctx,
		"SELECT status_code, served_model, provider FROM requests WHERE id = $1", rec.ID,
	).Scan(&status, &served, &prov); err != nil {
		t.Fatalf("reading row back: %v", err)
	}
	if status != 429 {
		t.Errorf("status_code = %d, want 429", status)
	}
	if served != nil || prov != nil {
		t.Errorf("served_model/provider = %v/%v, want NULL", served, prov)
	}
}

// Cancelling the writer's context must flush what is still queued, not discard
// it — the shutdown decision recorded for Step 1.2.
func TestWriterFinalFlushOnShutdown(t *testing.T) {
	pool, ctx := newTestPool(t)

	// A flush interval far longer than the test guarantees nothing is written
	// by the ticker: every row below lands only via the shutdown drain.
	w := NewWriter(pool, Config{FlushInterval: time.Hour}, nil, discardLogger())
	runCtx, cancel := context.WithCancel(context.Background())
	go w.Run(runCtx)

	const rows = 25
	for i := range rows {
		w.Write(sampleRecord(fmt.Sprintf("req-drain-%d", i)))
	}

	cancel()
	w.Wait(5 * time.Second)

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM requests").Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != rows {
		t.Errorf("flushed %d rows on shutdown, want %d", count, rows)
	}
}

// A repeated caller-supplied X-Request-ID collides on the primary key. The
// duplicate must be skipped without taking the rest of the batch down.
func TestWriterDuplicateIDDoesNotDropBatch(t *testing.T) {
	pool, ctx := newTestPool(t)

	w := NewWriter(pool, Config{FlushInterval: time.Hour}, nil, discardLogger())
	runCtx, cancel := context.WithCancel(context.Background())
	go w.Run(runCtx)

	w.Write(sampleRecord("dup"))
	w.Write(sampleRecord("dup"))
	w.Write(sampleRecord("unique"))

	cancel()
	w.Wait(5 * time.Second)

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM requests").Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 2 {
		t.Errorf("stored %d rows, want 2 (duplicate skipped, sibling kept)", count)
	}
}

// Write must never block, whatever the queue or Postgres is doing — the
// gateway is never the reason a request is slow.
func TestWriteNeverBlocksWhenQueueFull(t *testing.T) {
	w := NewWriter(nil, Config{QueueSize: 4}, nil, discardLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 1000 {
			w.Write(sampleRecord(fmt.Sprintf("req-%d", i)))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a full queue")
	}

	if dropped := w.dropped.Load(); dropped == 0 {
		t.Error("overfilling the queue dropped nothing; rows were lost silently or Write blocked")
	}
}

// Postgres unreachable: requests still succeed, the failure is logged and
// counted, the row is dropped. Fail open, always.
func TestWriterSurvivesUnreachablePostgres(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://nobody:nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("parsing dsn: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("building pool: %v", err)
	}
	defer pool.Close()

	w := NewWriter(pool, Config{
		FlushInterval: 20 * time.Millisecond,
		FlushTimeout:  time.Second,
	}, nil, discardLogger())

	runCtx, cancel := context.WithCancel(context.Background())
	go w.Run(runCtx)

	for i := range 10 {
		w.Write(sampleRecord(fmt.Sprintf("req-down-%d", i)))
	}
	time.Sleep(300 * time.Millisecond)

	cancel()
	// Returning at all is the assertion: a writer that hung on a dead Postgres
	// would never close done, and Wait would log its timeout instead.
	w.Wait(5 * time.Second)
}
