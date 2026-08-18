package resilience

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
		// Long enough that no test hits the abandoned-probe path by accident;
		// the test that exercises it shortens this deliberately.
		ProbeTimeout: 5 * time.Second,
		// Zero would fail validation, and anything above zero is ignored
		// entirely by the store-less breakers most of this file builds. The
		// Step 7.3 tests override it where the caching behaviour is the point.
		StateCacheTTL: 250 * time.Millisecond,
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
		"zero probe timeout": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.ProbeTimeout = 0; return c },
			wantErr: true,
		},
		"negative probe timeout": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.ProbeTimeout = -time.Second; return c },
			wantErr: true,
		},
		"zero state cache ttl": {
			mutate:  func(c BreakerConfig) BreakerConfig { c.StateCacheTTL = 0; return c },
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

// --- Step 7.2: half-open concurrency ---------------------------------------

// TestHalfOpenAdmitsExactlyOneProbe is Step 7.2's core rule at its simplest:
// the first caller into HalfOpen gets through, and every caller after it is
// rejected as if the breaker were still Open.
func TestHalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg) // this Allow claims the probe

	for i := 0; i < 5; i++ {
		if b.Allow(ctx) {
			t.Fatalf("Allow() = true on call %d while a probe is in flight, want false", i+2)
		}
	}
	if got := b.State(); got != StateHalfOpen {
		t.Errorf("State() = %v, want still HalfOpen — rejecting extra callers must not change state", got)
	}
}

// TestHalfOpenAdmitsOneProbeUnderConcurrency is the version that matters:
// many goroutines racing into Allow at the moment the cooldown expires, with
// exactly one winning. Run under -race, this is what proves the guard is a
// real mutual exclusion and not a check-then-act window.
func TestHalfOpenAdmitsOneProbeUnderConcurrency(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)

	// Sleep past the cooldown but do NOT call Allow — every goroutine below
	// races for both the Open -> HalfOpen transition and the probe slot.
	time.Sleep(cfg.CooldownBase + 10*time.Millisecond)

	const goroutines = 50
	var admitted atomic.Int64
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // release all goroutines at once
			if b.Allow(ctx) {
				admitted.Add(1)
			}
		}()
	}

	start.Done()
	done.Wait()

	if got := admitted.Load(); got != 1 {
		t.Fatalf("%d of %d concurrent callers admitted in HalfOpen, want exactly 1", got, goroutines)
	}
}

// TestHalfOpenAdmitsNextProbeAfterOutcomeReported proves the slot is returned
// on report, not held until the breaker closes: a probe that succeeds without
// meeting SuccessThreshold leaves the breaker HalfOpen and ready for the next
// probe, which is how a SuccessThreshold above 1 makes progress at all.
func TestHalfOpenAdmitsNextProbeAfterOutcomeReported(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	cfg.SuccessThreshold = 3 // above 1, so one success cannot close the breaker
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg)

	if b.Allow(ctx) {
		t.Fatalf("Allow() = true while the first probe is in flight, want false")
	}

	b.RecordSuccess(ctx)
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("State() = %v after 1 of %d successes, want still HalfOpen", got, cfg.SuccessThreshold)
	}
	if !b.Allow(ctx) {
		t.Fatalf("Allow() = false after the previous probe reported, want true — the slot should be free")
	}
}

// TestHalfOpenReclaimsAbandonedProbeAfterTimeout proves the safety valve: a
// caller that claims the slot and never reports back cannot wedge the breaker
// in HalfOpen rejecting everything forever.
func TestHalfOpenReclaimsAbandonedProbeAfterTimeout(t *testing.T) {
	log, buf := newBreakerTestLogger()
	cfg := testBreakerConfig()
	cfg.ProbeTimeout = 30 * time.Millisecond
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg) // claims the probe, then never reports

	if b.Allow(ctx) {
		t.Fatalf("Allow() = true before ProbeTimeout elapsed, want false")
	}

	time.Sleep(cfg.ProbeTimeout + 10*time.Millisecond)

	if !b.Allow(ctx) {
		t.Fatalf("Allow() = false after ProbeTimeout elapsed, want true — the abandoned slot should be reclaimed")
	}
	if !strings.Contains(buf.String(), "reclaiming abandoned circuit breaker probe") {
		t.Errorf("log output = %q, want the abandoned-probe reclaim logged", buf.String())
	}
}

// TestClosedIsNotProbeGuarded proves the single-probe restriction applies to
// HalfOpen only. A Closed breaker is the normal path for all production
// traffic; serializing it would make the breaker itself the bottleneck.
func TestClosedIsNotProbeGuarded(t *testing.T) {
	log, _ := newBreakerTestLogger()
	b := newTestBreaker(t, testBreakerConfig(), log)
	ctx := context.Background()

	const goroutines = 50
	var admitted atomic.Int64
	var done sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			if b.Allow(ctx) {
				admitted.Add(1)
			}
		}()
	}
	done.Wait()

	if got := admitted.Load(); got != goroutines {
		t.Fatalf("%d of %d concurrent callers admitted while Closed, want all of them", got, goroutines)
	}
}

// TestClosingReleasesTheProbeGuard proves a recovered breaker goes back to
// unrestricted traffic — the guard does not survive HalfOpen -> Closed.
func TestClosingReleasesTheProbeGuard(t *testing.T) {
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

	for i := 0; i < 5; i++ {
		if !b.Allow(ctx) {
			t.Fatalf("Allow() = false on call %d after closing, want true", i+1)
		}
	}
}

// TestReopeningReleasesTheProbeGuard proves a failed probe leaves no stale
// claim behind: once the next cooldown elapses, a fresh probe is admitted
// rather than being blocked by the previous one's slot.
func TestReopeningReleasesTheProbeGuard(t *testing.T) {
	log, _ := newBreakerTestLogger()
	cfg := testBreakerConfig()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	waitForHalfOpen(ctx, t, b, cfg)
	b.RecordFailure(ctx) // probe fails, breaker reopens with a longer cooldown

	// waitForHalfOpen reads the breaker's current cooldown, so this correctly
	// waits out the grown one rather than the base.
	waitForHalfOpen(ctx, t, b, cfg)
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
//
// Note that its Allow call claims the single probe slot (Step 7.2), so a test
// calling this holds the probe until it reports an outcome.
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
