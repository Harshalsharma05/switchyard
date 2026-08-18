// Breaker implements Phase 7 Step 7.1's circuit breaker state machine —
// Closed, Open, and HalfOpen. Per CLAUDE.md this file is hand-written by the
// repo owner; it exists here only because the owner explicitly asked to
// override that rule for this session.
//
// Step 7.1 is the state machine alone: three states and the four transitions
// between them, all logged. HalfOpen here lets every caller through — the
// single-in-flight-probe guard that stops a flood of traffic from re-killing
// a recovering provider is Step 7.2, deliberately not built yet. Step 7.3
// will back this with Redis so replicas agree, and Step 7.4 wires one Breaker
// per provider+model into the fallback chain from Step 6.2.
package resilience

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// State is one of the breaker's three states.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// BreakerConfig holds Step 7.1's tunable thresholds. Named with the Breaker
// prefix, not just Config, because retry.go already exports a Config for
// Step 6.1's retry policy — the same disambiguation health package uses for
// MonitorConfig alongside Checker's own settings.
type BreakerConfig struct {
	// FailureThreshold is how many failures within Window open the breaker
	// from Closed.
	FailureThreshold int

	// Window is the rolling window Step 7.1 counts failures within. Failures
	// older than Window are pruned and no longer count toward the threshold.
	Window time.Duration

	// CooldownBase is how long Open waits before allowing a probe into
	// HalfOpen, the first time the breaker opens.
	CooldownBase time.Duration

	// CooldownMax caps the cooldown's exponential growth (Step 7.1's "consider
	// making the cooldown exponential on repeated failures") across repeated
	// Open episodes, so a provider that keeps failing its probes doesn't back
	// off forever.
	CooldownMax time.Duration

	// SuccessThreshold is how many consecutive probe successes in HalfOpen
	// close the breaker.
	SuccessThreshold int
}

func (c BreakerConfig) validate() error {
	switch {
	case c.FailureThreshold <= 0:
		return fmt.Errorf("circuit breaker: failure threshold must be positive, got %d", c.FailureThreshold)
	case c.Window <= 0:
		return fmt.Errorf("circuit breaker: window must be positive, got %s", c.Window)
	case c.CooldownBase <= 0:
		return fmt.Errorf("circuit breaker: cooldown base must be positive, got %s", c.CooldownBase)
	case c.CooldownMax < c.CooldownBase:
		return fmt.Errorf("circuit breaker: cooldown max (%s) must be at least cooldown base (%s)", c.CooldownMax, c.CooldownBase)
	case c.SuccessThreshold <= 0:
		return fmt.Errorf("circuit breaker: success threshold must be positive, got %d", c.SuccessThreshold)
	}
	return nil
}

// maxCooldownExponent bounds the exponential cooldown's shift the same way
// retry.go's maxBackoffExponent bounds fullJitter: a defensive ceiling well
// below where CooldownBase << n would overflow time.Duration's int64, not a
// value any real config reaches.
const maxCooldownExponent = 20

// Breaker is one provider+model's circuit breaker. It is safe for concurrent
// use: Allow, RecordSuccess, and RecordFailure all take the same mutex, the
// same shape as health.providerState guarding its own status.
type Breaker struct {
	mu     sync.Mutex
	cfg    BreakerConfig
	log    *slog.Logger
	labels Labels

	state State

	// failures is the rolling window of failure timestamps while Closed.
	// Pruned lazily on each failure rather than on a timer — the same
	// reasoning CLAUDE.md gives for the rate limiter's lazy refill: no
	// background goroutine, no drift.
	failures []time.Time

	// successStreak counts consecutive probe successes while HalfOpen.
	successStreak int

	// openedAt is when the breaker most recently entered Open, and cooldown
	// is how long it waits from there before allowing a probe.
	openedAt time.Time
	cooldown time.Duration

	// reopens counts how many times in a row the breaker has entered Open
	// without an intervening full close, which is what nextCooldown grows
	// against. It resets to 0 the moment HalfOpen -> Closed succeeds, so a
	// provider that recovers cleanly starts the next failure at CooldownBase
	// again rather than carrying a long cooldown from an unrelated incident.
	reopens int
}

// NewBreaker validates cfg and returns a Breaker starting Closed — the same
// optimistic default health.NewMonitor uses, so a freshly resolved
// provider+model is assumed healthy until it proves otherwise.
func NewBreaker(cfg BreakerConfig, log *slog.Logger, labels Labels) (*Breaker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		return nil, fmt.Errorf("circuit breaker: logger must not be nil")
	}
	return &Breaker{cfg: cfg, log: log, labels: labels, state: StateClosed, cooldown: cfg.CooldownBase}, nil
}

// State returns the breaker's current state without side effects — for
// admin/health reporting, where reading the state must not itself trigger an
// Open -> HalfOpen transition the way Allow does.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow reports whether a request may proceed. It is where Open -> HalfOpen
// happens: the transition is driven by a caller asking to go through after
// the cooldown has elapsed, not by a background timer, so it only ever fires
// on the request path that actually needs the answer.
//
// Every caller sees the same HalfOpen answer here — Step 7.2 is what narrows
// this to exactly one in-flight probe. Until that lands, Allow alone cannot
// prevent a stampede of concurrent HalfOpen requests.
func (b *Breaker) Allow(ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen && time.Since(b.openedAt) >= b.cooldown {
		b.setState(ctx, StateHalfOpen, "cooldown_elapsed")
	}

	return b.state != StateOpen
}

// RecordSuccess reports that a call this breaker allowed succeeded.
func (b *Breaker) RecordSuccess(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != StateHalfOpen {
		// A Closed success needs no bookkeeping: the failure window only
		// tracks failures, and it ages out on its own via pruneBefore.
		return
	}

	b.successStreak++
	if b.successStreak >= b.cfg.SuccessThreshold {
		b.reopens = 0
		b.cooldown = b.cfg.CooldownBase
		b.setState(ctx, StateClosed, "probe_success_streak")
	}
}

// RecordFailure reports that a call this breaker allowed failed.
func (b *Breaker) RecordFailure(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		// Any probe failure reopens immediately — a single bad reading is
		// enough evidence the provider isn't ready, the same "one failure
		// interrupts recovery" convention health.Monitor uses for its own
		// hysteresis.
		b.open(ctx, "probe_failed")

	case StateClosed:
		now := time.Now()
		b.failures = pruneBefore(append(b.failures, now), now.Add(-b.cfg.Window))
		if len(b.failures) >= b.cfg.FailureThreshold {
			b.open(ctx, "failure_threshold_exceeded")
		}

	case StateOpen:
		// Already open; nothing this failure needs to change.
	}
}

// open commits the Closed -> Open or HalfOpen -> Open transition, growing the
// cooldown exponentially with each consecutive episode and resetting the
// failure window and success streak so the next state starts clean.
func (b *Breaker) open(ctx context.Context, reason string) {
	b.reopens++
	b.cooldown = nextCooldown(b.cfg, b.reopens)
	b.successStreak = 0
	b.failures = nil
	b.setState(ctx, StateOpen, reason)
}

// nextCooldown computes Open's wait before the next HalfOpen probe:
// CooldownBase doubled for each consecutive reopen, capped at CooldownMax.
func nextCooldown(cfg BreakerConfig, reopens int) time.Duration {
	exponent := reopens - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > maxCooldownExponent {
		exponent = maxCooldownExponent
	}

	d := cfg.CooldownBase << uint(exponent)
	if d <= 0 || d > cfg.CooldownMax {
		return cfg.CooldownMax
	}
	return d
}

// pruneBefore drops every timestamp older than cutoff from a slice kept in
// ascending time order, which append naturally maintains here since failures
// are only ever appended as they happen.
func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

// setState commits a transition: updates state, stamps openedAt when
// entering Open (Allow's cooldown check reads it from there), and logs the
// triggering condition. Assumes b.mu is already held.
func (b *Breaker) setState(ctx context.Context, next State, reason string) {
	prev := b.state
	b.state = next
	if next == StateOpen {
		b.openedAt = time.Now()
	}

	level := slog.LevelInfo
	if next == StateOpen {
		level = slog.LevelWarn
	}
	b.log.LogAttrs(ctx, level, "circuit breaker transition",
		slog.String("provider", b.labels.Provider),
		slog.String("model", b.labels.Model),
		slog.String("from", prev.String()),
		slog.String("to", next.String()),
		slog.String("reason", reason),
		slog.Duration("cooldown", b.cooldown),
	)
}
