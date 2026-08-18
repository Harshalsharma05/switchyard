// Breaker implements Phase 7 Step 7.1's circuit breaker state machine —
// Closed, Open, and HalfOpen. Per CLAUDE.md this file is hand-written by the
// repo owner; it exists here only because the owner explicitly asked to
// override that rule for this session.
//
// Step 7.1 is the state machine: three states and the four transitions
// between them, all logged. Step 7.2 adds the half-open concurrency guard —
// exactly one in-flight probe, everyone else rejected as if Open. Step 7.3
// backs it with Redis so replicas agree (see BreakerStore below and
// breaker_store.go). Step 7.4 wires one Breaker per provider+model into the
// fallback chain from Step 6.2.
//
// Step 7.2's decision, single probe rather than percentage-based: while the
// breaker is HalfOpen it admits one request at a time, not a fraction of
// traffic. A percentage recovers faster, because it re-establishes real
// throughput the moment the provider is healthy again instead of ramping one
// request at a time. It is also the riskier half of the trade — the provider
// that just failed is by definition fragile, and a percentage of production
// traffic against a fragile provider is how a recovering dependency gets
// knocked straight back over. Single probe caps the blast radius of a wrong
// guess at exactly one request, and it makes "what does HalfOpen do" a
// sentence rather than a tuning exercise. The cost is bounded and visible:
// with SuccessThreshold probes needed to close, full recovery takes that many
// sequential requests.
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

	// ProbeTimeout is how long a claimed HalfOpen probe may stay outstanding
	// before the breaker assumes its caller is never coming back and admits
	// another one.
	//
	// This exists because Step 7.2's guard is claim-then-report: Allow hands
	// out the single probe slot and RecordSuccess/RecordFailure hand it back.
	// A caller that takes the slot and then neither succeeds nor fails —
	// killed mid-request, or a wiring bug in a future call site — would
	// otherwise wedge the breaker in HalfOpen rejecting every request
	// forever. That is precisely the failure CLAUDE.md's first design
	// constraint forbids: the gateway itself becoming the reason requests
	// fail. Size it above the provider's configured request timeout, so a
	// slow-but-alive probe is never mistaken for an abandoned one.
	ProbeTimeout time.Duration

	// StateCacheTTL is how long a Breaker may serve the shared state it last
	// read from its BreakerStore before going back to Redis for a fresh one.
	// A Breaker with no store ignores it.
	//
	// This is Step 7.3's central trade-off, and the reason the plan asks for
	// the cache at all. Reading Redis inside Allow on every request would put
	// a network round trip in front of every call the gateway proxies,
	// against a total overhead budget of 10ms at p95 — and would spend it
	// re-reading a value that changes a handful of times a day. Caching it
	// means replicas can disagree for up to this long: one may still be
	// admitting traffic a moment after another has opened the circuit.
	//
	// That inconsistency is bounded, brief, and self-correcting; the latency
	// it is traded against is none of those things. A few hundred
	// milliseconds is the useful range — long enough that the read amortises
	// to nothing at any real request rate, short enough that a stale verdict
	// costs a handful of requests rather than a visible outage.
	StateCacheTTL time.Duration
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
	case c.ProbeTimeout <= 0:
		return fmt.Errorf("circuit breaker: probe timeout must be positive, got %s", c.ProbeTimeout)
	case c.StateCacheTTL <= 0:
		return fmt.Errorf("circuit breaker: state cache TTL must be positive, got %s", c.StateCacheTTL)
	}
	return nil
}

// BreakerStore is the shared state a Breaker needs to agree with its replicas
// — Step 7.3. It is defined here, next to its only consumer, per CLAUDE.md's
// "interfaces are defined by the consumer"; breaker_store.go holds the Redis
// implementation, and tests supply fakes.
//
// The Step 7.3 decision this interface encodes: the shared thing is the
// *verdict*, not the *evidence*. Each replica counts failures against its own
// in-memory window, and the first to cross its threshold publishes an open
// episode that every other replica honours. There is deliberately no method
// for contributing a failure to a pooled fleet-wide count.
//
// What that buys: a healthy, closed breaker makes zero Redis calls per
// request beyond a cached read that amortises to nothing, so the normal path
// stays inside the 10ms overhead budget. Redis is touched only at
// transitions and while probing — precisely the rare events.
//
// What it costs, and the honest answer to an interviewer who asks: the
// threshold means "N failures seen by one replica," not "N across the fleet."
// A single replica with a local network fault can open a breaker for
// everyone. That is the more trigger-happy direction to be wrong in, which is
// the right direction for a breaker — it protects a struggling provider
// sooner, and a wrong verdict costs one cooldown before a probe corrects it.
type BreakerStore interface {
	// Load returns the fleet's current view.
	Load(ctx context.Context) (SharedState, error)

	// Open publishes an open episode of the given cooldown, tagged with the
	// consecutive-episode count the exponential backoff derives from.
	Open(ctx context.Context, cooldown time.Duration, reopens int) error

	// RecordProbeSuccess adds one success to the shared streak and closes the
	// breaker if that reaches threshold, returning the resulting state.
	RecordProbeSuccess(ctx context.Context, threshold int) (SharedState, error)

	// ClaimProbe takes the fleet-wide probe slot, returning false when
	// another replica holds it.
	ClaimProbe(ctx context.Context, ttl time.Duration) (bool, error)

	// Reset clears the shared episode outright, for Step 7.4's manual admin
	// intervention.
	Reset(ctx context.Context) error
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

	// probeInFlight is Step 7.2's guard: true while a HalfOpen probe has been
	// handed out and not yet reported back. probeStartedAt is when, so an
	// abandoned probe can be reclaimed after ProbeTimeout.
	//
	// A plain bool rather than the atomic.CompareAndSwap the plan offers as an
	// alternative: Allow already holds b.mu to read and mutate state, so a CAS
	// would be a second synchronization mechanism guarding a decision the
	// mutex covers anyway — and two mechanisms over one piece of state is how
	// you get a race that only shows up under load. The mutex is held for a
	// few field reads and never across a provider call, so it is not a hot-path
	// concern.
	probeInFlight  bool
	probeStartedAt time.Time

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

	// --- Step 7.3: shared state ---------------------------------------------

	// store is the fleet-wide view, or nil for a purely local breaker. Every
	// field above stays in use either way: with a store the local state
	// becomes a mirror of the shared verdict, but the failure window remains
	// this replica's own evidence and is never shared.
	store BreakerStore

	// unpublished is set when this replica opened the breaker but could not
	// tell the store. While it is set the breaker ignores shared state and
	// runs on its own machine — see publishOpen for why adopting the fleet's
	// stale "closed" in that situation would be worse than not coordinating
	// at all. It clears on the next successful publish or local close, so the
	// replica rejoins the fleet at the next transition.
	unpublished bool

	// cacheMu guards the three fields below. It is deliberately not b.mu:
	// refreshing the cache does Redis I/O, and doing that under the state
	// mutex would block every concurrent RecordSuccess and RecordFailure
	// behind a network round trip. Concurrent refreshers still serialize on
	// this one, but only for the length of a single ~1ms read once per
	// StateCacheTTL, which is a far smaller window than the alternative.
	cacheMu  sync.Mutex
	cached   SharedState
	cachedOK bool

	// cachedAt is stamped on every refresh attempt, successful or not. Doing
	// it on failure too is what stops an unreachable Redis from being retried
	// on every single request — each attempt would otherwise cost a full
	// redisTimeout and turn a Redis outage into a latency outage, which is
	// exactly the failure mode CLAUDE.md's first design constraint exists to
	// prevent. It also naturally rate-limits the warning log to one line per
	// TTL instead of one per request.
	cachedAt time.Time
}

// NewBreaker validates cfg and returns a purely local Breaker starting Closed
// — the same optimistic default health.NewMonitor uses, so a freshly resolved
// provider+model is assumed healthy until it proves otherwise.
//
// A local breaker is correct for a single-replica deployment and is what the
// tests use. Use NewSharedBreaker when replicas must agree.
func NewBreaker(cfg BreakerConfig, log *slog.Logger, labels Labels) (*Breaker, error) {
	return NewSharedBreaker(cfg, log, labels, nil)
}

// NewSharedBreaker returns a Breaker whose verdict is shared with every other
// replica through store — Step 7.3. A nil store gives exactly NewBreaker.
func NewSharedBreaker(cfg BreakerConfig, log *slog.Logger, labels Labels, store BreakerStore) (*Breaker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		return nil, fmt.Errorf("circuit breaker: logger must not be nil")
	}
	return &Breaker{
		cfg:      cfg,
		log:      log,
		labels:   labels,
		state:    StateClosed,
		cooldown: cfg.CooldownBase,
		store:    store,
	}, nil
}

// State returns the breaker's current state without side effects — for
// admin/health reporting, where reading the state must not itself trigger an
// Open -> HalfOpen transition the way Allow does.
//
// With a store this is the mirror of the last shared state Allow read, not a
// fresh read of its own. That keeps it cheap and side-effect-free at the cost
// of being at most StateCacheTTL behind, which is the same staleness every
// request-path decision already accepts.
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
// Allow is not a pure read. A true return in HalfOpen claims the single probe
// slot, and the caller owes the breaker exactly one RecordSuccess or
// RecordFailure for it — see ProbeTimeout for what happens when that promise
// is broken. Callers that only want to look at the state without claiming
// anything use State instead.
//
// The bool deliberately does not distinguish "rejected because Open" from
// "rejected because another probe holds the slot." Step 7.4's chain
// resolution does the same thing in both cases — skip this candidate, try the
// next — and collapsing them here keeps that call site from branching on a
// distinction it has no use for.
func (b *Breaker) Allow(ctx context.Context) bool {
	b.mu.Lock()
	shared := b.usesStore()
	b.mu.Unlock()

	if shared {
		if s, ok := b.sharedState(ctx); ok {
			return b.allowShared(ctx, s)
		}
		// Redis is unreachable or answering nonsense. Fall through to this
		// replica's own state machine rather than failing the request: a
		// breaker that cannot reach its store still knows what it has seen
		// with its own eyes, and CLAUDE.md's first constraint says the
		// gateway must never be the reason a request fails. The cost is that
		// replicas stop agreeing until Redis returns — degraded coordination,
		// not degraded service.
	}
	return b.allowLocal(ctx)
}

// allowLocal is the single-replica decision: the Step 7.1/7.2 state machine
// exactly as it stands, with no shared state involved.
func (b *Breaker) allowLocal(ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen && time.Since(b.openedAt) >= b.cooldown {
		b.setState(ctx, StateHalfOpen, "cooldown_elapsed")
	}

	switch b.state {
	case StateClosed:
		return true
	case StateHalfOpen:
		return b.claimProbe(ctx)
	default: // StateOpen
		return false
	}
}

// allowShared decides against the fleet's verdict, mirroring it into this
// replica's local state on the way through.
//
// The shared state simply wins. There is no merge with the local machine's
// opinion, and that is the point of Step 7.3: if one replica has opened this
// breaker, every replica honours it, and when one replica's probes close it,
// every replica resumes. The local state machine is downgraded to a mirror
// plus a failure window — the one thing that stays genuinely per-replica.
//
// Note that no Open -> HalfOpen check happens here. The store derives that
// transition from a stored deadline against Redis's own clock (see
// loadScript), so it has already been decided by the time this reads it, and
// no replica has to own a timer or can miss the moment it fires.
func (b *Breaker) allowShared(ctx context.Context, shared SharedState) bool {
	b.mu.Lock()
	if shared.State != b.state {
		b.setState(ctx, shared.State, "adopted_shared_state")
	}
	b.reopens = shared.Reopens
	b.successStreak = shared.Successes
	b.mu.Unlock()

	switch shared.State {
	case StateClosed:
		return true
	case StateHalfOpen:
		return b.claimSharedProbe(ctx)
	default: // StateOpen
		return false
	}
}

// sharedState returns the fleet's view, from cache when it is fresh enough
// and from the store otherwise. The bool is false when the store could not be
// read, which the caller treats as "fall back to local."
//
// The Redis read happens under cacheMu and explicitly not under b.mu — see
// the field comment on cacheMu for why holding the state mutex across network
// I/O would be a mistake.
func (b *Breaker) sharedState(ctx context.Context) (SharedState, bool) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()

	if !b.cachedAt.IsZero() && time.Since(b.cachedAt) < b.cfg.StateCacheTTL {
		return b.cached, b.cachedOK
	}

	shared, err := b.store.Load(ctx)
	b.cachedAt = time.Now()
	if err != nil {
		b.cached, b.cachedOK = SharedState{}, false
		b.log.LogAttrs(ctx, slog.LevelWarn, "reading shared circuit breaker state, falling back to local",
			slog.String("provider", b.labels.Provider),
			slog.String("model", b.labels.Model),
			slog.Any("error", err),
		)
		return SharedState{}, false
	}

	b.cached, b.cachedOK = shared, true
	return shared, true
}

// installShared overwrites the cache with a state this replica just caused,
// so its own write is visible to it immediately rather than after the TTL. A
// replica that has just opened a breaker should not spend the next
// StateCacheTTL still admitting traffic on a stale cached "closed."
func (b *Breaker) installShared(shared SharedState) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()

	b.cached, b.cachedOK, b.cachedAt = shared, true, time.Now()
}

// invalidateShared forces the next read to go back to the store. Used when
// this replica knows the shared state changed but not what it changed to.
func (b *Breaker) invalidateShared() {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()

	b.cachedAt = time.Time{}
}

// claimSharedProbe takes the fleet-wide probe slot. A store error is treated
// as "not allowed" rather than falling back to the local guard: the failure
// mode being defended against here is several replicas probing a fragile
// provider at once, and quietly degrading to a per-replica guard would permit
// exactly that. Refusing costs one request, which the caller then routes to
// the next candidate in its fallback chain.
func (b *Breaker) claimSharedProbe(ctx context.Context) bool {
	ok, err := b.store.ClaimProbe(ctx, b.cfg.ProbeTimeout)
	if err != nil {
		b.log.LogAttrs(ctx, slog.LevelWarn, "claiming shared circuit breaker probe slot",
			slog.String("provider", b.labels.Provider),
			slog.String("model", b.labels.Model),
			slog.Any("error", err),
		)
		return false
	}
	return ok
}

// claimProbe implements Step 7.2's "exactly one in-flight probe": it hands
// out the slot if it is free, and reports the request rejected — as if Open —
// if it is not. Assumes b.mu is held.
func (b *Breaker) claimProbe(ctx context.Context) bool {
	if b.probeInFlight {
		if time.Since(b.probeStartedAt) < b.cfg.ProbeTimeout {
			return false
		}
		// Past ProbeTimeout the slot is presumed abandoned and reclaimed.
		// Logged at Warn rather than reclaimed silently: in a correctly wired
		// gateway every probe reports back, so this line means a caller is
		// dropping its outcome and the breaker is running on a safety net
		// instead of on real signal.
		b.log.LogAttrs(ctx, slog.LevelWarn, "reclaiming abandoned circuit breaker probe",
			slog.String("provider", b.labels.Provider),
			slog.String("model", b.labels.Model),
			slog.Duration("outstanding_for", time.Since(b.probeStartedAt)),
		)
	}

	b.probeInFlight = true
	b.probeStartedAt = time.Now()
	return true
}

// releaseProbe hands the probe slot back. It is unconditional rather than
// guarded on state, because every path that reports an outcome ends a probe
// if one was running — including the RecordFailure that reopens the breaker.
// Assumes b.mu is held.
func (b *Breaker) releaseProbe() {
	b.probeInFlight = false
}

// RecordSuccess reports that a call this breaker allowed succeeded. In
// HalfOpen it also returns the probe slot, so the next request may probe.
//
// A report that arrives after its probe was reclaimed as abandoned still
// counts, and will release whichever probe is current instead of its own.
// That misattribution is accepted rather than tracked with per-probe tokens:
// the outcome being reported is a real call against a real provider, so
// counting it is honest, and it can only happen when a caller has already
// exceeded ProbeTimeout — a condition claimProbe logs loudly.
// Both Record methods follow the same shape: a small helper owns b.mu and
// decides what happened, and any Redis I/O the decision implies runs after
// that lock is released. Doing the round trip inside the locked section would
// stall every concurrent caller behind the network for as long as it took,
// which is the same reason sharedState refreshes under cacheMu instead.
func (b *Breaker) RecordSuccess(ctx context.Context) {
	if b.recordSuccessLocally(ctx) {
		b.publishProbeSuccess(ctx)
	}
}

// recordSuccessLocally applies a success to this replica's own state and
// reports whether the shared store still needs telling.
func (b *Breaker) recordSuccessLocally(ctx context.Context) (publish bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.releaseProbe()

	if b.state != StateHalfOpen {
		// A Closed success needs no bookkeeping: the failure window only
		// tracks failures, and it ages out on its own via pruneBefore. This
		// early return is also what keeps the hot path free of Redis — the
		// overwhelmingly common case, a success on a closed breaker, does no
		// I/O at all.
		return false
	}

	// With a store, the streak lives in Redis: the increment and its
	// threshold test have to be one atomic operation, or two replicas could
	// each read the same count and each write back the same next value,
	// closing a breaker on half the probes it should have taken.
	if b.usesStore() {
		return true
	}

	b.successStreak++
	if b.successStreak >= b.cfg.SuccessThreshold {
		b.reopens = 0
		b.cooldown = b.cfg.CooldownBase
		b.setState(ctx, StateClosed, "probe_success_streak")
	}
	return false
}

// publishProbeSuccess sends one probe success to the shared store and mirrors
// whatever state that produced. A failure here leaves the breaker HalfOpen,
// which is the safe direction to be wrong in: the next probe simply tries
// again, whereas assuming the increment landed could close a breaker on a
// success the fleet never counted.
func (b *Breaker) publishProbeSuccess(ctx context.Context) {
	shared, err := b.store.RecordProbeSuccess(ctx, b.cfg.SuccessThreshold)
	if err != nil {
		b.log.LogAttrs(ctx, slog.LevelWarn, "publishing circuit breaker probe success",
			slog.String("provider", b.labels.Provider),
			slog.String("model", b.labels.Model),
			slog.Any("error", err),
		)
		b.invalidateShared()
		return
	}

	b.installShared(shared)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.successStreak = shared.Successes
	if shared.State != b.state {
		b.setState(ctx, shared.State, "probe_success_streak")
	}
	if shared.State == StateClosed {
		b.reopens = 0
		b.cooldown = b.cfg.CooldownBase
		b.unpublished = false
	}
}

// RecordFailure reports that a call this breaker allowed failed. In HalfOpen
// it also returns the probe slot — though the reopen below immediately makes
// that moot, since Open rejects everyone regardless.
func (b *Breaker) RecordFailure(ctx context.Context) {
	cooldown, reopens, opened := b.recordFailureLocally(ctx)
	if opened {
		b.publishOpen(ctx, cooldown, reopens)
	}
}

// recordFailureLocally applies a failure to this replica's own state,
// reporting the episode it opened (if any) for publishing outside the lock.
func (b *Breaker) recordFailureLocally(ctx context.Context) (cooldown time.Duration, reopens int, opened bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.releaseProbe()

	switch b.state {
	case StateHalfOpen:
		// Any probe failure reopens immediately — a single bad reading is
		// enough evidence the provider isn't ready, the same "one failure
		// interrupts recovery" convention health.Monitor uses for its own
		// hysteresis.
		b.open(ctx, "probe_failed")

	case StateClosed:
		// The failure window is this replica's own evidence and is never
		// shared — see BreakerStore for why the verdict travels but the
		// evidence does not.
		now := time.Now()
		b.failures = pruneBefore(append(b.failures, now), now.Add(-b.cfg.Window))
		if len(b.failures) < b.cfg.FailureThreshold {
			return 0, 0, false
		}
		b.open(ctx, "failure_threshold_exceeded")

	case StateOpen:
		// Already open; nothing this failure needs to change.
		return 0, 0, false
	}

	return b.cooldown, b.reopens, b.usesStore()
}

// publishOpen tells the fleet about an episode this replica just opened.
//
// When the write fails, the breaker marks itself unpublished and falls back
// to running purely locally until it manages to publish again. Without that,
// a failed write would be actively harmful rather than merely unhelpful: this
// replica's local Open would be overwritten within one StateCacheTTL by a
// shared state that still reads "closed", and because opening clears the
// failure window, the breaker would need a whole fresh threshold of failures
// to notice the outage a second time. Distrusting the fleet's answer while
// unable to contribute to it is the conservative reading.
func (b *Breaker) publishOpen(ctx context.Context, cooldown time.Duration, reopens int) {
	if err := b.store.Open(ctx, cooldown, reopens); err != nil {
		b.log.LogAttrs(ctx, slog.LevelWarn, "publishing open circuit breaker episode, falling back to local",
			slog.String("provider", b.labels.Provider),
			slog.String("model", b.labels.Model),
			slog.Any("error", err),
		)
		b.mu.Lock()
		b.unpublished = true
		b.mu.Unlock()
		b.invalidateShared()
		return
	}

	b.installShared(SharedState{State: StateOpen, Reopens: reopens})

	b.mu.Lock()
	defer b.mu.Unlock()
	b.unpublished = false
}

// open commits the Closed -> Open or HalfOpen -> Open transition, growing the
// cooldown exponentially with each consecutive episode and resetting the
// failure window and success streak so the next state starts clean.
//
// With a store, reopens is seeded from the shared count that allowShared
// mirrored in, so the exponential cooldown grows across the fleet's episodes
// rather than restarting at one per replica.
func (b *Breaker) open(ctx context.Context, reason string) {
	b.reopens++
	b.cooldown = nextCooldown(b.cfg, b.reopens)
	b.successStreak = 0
	b.failures = nil
	b.setState(ctx, StateOpen, reason)
}

// usesStore reports whether the shared path is currently in play: a store is
// configured and this replica is not stuck running locally after a failed
// publish. Assumes b.mu is held.
func (b *Breaker) usesStore() bool {
	return b.store != nil && !b.unpublished
}

// Reset forces the breaker back to Closed and clears everything that got it
// out of Closed — Step 7.4's manual intervention, for an operator who knows a
// provider has recovered and does not want to wait out a cooldown.
//
// The shared reset is attempted first and its failure is returned, but the
// local reset happens either way. An operator who asked for a reset should
// get one on at least the replica they reached, and reporting the error lets
// them know the rest of the fleet may still be holding the old episode.
func (b *Breaker) Reset(ctx context.Context) error {
	var storeErr error
	if b.store != nil {
		storeErr = b.store.Reset(ctx)
		b.installShared(SharedState{State: StateClosed})
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = nil
	b.successStreak = 0
	b.reopens = 0
	b.cooldown = b.cfg.CooldownBase
	b.probeInFlight = false
	b.unpublished = false
	if b.state != StateClosed {
		b.setState(ctx, StateClosed, "manual_reset")
	}

	return storeErr
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

	// Every transition invalidates any outstanding probe: the state it was
	// probing on behalf of no longer exists. Notably this is what guarantees
	// a freshly entered HalfOpen starts with its slot free, so the caller
	// that triggered the cooldown transition in Allow is the one that gets to
	// claim it.
	b.probeInFlight = false

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
