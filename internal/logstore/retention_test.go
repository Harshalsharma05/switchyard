package logstore

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type dailyTotals struct {
	rows, requests, errors, input, output, cost int64
}

func readDaily(t *testing.T, w *Writer, ctx context.Context) dailyTotals {
	t.Helper()
	var d dailyTotals
	err := w.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(requests),0), COALESCE(sum(errors),0),
		       COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
		       COALESCE(sum(cost_micros),0)
		FROM requests_daily`).Scan(&d.rows, &d.requests, &d.errors, &d.input, &d.output, &d.cost)
	if err != nil {
		t.Fatalf("reading requests_daily: %v", err)
	}
	return d
}

func countRequests(t *testing.T, w *Writer, ctx context.Context) int64 {
	t.Helper()
	var n int64
	if err := w.pool.QueryRow(ctx, "SELECT count(*) FROM requests").Scan(&n); err != nil {
		t.Fatalf("counting requests: %v", err)
	}
	return n
}

// The property that matters: every expired row is summed into the daily table
// exactly once and removed exactly once, even when the sweep spans many
// batches. A double-count or a dropped batch both show up here.
func TestSweepConservesTotalsAcrossBatches(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	const expired = 250
	const fresh = 7
	old := time.Now().UTC().Add(-72 * time.Hour)

	recs := make([]Record, 0, expired+fresh)
	var wantErrors int64
	for i := range expired {
		rec := sampleRecord(fmt.Sprintf("old-%03d", i))
		rec.Timestamp = old
		rec.InputTokens, rec.OutputTokens, rec.CostMicros = 10, 5, 13
		if i%10 == 0 {
			rec.StatusCode = 500
			wantErrors++
		}
		recs = append(recs, rec)
	}
	for i := range fresh {
		rec := sampleRecord(fmt.Sprintf("new-%03d", i))
		rec.Timestamp = time.Now().UTC()
		recs = append(recs, rec)
	}
	seed(t, w, ctx, recs)

	// A batch size far below the row count forces the multi-batch path, which
	// is where an accumulation bug would hide.
	r := NewRetainer(pool, RetentionConfig{
		Window:    24 * time.Hour,
		BatchSize: 30,
	}, nil, discardLogger())

	deleted, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != expired {
		t.Errorf("swept %d rows, want %d", deleted, expired)
	}
	if got := countRequests(t, w, ctx); got != fresh {
		t.Errorf("%d detail rows remain, want the %d inside the window", got, fresh)
	}

	d := readDaily(t, w, ctx)
	if d.requests != expired {
		t.Errorf("summary counted %d requests, want %d", d.requests, expired)
	}
	if d.errors != wantErrors {
		t.Errorf("summary counted %d errors, want %d", d.errors, wantErrors)
	}
	if d.input != expired*10 || d.output != expired*5 {
		t.Errorf("summary tokens = %d/%d, want %d/%d", d.input, d.output, expired*10, expired*5)
	}
	if d.cost != expired*13 {
		t.Errorf("summary cost = %d, want %d", d.cost, expired*13)
	}
}

// A second sweep must find nothing and must not inflate the summary it already
// wrote — the guarantee that makes an hourly ticker safe.
func TestSweepIsSafeToRepeat(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	rec := sampleRecord("old")
	rec.Timestamp = time.Now().UTC().Add(-72 * time.Hour)
	seed(t, w, ctx, []Record{rec})

	r := NewRetainer(pool, RetentionConfig{Window: 24 * time.Hour}, nil, discardLogger())
	if _, err := r.Sweep(ctx); err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	first := readDaily(t, w, ctx)

	deleted, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if deleted != 0 {
		t.Errorf("second sweep removed %d rows, want 0", deleted)
	}
	if second := readDaily(t, w, ctx); second != first {
		t.Errorf("summary changed on a no-op sweep: %+v then %+v", first, second)
	}
}

// Rows from the same day land on one summary row and accumulate, rather than
// each sweep appending a duplicate.
func TestSweepAccumulatesIntoOneRowPerDay(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	old := time.Now().UTC().Add(-72 * time.Hour)
	r := NewRetainer(pool, RetentionConfig{Window: 24 * time.Hour}, nil, discardLogger())

	for pass := range 3 {
		rec := sampleRecord(fmt.Sprintf("pass-%d", pass))
		rec.Timestamp = old
		rec.CostMicros = 100
		seed(t, w, ctx, []Record{rec})
		if _, err := r.Sweep(ctx); err != nil {
			t.Fatalf("sweep %d: %v", pass, err)
		}
	}

	d := readDaily(t, w, ctx)
	if d.rows != 1 {
		t.Errorf("summary has %d rows, want 1 (same day, team, provider, model)", d.rows)
	}
	if d.requests != 3 || d.cost != 300 {
		t.Errorf("summary = %d requests / %d micros, want 3 / 300", d.requests, d.cost)
	}
}

// A request that never reached a provider still has to be summarised; NULL
// provider and model become empty strings so they group instead of vanishing.
func TestSweepSummarisesRowsWithNoProvider(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	rec := sampleRecord("rejected")
	rec.Timestamp = time.Now().UTC().Add(-72 * time.Hour)
	rec.StatusCode, rec.Provider, rec.ServedModel = 402, "", ""
	seed(t, w, ctx, []Record{rec})

	r := NewRetainer(pool, RetentionConfig{Window: 24 * time.Hour}, nil, discardLogger())
	if _, err := r.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	d := readDaily(t, w, ctx)
	if d.requests != 1 || d.errors != 1 {
		t.Errorf("summary = %d requests / %d errors, want 1 / 1", d.requests, d.errors)
	}
}

func TestSweepDisabledByZeroWindow(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	rec := sampleRecord("ancient")
	rec.Timestamp = time.Now().UTC().Add(-10000 * time.Hour)
	seed(t, w, ctx, []Record{rec})

	r := NewRetainer(pool, RetentionConfig{Window: 0}, nil, discardLogger())
	if deleted, err := r.Sweep(ctx); err != nil || deleted != 0 {
		t.Fatalf("Sweep with a zero window: deleted=%d err=%v", deleted, err)
	}
	if countRequests(t, w, ctx) != 1 {
		t.Error("a zero window deleted rows; retention must be off entirely")
	}
}
