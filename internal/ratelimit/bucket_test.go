package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedisAddr lets CI or a differently-configured machine point these
// tests at a Redis that isn't on localhost, without touching the test code.
func testRedisAddr() string {
	if addr := os.Getenv("TEST_REDIS_ADDR"); addr != "" {
		return addr
	}
	return "localhost:6379"
}

// newTestLimiter skips the test rather than failing it when no Redis is
// reachable — these exercise the real Lua script against a real server, not
// a mock, so "no Redis" is an environment fact, not a code failure.
func newTestLimiter(t *testing.T) (*Limiter, *redis.Client) {
	t.Helper()

	rdb := redis.NewClient(&redis.Options{Addr: testRedisAddr()})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Skipf("no Redis reachable at %s (start it with `docker compose -f deploy/docker-compose.yml up -d redis`): %v", testRedisAddr(), err)
	}

	t.Cleanup(func() { rdb.Close() })
	return NewLimiter(rdb), rdb
}

// testTeamID gives every test its own bucket keys, so tests running in
// parallel — or a prior failed run's leftover keys — never bleed into each
// other, and deletes them from Redis once the test finishes.
func testTeamID(t *testing.T, rdb *redis.Client) string {
	t.Helper()

	var b [8]byte
	rand.Read(b[:])
	id := t.Name() + "-" + hex.EncodeToString(b[:])

	t.Cleanup(func() {
		rdb.Del(context.Background(), bucketKey(id, RPM), bucketKey(id, TPM))
	})
	return id
}

func TestConsumeAllowsWithinCapacity(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	res, err := l.Consume(ctx, team, RPM, 60, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !res.Allowed {
		t.Fatal("first request against a fresh bucket was denied")
	}
	if res.Remaining != 59 {
		t.Errorf("remaining = %v, want 59", res.Remaining)
	}
	if res.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 on an allowed request", res.RetryAfter)
	}
}

func TestConsumeDeniesOverCapacity(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	// Drain the bucket in one shot rather than one at a time — the
	// concurrency test below already covers exactness under many small
	// calls; this one is about the deny path itself.
	if _, err := l.Consume(ctx, team, RPM, 5, 5, time.Minute); err != nil {
		t.Fatalf("draining Consume: %v", err)
	}

	res, err := l.Consume(ctx, team, RPM, 5, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if res.Allowed {
		t.Fatal("request against an exhausted bucket was allowed")
	}
	if res.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive wait", res.RetryAfter)
	}

	// capacity=5 per minute means a refill rate of 5/60 tokens/sec; the
	// deficit is exactly 1 token, so retry-after should land at 12s.
	want := 12 * time.Second
	if diff := res.RetryAfter - want; diff > 200*time.Millisecond || diff < -200*time.Millisecond {
		t.Errorf("RetryAfter = %v, want ~%v", res.RetryAfter, want)
	}
}

// A denied request must not consume anything — otherwise a caller retrying
// after a 429 would find the bucket even further in the hole than the
// rejection already implied. Note this does not assert exact equality
// between the two calls: real time elapses between them, and the bucket
// keeps refilling (fractionally) regardless of whether a request is
// allowed — refill is unconditional, only consumption is gated. What must
// never happen is the balance going *down* on a call that took nothing.
func TestConsumeDeniedRequestTakesNothing(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, err := l.Consume(ctx, team, RPM, 5, 5, time.Minute); err != nil {
		t.Fatalf("draining Consume: %v", err)
	}

	before, err := l.Consume(ctx, team, RPM, 5, 3, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if before.Allowed {
		t.Fatal("expected this request to be denied")
	}

	after, err := l.Consume(ctx, team, RPM, 5, 3, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if after.Remaining < before.Remaining {
		t.Errorf("remaining dropped from %v to %v across two denied calls; a denial must not consume", before.Remaining, after.Remaining)
	}
}

// Lazy refill: nothing ticks in the background, so this proves elapsed time
// alone — computed from the stored timestamp — is what brings the bucket
// back, not a goroutine anyone forgot to start.
func TestConsumeRefillsOverElapsedTime(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	// capacity chosen high so the refill rate (capacity/60) is fast enough
	// to observe within a short sleep, keeping this test quick.
	const capacity = 6000 // refill = 100 tokens/sec

	if _, err := l.Consume(ctx, team, TPM, capacity, capacity, time.Minute); err != nil {
		t.Fatalf("draining Consume: %v", err)
	}

	denied, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if denied.Allowed {
		t.Fatal("bucket should be exhausted immediately after draining it")
	}

	time.Sleep(100 * time.Millisecond) // ~10 tokens at 100/sec

	allowed, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !allowed.Allowed {
		t.Fatal("bucket did not refill after waiting")
	}
}

// RPM and TPM are separate buckets even for the same team: exhausting one
// must not touch the other.
func TestConsumeBucketsAreIndependentPerType(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, err := l.Consume(ctx, team, RPM, 1, 1, time.Minute); err != nil {
		t.Fatalf("Consume(RPM): %v", err)
	}
	rpmDenied, err := l.Consume(ctx, team, RPM, 1, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume(RPM): %v", err)
	}
	if rpmDenied.Allowed {
		t.Fatal("RPM bucket should be exhausted")
	}

	tpmAllowed, err := l.Consume(ctx, team, TPM, 1000, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume(TPM): %v", err)
	}
	if !tpmAllowed.Allowed {
		t.Fatal("exhausting the RPM bucket must not affect the TPM bucket")
	}
}

// The bucket's key must not outlive an idle team by more than its TTL, or a
// gateway that has ever seen a team leaks one Redis key per team forever.
func TestConsumeSetsTTL(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, err := l.Consume(ctx, team, RPM, 60, 1, 500*time.Millisecond); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	if exists, err := rdb.Exists(ctx, bucketKey(team, RPM)).Result(); err != nil || exists != 1 {
		t.Fatalf("key should exist immediately after Consume (exists=%d, err=%v)", exists, err)
	}

	time.Sleep(700 * time.Millisecond)

	if exists, err := rdb.Exists(ctx, bucketKey(team, RPM)).Result(); err != nil || exists != 0 {
		t.Fatalf("key should have expired (exists=%d, err=%v)", exists, err)
	}
}

// This is Step 3.2's central correctness claim: many concurrent callers
// sharing one bucket must never let through more than capacity allows. A
// naive GET-modify-SET across two round trips would race here; the Lua
// script's atomicity is what this test actually exercises.
func TestConsumeConcurrentIsExact(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)

	const capacity = 50
	const attempts = 200

	var allowed int64
	var wg sync.WaitGroup
	wg.Add(attempts)

	for range attempts {
		go func() {
			defer wg.Done()
			res, err := l.Consume(context.Background(), team, RPM, capacity, 1, time.Minute)
			if err != nil {
				t.Errorf("Consume: %v", err)
				return
			}
			if res.Allowed {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if allowed != capacity {
		t.Errorf("allowed %d of %d concurrent requests against a %d-capacity bucket, want exactly %d",
			allowed, attempts, capacity, capacity)
	}
}

func TestConsumeRejectsInvalidArguments(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	tests := map[string]struct {
		capacity, amount int
		ttl              time.Duration
	}{
		"zero capacity":     {0, 1, time.Minute},
		"negative capacity": {-1, 1, time.Minute},
		"zero amount":       {60, 0, time.Minute},
		"negative amount":   {60, -1, time.Minute},
		"zero ttl":          {60, 1, 0},
		"negative ttl":      {60, 1, -time.Second},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := l.Consume(ctx, team, RPM, tc.capacity, tc.amount, tc.ttl); err == nil {
				t.Fatal("Consume succeeded, want a validation error")
			}
		})
	}
}

// --- reservations (Step 3.3) -------------------------------------------------

func TestReserveConsumesUpFront(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	reservation, res, err := l.Reserve(ctx, team, TPM, 1000, 300, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !res.Allowed {
		t.Fatal("reservation within capacity was denied")
	}
	if reservation == nil {
		t.Fatal("an allowed Reserve returned no handle to reconcile with")
	}
	if reservation.Reserved() != 300 {
		t.Errorf("Reserved() = %d, want 300", reservation.Reserved())
	}
	if res.Remaining != 700 {
		t.Errorf("remaining = %v, want 700", res.Remaining)
	}
}

// A denied reservation took nothing, so it has nothing to give back — and
// its nil handle must survive a deferred Reconcile without a nil check.
func TestReserveDeniedReturnsNilHandleThatReconcilesSafely(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, _, err := l.Reserve(ctx, team, TPM, 100, 100, time.Minute); err != nil {
		t.Fatalf("draining Reserve: %v", err)
	}

	reservation, res, err := l.Reserve(ctx, team, TPM, 100, 50, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Allowed {
		t.Fatal("reservation beyond capacity was allowed")
	}
	if reservation != nil {
		t.Fatal("a denied Reserve returned a non-nil handle")
	}

	if err := reservation.Reconcile(ctx, 0); err != nil {
		t.Errorf("Reconcile on a nil reservation: %v, want a no-op", err)
	}
}

// The core of Step 3.3: reserve a ceiling, then hand back what the response
// did not use.
func TestReconcileReturnsUnusedTokens(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	const capacity = 1000

	// Reserve 300 (say, 50 prompt + 250 max_tokens); the response really
	// used 80.
	reservation, res, err := l.Reserve(ctx, team, TPM, capacity, 300, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Remaining != 700 {
		t.Fatalf("remaining after reserve = %v, want 700", res.Remaining)
	}

	if err := reservation.Reconcile(ctx, 80); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Only the 80 actually used should still be gone: 1000 - 80 = 920.
	after, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	// The probe consumed 1 more, so expect ~919. Tolerance covers the
	// fractional refill that accrues between calls.
	if after.Remaining < 918 || after.Remaining > 921 {
		t.Errorf("remaining = %v, want ~919 (220 of the 300 reservation returned)", after.Remaining)
	}
}

// An estimate that came in too low must be debited, not forgiven.
func TestReconcileDebitsOverage(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	const capacity = 1000

	reservation, _, err := l.Reserve(ctx, team, TPM, capacity, 100, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The response used 400 against a 100 reservation.
	if err := reservation.Reconcile(ctx, 400); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if after.Remaining < 598 || after.Remaining > 601 {
		t.Errorf("remaining = %v, want ~599 (the full 400 debited, not just the 100 reserved)", after.Remaining)
	}
}

// Returning tokens must never lift a bucket above its ceiling, or repeated
// over-reservation would become burst credit.
func TestReconcileCapsAtCapacity(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	const capacity = 500

	reservation, _, err := l.Reserve(ctx, team, TPM, capacity, 100, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Used nothing, so all 100 come back — but the bucket was already near
	// full, so it must land at capacity, not above it.
	if err := reservation.Reconcile(ctx, 0); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if after.Remaining > capacity-1 {
		t.Errorf("remaining = %v, want no more than %d: a refund must not exceed capacity", after.Remaining, capacity-1)
	}
}

// A large overage may drive the balance below zero. That is intended: the
// team waits out the deficit rather than having it written off.
func TestReconcileMayGoNegative(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	const capacity = 100

	reservation, _, err := l.Reserve(ctx, team, TPM, capacity, 100, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := reservation.Reconcile(ctx, 250); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	denied, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if denied.Allowed {
		t.Fatal("a team in deficit was allowed through")
	}
	if denied.Remaining >= 0 {
		t.Errorf("remaining = %v, want a negative balance after a 150-token overage", denied.Remaining)
	}
	// And the deficit must translate into a longer wait than a merely empty
	// bucket would produce.
	if denied.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive wait", denied.RetryAfter)
	}
}

func TestReconcileExactUsageChangesNothing(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	const capacity = 1000

	reservation, res, err := l.Reserve(ctx, team, TPM, capacity, 200, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := reservation.Reconcile(ctx, 200); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if after.Remaining > res.Remaining {
		t.Errorf("remaining rose from %v to %v; reconciling an exact estimate must not credit anything", res.Remaining, after.Remaining)
	}
}

// Reconciling after the key's TTL elapsed must not fail or corrupt state —
// a bucket with no stored state is by definition full.
func TestReconcileAfterKeyExpiry(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	const capacity = 1000

	reservation, _, err := l.Reserve(ctx, team, TPM, capacity, 300, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	if err := reservation.Reconcile(ctx, 50); err != nil {
		t.Fatalf("Reconcile after expiry: %v", err)
	}

	after, err := l.Consume(ctx, team, TPM, capacity, 1, time.Minute)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if after.Remaining > capacity-1 {
		t.Errorf("remaining = %v, want no more than %d", after.Remaining, capacity-1)
	}
}

func TestReconcileRejectsNegativeActual(t *testing.T) {
	l, rdb := newTestLimiter(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	reservation, _, err := l.Reserve(ctx, team, TPM, 1000, 100, time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := reservation.Reconcile(ctx, -1); err == nil {
		t.Fatal("Reconcile accepted a negative usage figure")
	}
}

// bucketKey's exact schema is part of Step 3.2's spec, and Phase 4's admin
// API and Phase 9's metrics will both need to construct or match it.
func TestBucketKeySchema(t *testing.T) {
	got := bucketKey("acme", RPM)
	want := "switchyard:rl:acme:rpm"
	if got != want {
		t.Errorf("bucketKey = %q, want %q", got, want)
	}
}
