package resilience

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T, newStore func(Labels) BreakerStore) *BreakerRegistry {
	t.Helper()
	log, _ := newBreakerTestLogger()
	r, err := NewBreakerRegistry(testBreakerConfig(), log, newStore)
	if err != nil {
		t.Fatalf("NewBreakerRegistry() error: %v", err)
	}
	return r
}

func TestNewBreakerRegistryValidatesConfigAtBoot(t *testing.T) {
	log, _ := newBreakerTestLogger()

	bad := testBreakerConfig()
	bad.FailureThreshold = 0
	if _, err := NewBreakerRegistry(bad, log, nil); err == nil {
		t.Errorf("NewBreakerRegistry() error = nil for an invalid config, want error at boot")
	}
	if _, err := NewBreakerRegistry(testBreakerConfig(), nil, nil); err == nil {
		t.Errorf("NewBreakerRegistry() error = nil for a nil logger, want error")
	}
}

// TestRegistryReturnsTheSameBreakerForTheSameKey proves the registry is a
// registry and not a factory: two lookups of one provider+model must share a
// breaker, or each would hold half the failure evidence and neither would
// ever reach the threshold.
func TestRegistryReturnsTheSameBreakerForTheSameKey(t *testing.T) {
	r := newTestRegistry(t, nil)

	first := r.For(Labels{Provider: "groq", Model: "m"})
	second := r.For(Labels{Provider: "groq", Model: "m"})

	if first == nil || second == nil {
		t.Fatalf("For() returned nil")
	}
	if first != second {
		t.Errorf("For() returned different breakers for the same provider+model")
	}
}

// TestRegistryIsPerProviderAndModel is Step 7.4's granularity rule: one bad
// model must not take out the rest of a provider's catalogue.
func TestRegistryIsPerProviderAndModel(t *testing.T) {
	cfg := testBreakerConfig()
	log, _ := newBreakerTestLogger()
	r, err := NewBreakerRegistry(cfg, log, nil)
	if err != nil {
		t.Fatalf("NewBreakerRegistry() error: %v", err)
	}
	ctx := context.Background()

	big := r.For(Labels{Provider: "groq", Model: "gpt-4o"})
	small := r.For(Labels{Provider: "groq", Model: "gpt-4o-mini"})

	if big == small {
		t.Fatalf("two models on one provider share a breaker, want one each")
	}

	for i := 0; i < cfg.FailureThreshold; i++ {
		big.RecordFailure(ctx)
	}

	if got := big.State(); got != StateOpen {
		t.Fatalf("gpt-4o breaker = %v, want Open", got)
	}
	if got := small.State(); got != StateClosed {
		t.Errorf("gpt-4o-mini breaker = %v, want Closed — one model's failures must not trip its sibling", got)
	}
	if !small.Allow(ctx) {
		t.Errorf("gpt-4o-mini Allow() = false, want true")
	}
}

// TestRegistryForIsSafeUnderConcurrency proves the double-checked creation is
// correct: many goroutines racing on a cold key must still end up with one
// breaker between them.
func TestRegistryForIsSafeUnderConcurrency(t *testing.T) {
	r := newTestRegistry(t, nil)
	labels := Labels{Provider: "groq", Model: "m"}

	const goroutines = 50
	got := make([]*Breaker, goroutines)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			got[i] = r.For(labels)
		}(i)
	}
	start.Done()
	done.Wait()

	for i, b := range got {
		if b != got[0] {
			t.Fatalf("goroutine %d got a different breaker instance, want all identical", i)
		}
	}
}

// TestRegistryAnyOpen is what feeds Step 7.4's health integration.
func TestRegistryAnyOpen(t *testing.T) {
	cfg := testBreakerConfig()
	log, _ := newBreakerTestLogger()
	r, err := NewBreakerRegistry(cfg, log, nil)
	if err != nil {
		t.Fatalf("NewBreakerRegistry() error: %v", err)
	}
	ctx := context.Background()

	r.For(Labels{Provider: "groq", Model: "a"})
	b := r.For(Labels{Provider: "groq", Model: "b"})
	r.For(Labels{Provider: "ollama", Model: "c"})

	if r.AnyOpen("groq") {
		t.Fatalf("AnyOpen(groq) = true with everything closed, want false")
	}

	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}

	if !r.AnyOpen("groq") {
		t.Errorf("AnyOpen(groq) = false with one of its models open, want true")
	}
	if r.AnyOpen("ollama") {
		t.Errorf("AnyOpen(ollama) = true, want false — another provider's breaker must not count")
	}
	if r.AnyOpen("never-seen") {
		t.Errorf("AnyOpen(never-seen) = true, want false for a provider with no breakers")
	}
}

// TestRegistryResetClosesOnlyTheNamedProvider proves the admin reset is
// scoped: an operator clearing one provider must not silently clear another.
func TestRegistryResetClosesOnlyTheNamedProvider(t *testing.T) {
	cfg := testBreakerConfig()
	log, _ := newBreakerTestLogger()
	r, err := NewBreakerRegistry(cfg, log, nil)
	if err != nil {
		t.Fatalf("NewBreakerRegistry() error: %v", err)
	}
	ctx := context.Background()

	groqA := r.For(Labels{Provider: "groq", Model: "a"})
	groqB := r.For(Labels{Provider: "groq", Model: "b"})
	ollama := r.For(Labels{Provider: "ollama", Model: "c"})

	for _, b := range []*Breaker{groqA, groqB, ollama} {
		for i := 0; i < cfg.FailureThreshold; i++ {
			b.RecordFailure(ctx)
		}
	}

	count, err := r.Reset(ctx, "groq")
	if err != nil {
		t.Fatalf("Reset() error: %v", err)
	}
	if count != 2 {
		t.Errorf("Reset() count = %d, want 2", count)
	}

	if got := groqA.State(); got != StateClosed {
		t.Errorf("groq/a = %v after reset, want Closed", got)
	}
	if got := groqB.State(); got != StateClosed {
		t.Errorf("groq/b = %v after reset, want Closed", got)
	}
	if got := ollama.State(); got != StateOpen {
		t.Errorf("ollama/c = %v, want still Open — reset must be scoped to the named provider", got)
	}
}

// TestRegistryResetOfAnUnknownProviderReportsZero is what lets the admin
// handler tell "nothing to do" apart from "did something."
func TestRegistryResetOfAnUnknownProviderReportsZero(t *testing.T) {
	r := newTestRegistry(t, nil)

	count, err := r.Reset(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("Reset() error: %v", err)
	}
	if count != 0 {
		t.Errorf("Reset() count = %d, want 0", count)
	}
}

// TestBreakerResetAllowsTrafficImmediately is the behaviour the admin
// endpoint exists for: no waiting out the cooldown.
func TestBreakerResetAllowsTrafficImmediately(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.CooldownBase = time.Hour
	cfg.CooldownMax = time.Hour
	log, _ := newBreakerTestLogger()
	b := newTestBreaker(t, cfg, log)
	ctx := context.Background()

	openBreaker(ctx, t, b, cfg)
	if b.Allow(ctx) {
		t.Fatalf("Allow() = true while open, want false")
	}

	if err := b.Reset(ctx); err != nil {
		t.Fatalf("Reset() error: %v", err)
	}

	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v after Reset, want Closed", got)
	}
	if !b.Allow(ctx) {
		t.Errorf("Allow() = false straight after Reset, want true without waiting out the cooldown")
	}
}

// TestBreakerResetClearsTheSharedEpisodeToo proves the reset reaches the
// fleet, not just this replica.
func TestBreakerResetClearsTheSharedEpisode(t *testing.T) {
	cfg := testBreakerConfig()
	store := &fakeStore{}
	store.setState(SharedState{State: StateOpen, Reopens: 3})
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	if b.Allow(ctx) {
		t.Fatalf("Allow() = true against a shared-open breaker, want false")
	}

	if err := b.Reset(ctx); err != nil {
		t.Fatalf("Reset() error: %v", err)
	}
	if got := store.resets.Load(); got != 1 {
		t.Errorf("store.Reset called %d times, want 1", got)
	}
	if !b.Allow(ctx) {
		t.Errorf("Allow() = false after Reset, want true")
	}
}

// TestBreakerResetReportsSharedFailureButStillResetsLocally proves the
// partial-success contract the admin handler reports as a 502.
func TestBreakerResetReportsSharedFailureButStillResetsLocally(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.CooldownBase = time.Hour
	cfg.CooldownMax = time.Hour
	store := &fakeStore{resetErr: errors.New("redis is down")}
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() = %v, want Open", got)
	}

	if err := b.Reset(ctx); err == nil {
		t.Errorf("Reset() error = nil, want the store failure reported")
	}
	if got := b.State(); got != StateClosed {
		t.Errorf("State() = %v, want Closed — the local reset must happen regardless", got)
	}
}
