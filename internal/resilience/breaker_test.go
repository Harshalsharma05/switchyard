package resilience

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newBreakerTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// testBreakerConfig gives every field a small, round value so window- and
// cooldown-based tests can use real time.Sleep calls without running slow.
func testBreakerConfig() BreakerConfig {
	return BreakerConfig{
		FailureThreshold: 3,
		Window:           50 * time.Millisecond,
		CooldownBase:     20 * time.Millisecond,
		CooldownMax:      200 * time.Millisecond,
		SuccessThreshold: 2,
	}
}

func newTestBreaker(t *testing.T, cfg BreakerConfig, log *slog.Logger) *Breaker {
	t.Helper()
	b, err := NewBreaker(cfg, log, Labels{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatalf("NewBreaker() error: %v", err)
	}
	return b
}

func TestNewBreakerValidation(t *testing.T) {
	base := testBreakerConfig()

	tests := map[string]struct {
		mutate  func(BreakerConfig) BreakerConfig
		wantErr bool
	}{
		"valid": {mutate: func(c BreakerConfig) BreakerConfig { return c }},
		"zero failure threshold": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.FailureThreshold = 0; return c },
			wantErr: true,
		},
		"negative failure threshold": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.FailureThreshold = -1; return c },
			wantErr: true,
		},
		"zero window": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.Window = 0; return c },
			wantErr: true,
		},
		"zero cooldown base": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.CooldownBase = 0; return c },
			wantErr: true,
		},
		"cooldown max below base": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.CooldownMax = c.CooldownBase - time.Millisecond; return c },
			wantErr: true,
		},
		"cooldown max equal base is ok": {
			mutate: func(c BreakerConfig) BreakerConfig { c.CooldownMax = c.CooldownBase; return c },
		},
		"zero success threshold": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.SuccessThreshold = 0; return c },
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewBreaker(tt.mutate(base), discardLog(), Labels{Provider: "p"})
			if tt.wantErr && err == nil {
				t.Fatalf("NewBreaker() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("NewBreaker() unexpected error: %v", err)
			}
		})
	}
}

func TestNewBreakerRejectsNilLogger(t *testing.T) {
	if _, err := NewBreaker(testBreakerConfig(), nil, Labels{Provider: "p"}); err == nil {
		t.Fatalf("NewBreaker() error = nil, want error for a nil logger")
	}
}

// TestBreakerStartsClosedAndAllowsTraffic proves the Step 7.1 default: a
// freshly built breaker is Closed and lets every request through, the same
// optimistic-until-proven-otherwise shape health.NewMonitor uses.
func TestBreakerStartsClosedAndAllowsTraffic(t *testing.T) {
	log, _ := newBreakerTestLogger()
	b := newTestBreaker(t, testBreakerConfig(), log)

	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v, want Closed", got)
	}
	if !b.Allow(context.Background()) {
		t.Fatalf("Allow() = false, want true on a fresh Closed breaker")
	}
}

// TestClosedStaysClosedBelowThreshold proves a failure count under
// FailureThreshold does not open the breaker.
func TestClosedStaysClosedBelowThreshold(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold-1; i++ {
		b.RecordFailure(ctx)
	}

	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v, want Closed with %d failures (threshold %d)", got, cfg.FailureThreshold-1, cfg.FailureThreshold)
	}
}

// TestClosedToOpenAtThreshold proves Closed -> Open fires exactly when the
// failure count reaches FailureThreshold within Window, and that it happens
// on the failure that crosses the line, not one after.
func TestClosedToOpenAtThreshold(t *testing.T) {
	log, buf := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}

	if got := b.State(); got != StateOpen {
		t.Fatalf("State() = %v, want Open after %d failures", got, cfg.FailureThreshold)
	}
	if b.Allow(ctx) {
		t.Errorf("Allow() = true, want false immediately after opening")
	}
	if !strings.Contains(buf.String(), "failure_threshold_exceeded") {
		t.Errorf("log output = %q, want the failure_threshold_exceeded transition logged", buf.String())
	}
}

// TestFailuresOutsideWindowDoNotCount proves the rolling window: failures
// older than Window are pruned and do not contribute toward the threshold, so
// a slow trickle of failures never opens the breaker the way a burst does.
func TestFailuresOutsideWindowDoNotCount(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	b.RecordFailure(ctx)
	time.Sleep(cfg.Window + 10*time.Millisecond)
	b.RecordFailure(ctx)
	b.RecordFailure(ctx)

	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v, want Closed — the first failure should have aged out of the window", got)
	}
}

// TestOpenRejectsUntilCooldownElapses proves Allow stays false for the whole
// cooldown and only flips once it has actually elapsed.
func TestOpenRejectsUntilCooldownElapses(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}

	if b.Allow(ctx) {
		t.Fatalf("Allow() = true immediately after opening, want false")
	}

	time.Sleep(cfg.CooldownBase / 2)
	if b.Allow(ctx) {
		t.Fatalf("Allow() = true before cooldown elapsed, want false")
	}

	time.Sleep(cfg.CooldownBase)
	if !b.Allow(ctx) {
		t.Fatalf("Allow() = false after cooldown elapsed, want true (HalfOpen probe)")
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("State() = %v, want HalfOpen after cooldown", got)
	}
}

// TestHalfOpenClosesAfterSuccessStreak proves HalfOpen -> Closed only fires
// once SuccessThreshold consecutive probes succeed, not before.
func TestHalfOpenClosesAfterSuccessStreak(t *testing.T) {
	log, buf := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg)

	for i := 0; i < cfg.SuccessThreshold-1; i++ {
		b.RecordSuccess(ctx)
		if got := b.State(); got != StateHalfOpen {
			t.Fatalf("State() = %v after %d/%d successes, want still HalfOpen", got, i+1, cfg.SuccessThreshold)
		}
	}

	b.RecordSuccess(ctx)
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v after %d consecutive successes, want Closed", got, cfg.SuccessThreshold)
	}
	if !b.Allow(ctx) {
		t.Errorf("Allow() = false on a freshly Closed breaker, want true")
	}
	if !strings.Contains(buf.String(), "probe_success_streak") {
		t.Errorf("log output = %q, want the probe_success_streak transition logged", buf.String())
	}
}

// TestHalfOpenReopensOnSingleProbeFailure proves any probe failure in
// HalfOpen reopens immediately — one bad probe is enough, no threshold to
// exceed the way Closed's failure count has one.
func TestHalfOpenReopensOnSingleProbeFailure(t *testing.T) {
	log, buf := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg)

	b.RecordFailure(ctx)

	if got := b.State(); got != StateOpen {
		t.Fatalf("State() = %v after a single probe failure, want Open", got)
	}
	if !strings.Contains(buf.String(), "probe_failed") {
		t.Errorf("log output = %q, want the probe_failed transition logged", buf.String())
	}
}

// TestCooldownGrowsExponentiallyOnRepeatedOpens proves the second Open
// episode (Closed -> Open -> HalfOpen -> Open again) waits longer than the
// first, up to CooldownMax — Step 7.1's "consider making the cooldown
// exponential on repeated failures."
func TestCooldownGrowsExponentiallyOnRepeatedOpens(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg)
	b.RecordFailure(ctx) // HalfOpen -> Open again, second episode

	b.mu.Lock()
	got := b.cooldown
	b.mu.Unlock()

	want := 2 * cfg.CooldownBase
	if got != want {
		t.Fatalf("cooldown after second Open = %v, want %v (base doubled)", got, want)
	}
}

// TestCooldownResetsAfterFullRecovery proves a breaker that closes cleanly
// forgets its exponential growth: the next time it opens, it waits
// CooldownBase again, not wherever the previous episode left off.
func TestCooldownResetsAfterFullRecovery(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg)
	for i := 0; i < cfg.SuccessThreshold; i++ {
		b.RecordSuccess(ctx)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v, want Closed after the success streak", got)
	}

	openBreaker(ctx, t, b, cfg)

	b.mu.Lock()
	got := b.cooldown
	b.mu.Unlock()

	if got != cfg.CooldownBase {
		t.Fatalf("cooldown after a fresh open following full recovery = %v, want CooldownBase (%v)", got, cfg.CooldownBase)
	}
}

// TestCooldownCapsAtMax proves repeated Open episodes stop growing once they
// reach CooldownMax, rather than doubling past it.
func TestCooldownCapsAtMax(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	cfg.CooldownMax = cfg.CooldownBase * 3 // forces a cap well before many doublings
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	for episode := 0; episode < 5; episode++ {
		openBreaker(ctx, t, b, cfg)
		waitForHalfOpen(ctx, t, b, cfg)
		b.RecordFailure(ctx)

		b.mu.Lock()
		cooldown := b.cooldown
		b.mu.Unlock()
		if cooldown > cfg.CooldownMax {
			t.Fatalf("episode %d: cooldown = %v, want capped at %v", episode, cooldown, cfg.CooldownMax)
		}
	}
}

// openBreaker drives a Closed breaker to Open by recording FailureThreshold
// failures, and fails the test if it doesn't land there.
func openBreaker(ctx context.Context, t *testing.T, b *Breaker, cfg BreakerConfig) {
	t.Helper()
	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("openBreaker: State() = %v, want Open", got)
	}
}

// waitForHalfOpen sleeps past the breaker's current cooldown and calls Allow
// to trigger Open -> HalfOpen, failing the test if it doesn't land there.
func waitForHalfOpen(ctx context.Context, t *testing.T, b *Breaker, cfg BreakerConfig) {
	t.Helper()
	b.mu.Lock()
	cooldown := b.cooldown
	b.mu.Unlock()

	time.Sleep(cooldown + 10*time.Millisecond)
	if !b.Allow(ctx) {
		t.Fatalf("waitForHalfOpen: Allow() = false after cooldown, want true")
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("waitForHalfOpen: State() = %v, want HalfOpen", got)
	}
}
