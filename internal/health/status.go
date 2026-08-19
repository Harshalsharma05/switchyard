package health

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// Status is a provider's overall health as Step 5.3 computes it: the active
// checker's pings folded together with the passive window's real-traffic
// signal.
type Status int

const (
	StatusHealthy Status = iota
	StatusDegraded
	StatusDown
)

func (s Status) String() string {
	switch s {
	case StatusHealthy:
		return "healthy"
	case StatusDegraded:
		return "degraded"
	case StatusDown:
		return "down"
	default:
		return "unknown"
	}
}

// keyPrefix namespaces every key this package writes, so a Redis instance
// shared with rate limiting and budget state (Phases 3 and 4) never collides
// with health's own keys.
const keyPrefix = "switchyard:health"

func statusKey(providerName string) string {
	return fmt.Sprintf("%s:%s:status", keyPrefix, providerName)
}

// redisWriteTimeout bounds how long persisting a status change to Redis may
// take. This runs off the checker's ticking goroutine, not a request, so it
// does not have to be as tight as the request-path timeouts in ratelimit and
// budget — but it still must not be able to wedge that goroutine indefinitely
// against an unreachable Redis.
const redisWriteTimeout = 500 * time.Millisecond

// baselineMultiplier is fixed at the plan's literal "3x," unlike the error
// rate thresholds below, which the plan only offers as "e.g." examples.
const baselineMultiplier = 3.0

// baselineAlpha is the EMA smoothing factor for the p99 latency baseline: how
// much weight each new reading gets against the accumulated history. 0.2
// means a baseline that takes roughly the last 5 evaluations (a few minutes,
// at the default 30s check interval) to substantially follow a sustained
// change in latency — responsive enough to matter, slow enough that one noisy
// reading can't move it far.
const baselineAlpha = 0.2

// MonitorConfig holds Step 5.3's tunable thresholds. All are process-level
// settings (env vars in cmd/gateway), the same reasoning as Checker's
// interval and timeout: they describe how cautious this deployment wants to
// be, not something that differs per provider, so they don't belong in
// configs/providers.yaml.
type MonitorConfig struct {
	// DegradedErrorRate is the passive window's error rate above which a
	// provider is degraded. The plan's example is 0.10.
	DegradedErrorRate float64

	// DownErrorRate is the hard error rate threshold above which a provider is
	// down outright, regardless of the active checker. The plan's example is
	// 0.50.
	DownErrorRate float64

	// DownConsecutiveFailures is how many consecutive active-check pings must
	// fail before a provider is declared down on that signal alone.
	DownConsecutiveFailures int

	// RecoveryStreak is how many consecutive evaluations must independently
	// read healthy before a degraded or down provider is allowed back to
	// healthy. This is Step 5.3's hysteresis: without it, a provider hovering
	// right at a threshold flaps every check.
	RecoveryStreak int
}

func (c MonitorConfig) validate() error {
	switch {
	case c.DegradedErrorRate <= 0 || c.DegradedErrorRate >= 1:
		return fmt.Errorf("health monitor: degraded error rate must be in (0, 1), got %v", c.DegradedErrorRate)
	case c.DownErrorRate <= 0 || c.DownErrorRate >= 1:
		return fmt.Errorf("health monitor: down error rate must be in (0, 1), got %v", c.DownErrorRate)
	case c.DownErrorRate <= c.DegradedErrorRate:
		return fmt.Errorf("health monitor: down error rate (%v) must exceed degraded error rate (%v)", c.DownErrorRate, c.DegradedErrorRate)
	case c.DownConsecutiveFailures <= 0:
		return fmt.Errorf("health monitor: down consecutive failures must be positive, got %d", c.DownConsecutiveFailures)
	case c.RecoveryStreak <= 0:
		return fmt.Errorf("health monitor: recovery streak must be positive, got %d", c.RecoveryStreak)
	}
	return nil
}

// providerState is one provider's mutable evaluation state. It carries its
// own mutex, the same reasoning as passive.go's window: Observe can run
// concurrently for different providers (one goroutine per provider, per
// Checker), so each provider's state must not share a lock with any other's.
type providerState struct {
	mu sync.Mutex

	status Status

	consecutiveFails int
	recoveryStreak   int

	baselineP99  time.Duration
	haveBaseline bool

	// lastCheckAt is when Observe was last called for this provider — the
	// most recent active check, regardless of whether it changed anything.
	// Step 5.4's admin endpoint reports it.
	lastCheckAt time.Time

	// history is Step 5.4's retained transition log: the last 100 status
	// changes, for post-incident review.
	history *transitionLog
}

// Transition is one recorded status change: what it went from, what it went
// to, when, and why — the same reason string classify produced.
type Transition struct {
	At     time.Time
	From   Status
	To     Status
	Reason string
}

// transitionHistoryCapacity bounds how many transitions Step 5.4 retains per
// provider. The plan asks for "last 100 transitions" explicitly.
const transitionHistoryCapacity = 100

// transitionLog is a fixed-capacity ring buffer of Transitions, the same
// shape as passive.go's window but for state changes instead of request
// samples: transitions are rare compared to requests, so there is no
// time-based eviction to layer on top, only the capacity bound.
type transitionLog struct {
	mu      sync.Mutex
	entries [transitionHistoryCapacity]Transition
	next    int
	count   int
}

func (l *transitionLog) append(t Transition) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries[l.next] = t
	l.next = (l.next + 1) % transitionHistoryCapacity
	if l.count < transitionHistoryCapacity {
		l.count++
	}
}

// recent returns up to transitionHistoryCapacity entries, most recent first
// — the natural order for "what just happened" review.
func (l *transitionLog) recent() []Transition {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Transition, 0, l.count)
	for i := 0; i < l.count; i++ {
		idx := (l.next - 1 - i + transitionHistoryCapacity) % transitionHistoryCapacity
		out = append(out, l.entries[idx])
	}
	return out
}

// last returns the most recent transition, if any.
func (l *transitionLog) last() (Transition, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.count == 0 {
		return Transition{}, false
	}
	idx := (l.next - 1 + transitionHistoryCapacity) % transitionHistoryCapacity
	return l.entries[idx], true
}

// BreakerOracle reports whether a provider has any circuit breaker open —
// Step 7.4's "breaker state feeds the health status."
//
// It is declared here, by the consumer, and not as an import of
// internal/resilience: that package already imports this one for
// health.Status, so the dependency has to run this way round or the two would
// form a cycle. resilience.BreakerRegistry satisfies it structurally, and
// cmd/gateway is what connects the two.
type BreakerOracle interface {
	AnyOpen(providerName string) bool
}

// Monitor computes and stores each provider's Status. It is fed by two
// sources: Checker calls Observe once per provider per tick with the active
// ping's outcome, and Observe itself pulls the latest passive Stats from
// Recorder at that same moment — so a full evaluation happens on the
// checker's cadence (every ~30s per provider), never on the request path.
// Recording a passive sample (health.Recorder.Record, called from
// internal/proxy) stays a cheap in-memory write; nothing about a request
// triggers a status re-evaluation or a Redis round trip. That split is what
// keeps this phase compatible with the project's <10ms gateway overhead
// budget.
type Monitor struct {
	cfg         MonitorConfig
	recorder    *Recorder
	breakers    BreakerOracle
	rdb         *redis.Client
	promMetrics *telemetry.Metrics
	log         *slog.Logger

	// states is built once, in NewMonitor, and never mutated afterward — the
	// same reasoning as Recorder.windows — so concurrent Observe/Status calls
	// need no lock around the map itself, only around each entry's own
	// providerState.
	states map[string]*providerState

	// order preserves configs/providers.yaml's ordering for Snapshots, since
	// map iteration order is random and the admin listing should not
	// reshuffle on every request.
	order []string
}

// NewMonitor builds a Monitor with one state slot per provider, all starting
// Healthy — the same optimistic default Checker's immediate first ping
// relies on, so a freshly booted gateway reports every provider healthy
// before any evidence to the contrary has arrived. It then tries to restore
// each provider's last known status from Redis, best-effort, which is what
// lets status survive a gateway restart: a stale or unreachable Redis simply
// leaves the optimistic Healthy default in place.
// breakers may be nil, which simply removes that signal from classify — the
// shape every optional dependency in this project takes.
func NewMonitor(providers []provider.Provider, recorder *Recorder, breakers BreakerOracle, rdb *redis.Client, promMetrics *telemetry.Metrics, log *slog.Logger, cfg MonitorConfig) (*Monitor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	states := make(map[string]*providerState, len(providers))
	order := make([]string, 0, len(providers))
	for _, p := range providers {
		states[p.Name()] = &providerState{status: StatusHealthy, history: &transitionLog{}}
		order = append(order, p.Name())
	}

	m := &Monitor{cfg: cfg, recorder: recorder, breakers: breakers, rdb: rdb, promMetrics: promMetrics, log: log, states: states, order: order}
	m.restoreFromRedis(context.Background(), providers)
	if m.promMetrics != nil {
		for _, name := range order {
			m.promMetrics.ProviderHealth.WithLabelValues(name, "").Set(healthStatusValue(states[name].status))
		}
	}
	return m, nil
}

// restoreFromRedis seeds each provider's status from its last persisted
// value, in one round trip. Any failure — Redis unreachable, a key never
// written, a value that doesn't parse — leaves that provider at its
// constructor default (Healthy) rather than blocking startup or guessing.
func (m *Monitor) restoreFromRedis(ctx context.Context, providers []provider.Provider) {
	if m.rdb == nil || len(providers) == 0 {
		return
	}

	keys := make([]string, len(providers))
	for i, p := range providers {
		keys[i] = statusKey(p.Name())
	}

	readCtx, cancel := context.WithTimeout(ctx, redisWriteTimeout)
	defer cancel()

	vals, err := m.rdb.MGet(readCtx, keys...).Result()
	if err != nil {
		m.log.LogAttrs(ctx, slog.LevelWarn, "restoring provider health status from redis", slog.Any("error", err))
		return
	}

	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			// redis.Nil for a key that was never written comes back as a nil
			// interface here, not a string — nothing to restore.
			continue
		}
		status, ok := parseStatus(s)
		if !ok {
			continue
		}
		m.states[providers[i].Name()].status = status
	}
}

// parseStatus is String's inverse, for reading a persisted status back out of
// Redis.
func parseStatus(s string) (Status, bool) {
	switch s {
	case "healthy":
		return StatusHealthy, true
	case "degraded":
		return StatusDegraded, true
	case "down":
		return StatusDown, true
	default:
		return Status(0), false
	}
}

// Observe folds one active ping's outcome together with the provider's
// current passive window into a status decision, applying hysteresis on the
// way back to healthy. pingErr is exactly what Provider.Ping returned — nil
// on success.
func (m *Monitor) Observe(ctx context.Context, providerName string, pingErr error) {
	st := m.stateFor(providerName)
	if st == nil {
		// Not a provider this Monitor was built to track — same fail-open
		// shape as Recorder.Record's unknown-name handling. Should not happen
		// in production, since Checker, Recorder, and Monitor are all built
		// from the same registry snapshot in cmd/gateway.
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	st.lastCheckAt = time.Now()

	if pingErr != nil {
		st.consecutiveFails++
	} else {
		st.consecutiveFails = 0
	}

	stats := m.recorder.Stats(providerName)

	raw, reason := m.classify(providerName, st, stats)
	m.transition(ctx, providerName, st, raw, reason, pingErr == nil)

	// Baseline updates after classification, against the reading that was
	// just judged — updating first would let one slow window immediately
	// widen the baseline enough to stop looking slow against itself.
	m.updateBaseline(st, stats)
}

// classify computes the raw status Step 5.3's thresholds indicate right now,
// ignoring hysteresis entirely — transition is what applies that. Down
// conditions are checked first, so a provider that is both failing pings and
// showing a high error rate is reported for the more severe reason.
func (m *Monitor) classify(providerName string, st *providerState, stats Stats) (Status, string) {
	if st.consecutiveFails >= m.cfg.DownConsecutiveFailures {
		return StatusDown, "consecutive_ping_failures"
	}
	if stats.Count > 0 && stats.ErrorRate >= m.cfg.DownErrorRate {
		return StatusDown, "error_rate_hard_threshold"
	}

	// Step 7.4: an open breaker means the gateway is already refusing to send
	// this provider traffic, so reporting it healthy would contradict the
	// gateway's own behaviour. It is ranked above the error-rate checks that
	// follow — all three yield Degraded, so only the recorded reason differs,
	// and "circuit_breaker_open" is the more actionable line for an operator:
	// it names what is happening now rather than the measurement that caused
	// it. Never Down: a breaker is a decision to stop trying, not evidence
	// that the provider is unreachable, and the two shouldn't be conflated in
	// a status an operator reads during an incident.
	if m.breakers != nil && m.breakers.AnyOpen(providerName) {
		return StatusDegraded, "circuit_breaker_open"
	}

	if stats.Count > 0 && stats.ErrorRate >= m.cfg.DegradedErrorRate {
		return StatusDegraded, "error_rate_threshold"
	}
	if st.haveBaseline && stats.Count > 0 && float64(stats.P99Latency) >= baselineMultiplier*float64(st.baselineP99) {
		return StatusDegraded, "p99_above_baseline"
	}
	return StatusHealthy, "within_thresholds"
}

// transition applies raw against the provider's current stored status.
// Degrading is immediate: a single bad reading takes effect right away,
// because Step 5.2's whole premise is that active checks alone react too
// slowly. Recovering to healthy is gated by RecoveryStreak: every other
// transition (including Down straight to Degraded) takes effect immediately,
// but only landing on Healthy needs sustained good evidence — the plan's
// hysteresis requirement is specifically about "returning to healthy."
//
// pingOK is this specific tick's own active-check result — nil error, not
// just "consecutiveFails hasn't crossed the down threshold yet." A ping that
// failed but wasn't (yet) the Nth in a row still doesn't count as a clean
// signal for recovery purposes: hysteresis is only meaningful if a single bad
// reading can interrupt it, the same "any probe failure resets" convention
// Phase 7's circuit breaker uses.
func (m *Monitor) transition(ctx context.Context, providerName string, st *providerState, raw Status, reason string, pingOK bool) {
	if raw != StatusHealthy {
		st.recoveryStreak = 0
		if raw != st.status {
			m.setStatus(ctx, providerName, st, raw, reason)
		}
		return
	}

	if st.status == StatusHealthy {
		return
	}

	if !pingOK {
		st.recoveryStreak = 0
		return
	}

	st.recoveryStreak++
	if st.recoveryStreak < m.cfg.RecoveryStreak {
		return
	}
	m.setStatus(ctx, providerName, st, StatusHealthy, "recovered")
	st.recoveryStreak = 0
}

// setStatus commits a status change: logs it with the reason that triggered
// it, and persists it to Redis so other replicas can read it. It assumes
// st.mu is already held by the caller.
func (m *Monitor) setStatus(ctx context.Context, providerName string, st *providerState, newStatus Status, reason string) {
	old := st.status
	st.status = newStatus

	level := slog.LevelInfo
	if newStatus == StatusDown || (newStatus == StatusDegraded && old != StatusDegraded) {
		level = slog.LevelWarn
	}
	m.log.LogAttrs(ctx, level, "provider health transition",
		slog.String("provider", providerName),
		slog.String("from", old.String()),
		slog.String("to", newStatus.String()),
		slog.String("reason", reason),
	)

	st.history.append(Transition{At: time.Now(), From: old, To: newStatus, Reason: reason})

	if m.promMetrics != nil {
		m.promMetrics.ProviderHealth.WithLabelValues(providerName, "").Set(healthStatusValue(newStatus))
	}

	m.persist(ctx, providerName, newStatus)
}

func healthStatusValue(s Status) float64 {
	switch s {
	case StatusDown:
		return 0
	case StatusDegraded:
		return 1
	case StatusHealthy:
		return 2
	default:
		return -1
	}
}

// persist writes the new status to Redis, best-effort. A failure here is
// logged and dropped, never propagated: per CLAUDE.md, "Health checker stale
// → treat provider as healthy, rely on passive signals" — this replica's own
// in-memory status (which setStatus already committed above) is what actually
// drives its behavior. Redis is only how *other* replicas find out.
func (m *Monitor) persist(ctx context.Context, providerName string, status Status) {
	if m.rdb == nil {
		return
	}

	writeCtx, cancel := context.WithTimeout(ctx, redisWriteTimeout)
	defer cancel()

	if err := m.rdb.Set(writeCtx, statusKey(providerName), status.String(), 0).Err(); err != nil {
		m.log.LogAttrs(ctx, slog.LevelWarn, "persisting provider health status to redis",
			slog.String("provider", providerName), slog.Any("error", err))
	}
}

// updateBaseline folds the current window's p99 into the EMA baseline. The
// first observation seeds the baseline outright rather than easing into it,
// so a provider does not need several evaluations before classify has
// anything to compare against.
func (m *Monitor) updateBaseline(st *providerState, stats Stats) {
	if stats.Count == 0 {
		return
	}
	if !st.haveBaseline {
		st.baselineP99 = stats.P99Latency
		st.haveBaseline = true
		return
	}
	st.baselineP99 = time.Duration(baselineAlpha*float64(stats.P99Latency) + (1-baselineAlpha)*float64(st.baselineP99))
}

// Status returns the provider's current status, or StatusHealthy for a name
// this Monitor does not track — the same optimistic default an unrecognized
// name gets everywhere else in this package.
func (m *Monitor) Status(providerName string) Status {
	st := m.stateFor(providerName)
	if st == nil {
		return StatusHealthy
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status
}

func (m *Monitor) stateFor(providerName string) *providerState {
	return m.states[providerName]
}

// ProviderHealth is one provider's full Step 5.4 picture: everything
// GET /admin/providers/health reports.
type ProviderHealth struct {
	Provider       string
	Status         Status
	ErrorRate      float64
	P99Latency     time.Duration
	LastCheckAt    time.Time
	LastTransition *Transition
	History        []Transition
}

// Snapshot returns providerName's current ProviderHealth. The bool return is
// false only for a name this Monitor was never built to track — the admin
// handler uses it to tell "unknown provider" apart from "known, but nothing
// has happened to it yet."
func (m *Monitor) Snapshot(providerName string) (ProviderHealth, bool) {
	st := m.stateFor(providerName)
	if st == nil {
		return ProviderHealth{}, false
	}

	stats := m.recorder.Stats(providerName)

	st.mu.Lock()
	defer st.mu.Unlock()

	var lastTransition *Transition
	if t, ok := st.history.last(); ok {
		lastTransition = &t
	}

	return ProviderHealth{
		Provider:       providerName,
		Status:         st.status,
		ErrorRate:      stats.ErrorRate,
		P99Latency:     stats.P99Latency,
		LastCheckAt:    st.lastCheckAt,
		LastTransition: lastTransition,
		History:        st.history.recent(),
	}, true
}

// Snapshots returns every tracked provider's ProviderHealth, in
// configs/providers.yaml's order.
func (m *Monitor) Snapshots() []ProviderHealth {
	out := make([]ProviderHealth, 0, len(m.order))
	for _, name := range m.order {
		if snap, ok := m.Snapshot(name); ok {
			out = append(out, snap)
		}
	}
	return out
}

// AllDown reports whether every tracked provider is Down. Step 5.4's /readyz
// treats only this as "the gateway cannot serve traffic" — a single bad
// provider is a routing problem for Phase 6's fallback chains to handle, not
// a reason to fail a readiness probe and pull the whole gateway from a load
// balancer.
func (m *Monitor) AllDown() bool {
	if len(m.order) == 0 {
		return false
	}
	for _, name := range m.order {
		if m.Status(name) != StatusDown {
			return false
		}
	}
	return true
}
