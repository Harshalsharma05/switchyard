package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// --- fake store -------------------------------------------------------------

// fakeStore is an in-memory BreakerStore for the tests that are about the
// Breaker's *use* of a store — caching, fail-open, publish failure — rather
// than about Redis itself. The Redis tests below use the real thing.
type fakeStore struct {
	mu sync.Mutex

	state     SharedState
	probeHeld bool

	loadErr  error
	openErr  error
	claimErr error
	resetErr error

	loads   atomic.Int64
	opens   atomic.Int64
	claims  atomic.Int64
	streaks atomic.Int64
	resets  atomic.Int64
}

func (f *fakeStore) Load(context.Context) (SharedState, error) {
	f.loads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return SharedState{}, f.loadErr
	}
	return f.state, nil
}

func (f *fakeStore) Open(_ context.Context, _ time.Duration, reopens int) error {
	f.opens.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return f.openErr
	}
	f.state = SharedState{State: StateOpen, Reopens: reopens}
	f.probeHeld = false
	return nil
}

func (f *fakeStore) RecordProbeSuccess(_ context.Context, threshold int) (SharedState, error) {
	f.streaks.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()

	f.probeHeld = false
	f.state.Successes++
	if f.state.Successes >= threshold {
		f.state = SharedState{State: StateClosed}
	} else {
		f.state.State = StateHalfOpen
	}
	return f.state, nil
}

func (f *fakeStore) ClaimProbe(context.Context, time.Duration) (bool, error) {
	f.claims.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if f.probeHeld {
		return false, nil
	}
	f.probeHeld = true
	return true, nil
}

func (f *fakeStore) Reset(context.Context) error {
	f.resets.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resetErr != nil {
		return f.resetErr
	}
	f.state = SharedState{State: StateClosed}
	f.probeHeld = false
	return nil
}

func (f *fakeStore) setState(s SharedState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func newSharedTestBreaker(t *testing.T, cfg BreakerConfig, store BreakerStore) *Breaker {
	t.Helper()
	log, _ := newBreakerTestLogger()
	b, err := NewSharedBreaker(cfg, log, Labels{Provider: "p", Model: "m"}, store)
	if err != nil {
		t.Fatalf("NewSharedBreaker() error: %v", err)
	}
	return b
}

// --- Breaker <-> store interaction ------------------------------------------

// TestSharedStateIsCachedForTTL is Step 7.3's whole reason for existing: the
// shared state is read once per StateCacheTTL, not once per request.
func TestSharedStateIsCachedForTTL(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.StateCacheTTL = 100 * time.Millisecond
	store := &fakeStore{}
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if !b.Allow(ctx) {
			t.Fatalf("Allow() = false on call %d, want true (shared state is closed)", i+1)
		}
	}
	if got := store.loads.Load(); got != 1 {
		t.Fatalf("store.Load called %d times across 20 requests, want 1 — the cache is not being used", got)
	}

	time.Sleep(cfg.StateCacheTTL + 20*time.Millisecond)
	b.Allow(ctx)

	if got := store.loads.Load(); got != 2 {
		t.Fatalf("store.Load called %d times after the TTL expired, want 2 — the cache is never refreshing", got)
	}
}

// TestSharedOpenIsAdoptedByAReplicaThatNeverFailed is "replicas agree" at the
// unit level: this breaker has seen no failures of its own, but the fleet says
// the circuit is open, so it rejects.
func TestSharedOpenIsAdoptedByAReplicaThatNeverFailed(t *testing.T) {
	cfg := testBreakerConfig()
	store := &fakeStore{}
	store.setState(SharedState{State: StateOpen, Reopens: 1})
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	if b.Allow(ctx) {
		t.Fatalf("Allow() = true, want false — the fleet reports this breaker open")
	}
	if got := b.State(); got != StateOpen {
		t.Errorf("State() = %v, want Open mirrored from the shared state", got)
	}
}

// TestSharedClosedIsAdoptedByAReplicaThatOpened is the same rule in the other
// direction, and the one that makes recovery fleet-wide: another replica's
// probes closed the circuit, so this replica resumes even though it was the
// one that opened it.
func TestSharedClosedIsAdoptedByAReplicaThatOpened(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.StateCacheTTL = 20 * time.Millisecond
	store := &fakeStore{}
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	b.Allow(ctx) // prime the cache
	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}
	if b.Allow(ctx) {
		t.Fatalf("Allow() = true right after opening, want false")
	}

	// Stand in for another replica's probes having closed the circuit.
	store.setState(SharedState{State: StateClosed})
	time.Sleep(cfg.StateCacheTTL + 10*time.Millisecond)

	if !b.Allow(ctx) {
		t.Fatalf("Allow() = false, want true — the fleet has closed this breaker")
	}
	if got := b.State(); got != StateClosed {
		t.Errorf("State() = %v, want Closed adopted from the shared state", got)
	}
}

// TestOpeningPublishesToTheStore proves a local threshold crossing becomes a
// fleet-wide episode, tagged with the episode count the cooldown grows from.
func TestOpeningPublishesToTheStore(t *testing.T) {
	cfg := testBreakerConfig()
	store := &fakeStore{}
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}

	if got := store.opens.Load(); got != 1 {
		t.Fatalf("store.Open called %d times, want 1", got)
	}
	store.mu.Lock()
	got := store.state
	store.mu.Unlock()
	if got.State != StateOpen || got.Reopens != 1 {
		t.Errorf("shared state = %+v, want Open with Reopens 1", got)
	}
}

// TestClosedPathDoesNoStoreWrites is the hot-path guarantee behind Step 7.3's
// design: a healthy closed breaker costs one cached read and nothing else. If
// this regresses, every proxied request grows a Redis write.
func TestClosedPathDoesNoStoreWrites(t *testing.T) {
	store := &fakeStore{}
	b := newSharedTestBreaker(t, testBreakerConfig(), store)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		b.Allow(ctx)
		b.RecordSuccess(ctx)
	}

	if got := store.opens.Load(); got != 0 {
		t.Errorf("store.Open called %d times on a healthy closed breaker, want 0", got)
	}
	if got := store.streaks.Load(); got != 0 {
		t.Errorf("store.RecordProbeSuccess called %d times on a healthy closed breaker, want 0", got)
	}
	if got := store.claims.Load(); got != 0 {
		t.Errorf("store.ClaimProbe called %d times on a healthy closed breaker, want 0", got)
	}
	if got := store.loads.Load(); got > 2 {
		t.Errorf("store.Load called %d times across 50 requests, want the cache to hold it near 1", got)
	}
}

// TestStoreReadFailureFallsBackToLocal proves the Redis-down behaviour: the
// breaker keeps working on its own state machine rather than failing requests,
// per CLAUDE.md's "the gateway must never be the reason a request fails."
func TestStoreReadFailureFallsBackToLocal(t *testing.T) {
	cfg := testBreakerConfig()
	store := &fakeStore{loadErr: errors.New("redis is down")}
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	if !b.Allow(ctx) {
		t.Fatalf("Allow() = false with an unreachable store, want true — a closed local breaker should still serve")
	}

	// The local machine must still be able to open on its own evidence.
	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}
	if b.Allow(ctx) {
		t.Fatalf("Allow() = true after the local threshold was crossed, want false")
	}
}

// TestStoreReadFailureIsNotRetriedEveryRequest is the detail that keeps a
// Redis outage from becoming a latency outage: the cache timestamp is stamped
// on failed reads too, so a dead store is retried once per TTL rather than
// once per request, each attempt otherwise costing a full timeout.
func TestStoreReadFailureIsNotRetriedEveryRequest(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.StateCacheTTL = time.Second // longer than this test runs
	store := &fakeStore{loadErr: errors.New("redis is down")}
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		b.Allow(ctx)
	}

	if got := store.loads.Load(); got != 1 {
		t.Fatalf("store.Load called %d times against a failing store across 100 requests, want 1", got)
	}
}

// TestFailedPublishKeepsTheBreakerLocallyOpen is the subtle one. A replica
// that opens but cannot tell the fleet must not then adopt the fleet's stale
// "closed" — opening clears the failure window, so it would need a whole
// fresh threshold of failures to notice the same outage twice.
func TestFailedPublishKeepsTheBreakerLocallyOpen(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.StateCacheTTL = 10 * time.Millisecond
	// Long enough that the sleep below cannot let the local breaker reach its
	// own cooldown — the point under test is which state wins, not the
	// Open -> HalfOpen timing.
	cfg.CooldownBase = time.Minute
	cfg.CooldownMax = time.Minute
	store := &fakeStore{openErr: errors.New("redis write failed")}
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}

	// The store still reports closed, because the publish never landed.
	store.setState(SharedState{State: StateClosed})
	time.Sleep(cfg.StateCacheTTL + 10*time.Millisecond)

	if b.Allow(ctx) {
		t.Fatalf("Allow() = true, want false — a replica that could not publish must trust its own state")
	}
	if got := b.State(); got != StateOpen {
		t.Errorf("State() = %v, want Open", got)
	}
}

// TestSharedProbeSlotIsClaimedThroughTheStore proves the HalfOpen guard is
// delegated to the store once one exists, which is what makes it fleet-wide
// rather than per-replica.
func TestSharedProbeSlotIsClaimedThroughTheStore(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.StateCacheTTL = time.Millisecond
	store := &fakeStore{}
	store.setState(SharedState{State: StateHalfOpen, Reopens: 1})
	b := newSharedTestBreaker(t, cfg, store)
	ctx := context.Background()

	if !b.Allow(ctx) {
		t.Fatalf("Allow() = false, want true — the probe slot was free")
	}
	if got := store.claims.Load(); got != 1 {
		t.Fatalf("store.ClaimProbe called %d times, want 1", got)
	}

	time.Sleep(2 * time.Millisecond) // let the cache expire so we re-read HalfOpen
	if b.Allow(ctx) {
		t.Fatalf("Allow() = true while the shared probe slot is held, want false")
	}
}

// TestSharedProbeClaimErrorRejects proves a store failure during probe
// claiming rejects rather than silently degrading to the local guard —
// degrading there would let every replica probe at once, which is the exact
// stampede the guard exists to prevent.
func TestSharedProbeClaimErrorRejects(t *testing.T) {
	cfg := testBreakerConfig()
	store := &fakeStore{claimErr: errors.New("redis is down")}
	store.setState(SharedState{State: StateHalfOpen, Reopens: 1})
	b := newSharedTestBreaker(t, cfg, store)

	if b.Allow(context.Background()) {
		t.Fatalf("Allow() = true when the probe claim errored, want false")
	}
}

// --- Redis-backed store ------------------------------------------------------

// newTestRedis connects to a real Redis and skips rather than fails when none
// is reachable — the same convention internal/health, internal/ratelimit, and
// internal/budget use, since the Lua scripts are the thing under test and a
// mock would only test the mock.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()

	addr := "localhost:6379"
	rdb := redis.NewClient(&redis.Options{Addr: addr})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Skipf("no Redis reachable at %s: %v", addr, err)
	}

	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// uniqueLabels keeps concurrently running tests (and reruns) off each other's
// keys, the same trick health's Redis tests use for provider names.
func uniqueLabels(t *testing.T) Labels {
	t.Helper()
	return Labels{
		Provider: "test",
		Model:    fmt.Sprintf("%s-%s", t.Name(), time.Now().Format("150405.000000000")),
	}
}

func newTestRedisStore(t *testing.T) (*RedisStore, Labels) {
	t.Helper()
	rdb := newTestRedis(t)
	labels := uniqueLabels(t)
	t.Cleanup(func() {
		rdb.Del(context.Background(), stateKey(labels), probeKey(labels))
	})
	return NewRedisStore(rdb, labels), labels
}

// TestRedisStoreLoadOfAnUnknownBreakerIsClosed proves the absence of a key
// reads as closed — the optimistic default, and the reason closing is just a
// DEL with no "closed" record to leave behind.
func TestRedisStoreLoadOfAnUnknownBreakerIsClosed(t *testing.T) {
	store, _ := newTestRedisStore(t)

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.State != StateClosed {
		t.Errorf("Load() = %+v, want Closed for a breaker with no stored episode", got)
	}
}

// TestRedisStoreDerivesOpenThenHalfOpenFromTheDeadline proves loadScript's
// central trick: one stored deadline yields Open before it and HalfOpen after,
// with no timer and nobody owning the transition.
func TestRedisStoreDerivesOpenThenHalfOpenFromTheDeadline(t *testing.T) {
	store, _ := newTestRedisStore(t)
	ctx := context.Background()

	cooldown := 60 * time.Millisecond
	if err := store.Open(ctx, cooldown, 1); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.State != StateOpen {
		t.Fatalf("Load() = %+v immediately after Open, want Open", got)
	}
	if got.Reopens != 1 {
		t.Errorf("Reopens = %d, want 1", got.Reopens)
	}

	time.Sleep(cooldown + 20*time.Millisecond)

	got, err = store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.State != StateHalfOpen {
		t.Errorf("Load() = %+v after the cooldown elapsed, want HalfOpen", got)
	}
}

// TestRedisStoreProbeSlotIsExclusive proves SET NX gives exactly one winner.
func TestRedisStoreProbeSlotIsExclusive(t *testing.T) {
	store, _ := newTestRedisStore(t)
	ctx := context.Background()

	ok, err := store.ClaimProbe(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ClaimProbe() error: %v", err)
	}
	if !ok {
		t.Fatalf("ClaimProbe() = false on a free slot, want true")
	}

	ok, err = store.ClaimProbe(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ClaimProbe() error: %v", err)
	}
	if ok {
		t.Errorf("ClaimProbe() = true while the slot is held, want false")
	}
}

// TestRedisStoreProbeSlotExpires proves the TTL releases an abandoned slot,
// so a replica dying mid-probe cannot wedge the fleet.
func TestRedisStoreProbeSlotExpires(t *testing.T) {
	store, _ := newTestRedisStore(t)
	ctx := context.Background()

	if ok, err := store.ClaimProbe(ctx, 50*time.Millisecond); err != nil || !ok {
		t.Fatalf("ClaimProbe() = %v, %v; want true, nil", ok, err)
	}

	time.Sleep(80 * time.Millisecond)

	ok, err := store.ClaimProbe(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ClaimProbe() error: %v", err)
	}
	if !ok {
		t.Errorf("ClaimProbe() = false after the slot's TTL expired, want true")
	}
}

// TestRedisStoreProbeSuccessClosesAtThreshold proves the shared streak counts
// up and the breaker closes exactly at the threshold — and that closing frees
// the probe slot.
func TestRedisStoreProbeSuccessClosesAtThreshold(t *testing.T) {
	store, _ := newTestRedisStore(t)
	ctx := context.Background()

	if err := store.Open(ctx, time.Millisecond, 1); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	got, err := store.RecordProbeSuccess(ctx, 2)
	if err != nil {
		t.Fatalf("RecordProbeSuccess() error: %v", err)
	}
	if got.State != StateHalfOpen || got.Successes != 1 {
		t.Fatalf("after 1 success = %+v, want HalfOpen with 1 success", got)
	}

	got, err = store.RecordProbeSuccess(ctx, 2)
	if err != nil {
		t.Fatalf("RecordProbeSuccess() error: %v", err)
	}
	if got.State != StateClosed {
		t.Fatalf("after 2 successes = %+v, want Closed", got)
	}

	after, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if after.State != StateClosed {
		t.Errorf("Load() = %+v after closing, want Closed", after)
	}
}

// TestRedisStoreOpenInvalidatesAnInFlightProbe proves a new episode drops the
// probe lock: the probe it belonged to was testing a recovery this failure
// just disproved, and leaving it held would stall the next episode's first
// probe until its TTL ran out.
func TestRedisStoreOpenInvalidatesAnInFlightProbe(t *testing.T) {
	store, _ := newTestRedisStore(t)
	ctx := context.Background()

	if ok, err := store.ClaimProbe(ctx, time.Minute); err != nil || !ok {
		t.Fatalf("ClaimProbe() = %v, %v; want true, nil", ok, err)
	}
	if err := store.Open(ctx, time.Minute, 2); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	ok, err := store.ClaimProbe(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ClaimProbe() error: %v", err)
	}
	if !ok {
		t.Errorf("ClaimProbe() = false after a new episode opened, want true")
	}
}

// --- two replicas ------------------------------------------------------------

// newReplicaPair builds two Breakers over separate RedisStores pointing at the
// same provider+model keys — the shape of two gateway processes sharing one
// Redis, without needing two processes.
func newReplicaPair(t *testing.T, cfg BreakerConfig) (*Breaker, *Breaker) {
	t.Helper()

	rdb := newTestRedis(t)
	labels := uniqueLabels(t)
	t.Cleanup(func() {
		rdb.Del(context.Background(), stateKey(labels), probeKey(labels))
	})

	build := func() *Breaker {
		log, _ := newBreakerTestLogger()
		b, err := NewSharedBreaker(cfg, log, labels, NewRedisStore(rdb, labels))
		if err != nil {
			t.Fatalf("NewSharedBreaker() error: %v", err)
		}
		return b
	}
	return build(), build()
}

// TestReplicasAgreeOnAnOpenBreaker is the Step 7.3 headline: replica A trips
// on its own failures, and replica B — which has seen none — stops sending
// traffic too.
func TestReplicasAgreeOnAnOpenBreaker(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.CooldownBase = time.Minute // stay open for the whole test
	cfg.CooldownMax = time.Minute
	cfg.StateCacheTTL = time.Millisecond
	a, b := newReplicaPair(t, cfg)
	ctx := context.Background()

	if !b.Allow(ctx) {
		t.Fatalf("replica B: Allow() = false before anything failed, want true")
	}

	for i := 0; i < cfg.FailureThreshold; i++ {
		a.RecordFailure(ctx)
	}

	time.Sleep(5 * time.Millisecond) // let B's cache expire

	if b.Allow(ctx) {
		t.Fatalf("replica B: Allow() = true after replica A opened the breaker, want false")
	}
	if got := b.State(); got != StateOpen {
		t.Errorf("replica B: State() = %v, want Open", got)
	}
}

// TestReplicasShareOneProbeSlot proves the guard is genuinely fleet-wide: two
// replicas both seeing HalfOpen produce one probe between them, not one each.
func TestReplicasShareOneProbeSlot(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.CooldownBase = 40 * time.Millisecond
	cfg.CooldownMax = 40 * time.Millisecond
	cfg.StateCacheTTL = time.Millisecond
	a, b := newReplicaPair(t, cfg)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		a.RecordFailure(ctx)
	}
	time.Sleep(cfg.CooldownBase + 20*time.Millisecond)

	admitted := 0
	if a.Allow(ctx) {
		admitted++
	}
	if b.Allow(ctx) {
		admitted++
	}

	if admitted != 1 {
		t.Fatalf("%d of 2 replicas admitted a probe, want exactly 1", admitted)
	}
}

// TestProbeStreakSpansReplicas is why SuccessThreshold has to live in Redis.
// With a fleet-wide probe slot, consecutive probes land on whichever replica
// wins the race — so a per-replica streak would sit at one on each of them and
// the breaker would never close.
func TestProbeStreakSpansReplicas(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.SuccessThreshold = 2
	cfg.CooldownBase = 30 * time.Millisecond
	cfg.CooldownMax = 30 * time.Millisecond
	cfg.StateCacheTTL = time.Millisecond
	a, b := newReplicaPair(t, cfg)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		a.RecordFailure(ctx)
	}
	time.Sleep(cfg.CooldownBase + 20*time.Millisecond)

	// One probe each, on different replicas.
	if !a.Allow(ctx) {
		t.Fatalf("replica A: Allow() = false, want the first probe")
	}
	a.RecordSuccess(ctx)

	time.Sleep(5 * time.Millisecond)
	if !b.Allow(ctx) {
		t.Fatalf("replica B: Allow() = false, want the second probe after A released the slot")
	}
	b.RecordSuccess(ctx)

	time.Sleep(5 * time.Millisecond)
	if !a.Allow(ctx) {
		t.Errorf("replica A: Allow() = false after two successful probes across replicas, want the breaker closed")
	}
	if got := a.State(); got != StateClosed {
		t.Errorf("replica A: State() = %v, want Closed", got)
	}
}

// TestReplicasShareTheExponentialCooldown proves the episode count is fleet
// state: a reopen from replica B continues the backoff replica A started
// rather than restarting it at CooldownBase.
func TestReplicasShareTheExponentialCooldown(t *testing.T) {
	cfg := testBreakerConfig()
	cfg.CooldownBase = 30 * time.Millisecond
	cfg.CooldownMax = time.Minute
	cfg.StateCacheTTL = time.Millisecond
	a, b := newReplicaPair(t, cfg)
	ctx := context.Background()

	for i := 0; i < cfg.FailureThreshold; i++ {
		a.RecordFailure(ctx)
	}
	time.Sleep(cfg.CooldownBase + 20*time.Millisecond)

	// B picks up the probe, fails it, and should open episode 2 — not a
	// fresh episode 1 with the base cooldown.
	if !b.Allow(ctx) {
		t.Fatalf("replica B: Allow() = false, want the probe")
	}
	b.RecordFailure(ctx)

	b.mu.Lock()
	got := b.cooldown
	b.mu.Unlock()

	want := 2 * cfg.CooldownBase
	if got != want {
		t.Fatalf("replica B cooldown = %v, want %v — the episode count should have carried across replicas", got, want)
	}
}
