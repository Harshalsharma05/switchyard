package telemetry

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// Backoff schedule for a panicking background loop. The wait after a panic
// starts here, doubles each consecutive panic, and is capped; it resets once
// fn has run longer than the cap without panicking, so an isolated fault does
// not inherit an earlier one's penalty. var, not const, only so a test can
// shrink them.
var (
	superviseInitialBackoff = 1 * time.Second
	superviseMaxBackoff     = 30 * time.Second
)

// Supervise runs fn and keeps it running for the life of ctx.
//
// If fn returns on its own, so does Supervise — every caller's fn is a loop
// that exits only when its context is cancelled, so a clean return is a clean
// shutdown. If fn panics, the panic is recovered and logged with name and a
// stack trace, counted in switchyard_panics_total{source=name}, and fn is
// restarted after a capped, doubling backoff. Supervise also returns if ctx is
// cancelled while it is backing off.
//
// fn always runs at least once, even if ctx is already cancelled on entry.
// Cancellation is the supervised loop's own business: Writer.loop drains its
// queue on ctx.Done before returning, so skipping the call outright would
// silently discard queued rows when shutdown races the supervisor's first
// iteration.
//
// It blocks; callers launch it with `go`. This shape rather than a
// fire-and-forget spawner because the callers that fan out
// (health.Checker, quality.Worker) track their goroutines with a
// sync.WaitGroup for deterministic shutdown, which a blocking supervisor slots
// into and a spawner does not.
//
// m may be nil (a build without a metrics registry); the counter is skipped.
func Supervise(ctx context.Context, log *slog.Logger, m *Metrics, name string, fn func()) {
	backoff := superviseInitialBackoff
	for {
		start := time.Now()
		if !guard(log, m, name, fn) {
			return
		}
		if time.Since(start) > superviseMaxBackoff {
			backoff = superviseInitialBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, superviseMaxBackoff)
	}
}

// guard runs fn once and reports whether it panicked, recovering and recording
// the panic if so.
func guard(log *slog.Logger, m *Metrics, name string, fn func()) (panicked bool) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		panicked = true
		log.LogAttrs(context.Background(), slog.LevelError, "background goroutine panic recovered",
			slog.String("goroutine", name),
			slog.Any("panic", rec),
			slog.String("stack", string(debug.Stack())),
		)
		if m != nil {
			m.PanicsTotal.WithLabelValues(name).Inc()
		}
	}()
	fn()
	return false
}
