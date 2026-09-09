package telemetry

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func panicCount(t *testing.T, m *Metrics, source string) float64 {
	t.Helper()
	var d dto.Metric
	if err := m.PanicsTotal.WithLabelValues(source).Write(&d); err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	return d.GetCounter().GetValue()
}

// A loop that exits on its own (ctx cancelled) must not be restarted, and must
// not touch the panic counter.
func TestSuperviseReturnsOnCleanExit(t *testing.T) {
	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	var runs atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// fn returns on its own, the way a real loop does once it sees its
		// context cancelled. Supervise must treat that as done, not restart it.
		Supervise(ctx, discardLogger(), m, "clean", func() {
			runs.Add(1)
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise did not return after fn exited cleanly")
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("fn ran %d times, want 1 — a clean exit must not restart", got)
	}
	if got := panicCount(t, m, "clean"); got != 0 {
		t.Errorf("panic counter = %v, want 0", got)
	}
}

// A panicking loop is recovered, counted, and restarted — then stops when ctx
// is cancelled during the backoff.
func TestSuperviseRestartsAfterPanic(t *testing.T) {
	old := superviseInitialBackoff
	superviseInitialBackoff = time.Millisecond
	t.Cleanup(func() { superviseInitialBackoff = old })

	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	var runs atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		Supervise(ctx, discardLogger(), m, "flaky", func() {
			if runs.Add(1) >= 3 {
				cancel()
				<-ctx.Done()
				return
			}
			panic("boom")
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise never returned")
	}
	if got := runs.Load(); got != 3 {
		t.Errorf("fn ran %d times, want 3 (two panics restarted, third exits clean)", got)
	}
	if got := panicCount(t, m, "flaky"); got != 2 {
		t.Errorf("panic counter = %v, want 2", got)
	}
}

// nil metrics is a supported build; Supervise must not dereference it.
func TestSuperviseNilMetrics(t *testing.T) {
	old := superviseInitialBackoff
	superviseInitialBackoff = time.Millisecond
	t.Cleanup(func() { superviseInitialBackoff = old })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		Supervise(ctx, discardLogger(), nil, "no-metrics", func() { panic("boom") })
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise with nil metrics hung or crashed")
	}
}

// An already-cancelled context must still get one run of fn. The supervised
// loop is what drains on shutdown — Writer.loop flushes its queue on ctx.Done
// before returning — so skipping fn here loses queued rows whenever shutdown
// races the supervisor's first iteration.
func TestSuperviseRunsOnceWhenContextAlreadyCancelled(t *testing.T) {
	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var runs atomic.Int32
	Supervise(ctx, discardLogger(), m, "already-cancelled", func() {
		runs.Add(1)
	})

	if got := runs.Load(); got != 1 {
		t.Fatalf("fn ran %d times, want exactly 1", got)
	}
}
