package resilience

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// BreakerRegistry owns one Breaker per provider+model — Step 7.4's
// granularity rule.
//
// Per provider+model rather than per provider because a provider is not one
// dependency, it is several: a model can be rate limited, deprecated, or
// simply overloaded on its own while everything else that provider serves is
// fine. A breaker keyed only by provider would let one struggling model trip
// the circuit for its healthy siblings, turning a partial outage into a total
// one — the opposite of what a breaker is for. The cost of the finer key is
// more breakers, each of which needs its own failures before it trips; that
// is the right trade, because the evidence that gpt-4o is failing genuinely
// says nothing about gpt-4o-mini.
//
// Breakers are created on first use rather than enumerated from
// configs/providers.yaml at boot. A breaker's whole state is "nothing has
// gone wrong yet" until something does, so an absent one and a freshly built
// one are indistinguishable — and lazy creation means a hot reload adding a
// model needs no registry rebuild, and this package needs no dependency on
// internal/config.
type BreakerRegistry struct {
	mu       sync.RWMutex
	breakers map[Labels]*Breaker

	cfg BreakerConfig
	log *slog.Logger

	// newStore builds the shared store for a breaker, or is nil for a
	// registry whose breakers are purely local. It is a func rather than a
	// store because each breaker needs its own, scoped to its own keys.
	newStore func(Labels) BreakerStore
}

// NewBreakerRegistry returns a registry whose breakers are local to this
// process. newStore may be nil; pass one (see NewRedisBreakerRegistry) to
// share verdicts across replicas.
func NewBreakerRegistry(cfg BreakerConfig, log *slog.Logger, newStore func(Labels) BreakerStore) (*BreakerRegistry, error) {
	// Validated once here rather than on every lazy construction below, so a
	// bad breaker config fails at boot like every other config in this
	// project instead of at the first failing request.
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		return nil, errors.New("circuit breaker registry: logger must not be nil")
	}

	return &BreakerRegistry{
		breakers: make(map[Labels]*Breaker),
		cfg:      cfg,
		log:      log,
		newStore: newStore,
	}, nil
}

// For returns the breaker guarding one provider+model, creating it if this is
// the first time anything has asked.
func (r *BreakerRegistry) For(labels Labels) *Breaker {
	// The read lock covers the overwhelmingly common case — the breaker
	// already exists — so concurrent requests for different models do not
	// serialize on a write lock they do not need.
	r.mu.RLock()
	b, ok := r.breakers[labels]
	r.mu.RUnlock()
	if ok {
		return b
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-checked under the write lock: another goroutine may have created it
	// between the read unlock and here, and two breakers for one
	// provider+model would each hold half the failure evidence.
	if b, ok := r.breakers[labels]; ok {
		return b
	}

	var store BreakerStore
	if r.newStore != nil {
		store = r.newStore(labels)
	}

	// NewSharedBreaker only fails on a config the constructor already
	// validated and a nil logger it already rejected, so an error here is
	// unreachable. Returning it would force every call site — including the
	// request path — to handle a case that cannot happen; logging and falling
	// back to an always-allow breaker keeps the signature honest about that.
	b, err := NewSharedBreaker(r.cfg, r.log, labels, store)
	if err != nil {
		r.log.Error("building circuit breaker",
			slog.String("provider", labels.Provider),
			slog.String("model", labels.Model),
			slog.Any("error", err))
		return nil
	}

	r.breakers[labels] = b
	return b
}

// AnyOpen reports whether any of a provider's models currently has an open
// breaker. This is what feeds Step 7.4's health integration: a provider with
// an open circuit is at least degraded, whatever its ping and error rate say.
//
// It reads each breaker's mirrored state rather than its store, so it costs
// no I/O and is safe to call from health.Monitor's evaluation.
func (r *BreakerRegistry) AnyOpen(providerName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for labels, b := range r.breakers {
		if labels.Provider == providerName && b.State() == StateOpen {
			return true
		}
	}
	return false
}

// Reset closes every breaker belonging to one provider and reports how many
// it touched, for Step 7.4's admin endpoint. The count is what tells an
// operator whether the name they typed matched anything.
//
// Errors from individual breakers are joined rather than returned on the
// first one: a partial reset is worse than a reported one, so every breaker
// gets its attempt regardless of what happened to the last.
func (r *BreakerRegistry) Reset(ctx context.Context, providerName string) (int, error) {
	r.mu.RLock()
	targets := make([]*Breaker, 0, len(r.breakers))
	for labels, b := range r.breakers {
		if labels.Provider == providerName {
			targets = append(targets, b)
		}
	}
	r.mu.RUnlock()

	// Reset is called outside the registry lock: it does Redis I/O, and
	// holding the map lock across it would block every concurrent For.
	var failed error
	for _, b := range targets {
		if err := b.Reset(ctx); err != nil {
			failed = errors.Join(failed, err)
		}
	}
	return len(targets), failed
}

// Snapshots returns a side-effect-free view of every breaker this registry has
// built, keyed by provider+model — the read Step 7.4's health endpoint and
// Part 2's Live Ops breaker visualisation report.
func (r *BreakerRegistry) Snapshots() map[Labels]BreakerSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[Labels]BreakerSnapshot, len(r.breakers))
	for labels, b := range r.breakers {
		out[labels] = b.Inspect()
	}
	return out
}
