package budget

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

// newTestTracker skips the test rather than failing it when no Redis is
// reachable — these exercise the real Lua script against a real server, not
// a mock, so "no Redis" is an environment fact, not a code failure.
func newTestTracker(t *testing.T) (*Tracker, *redis.Client) {
	t.Helper()

	rdb := redis.NewClient(&redis.Options{Addr: testRedisAddr()})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Skipf("no Redis reachable at %s (start it with `docker compose -f deploy/docker-compose.yml up -d redis`): %v", testRedisAddr(), err)
	}

	t.Cleanup(func() { rdb.Close() })
	return NewTracker(rdb), rdb
}

// testTeamID gives every test its own period key, so tests running in
// parallel — or a prior failed run's leftover keys — never bleed into each
// other, and deletes it from Redis once the test finishes.
func testTeamID(t *testing.T, rdb *redis.Client) string {
	t.Helper()

	var b [8]byte
	rand.Read(b[:])
	id := t.Name() + "-" + hex.EncodeToString(b[:])

	t.Cleanup(func() {
		rdb.Del(context.Background(), periodKey(id, currentPeriod(time.Now())))
	})
	return id
}

func TestReserveAllowsWithinCap(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	reservation, res, err := tr.Reserve(ctx, team, 1_000_000, 300_000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !res.Allowed {
		t.Fatal("reservation within cap was denied")
	}
	if reservation == nil {
		t.Fatal("an allowed Reserve returned no handle to reconcile with")
	}
	if reservation.Reserved() != 300_000 {
		t.Errorf("Reserved() = %d, want 300000", reservation.Reserved())
	}
	if res.SpentMicros != 300_000 {
		t.Errorf("SpentMicros = %d, want 300000", res.SpentMicros)
	}
}

func TestReserveDeniesOverCap(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, res, err := tr.Reserve(ctx, team, 1_000_000, 900_000); err != nil || !res.Allowed {
		t.Fatalf("priming reservation: res=%+v err=%v", res, err)
	}

	reservation, res, err := tr.Reserve(ctx, team, 1_000_000, 200_000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Allowed {
		t.Fatal("reservation that would exceed the cap was allowed")
	}
	if reservation != nil {
		t.Fatal("a denied Reserve must return a nil handle")
	}
	if res.SpentMicros != 900_000 {
		t.Errorf("SpentMicros = %d, want 900000 (the pre-reservation total)", res.SpentMicros)
	}
}

// A denied reservation must roll back its own increment, or the counter
// would permanently overstate the team's spend by every rejected attempt —
// this is what proves reserveScript's DECRBY branch actually runs, not just
// that the decision it returns is correct.
func TestReserveDeniedRollsBackCleanly(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, res, err := tr.Reserve(ctx, team, 1_000_000, 900_000); err != nil || !res.Allowed {
		t.Fatalf("priming reservation: res=%+v err=%v", res, err)
	}

	for i := 0; i < 3; i++ {
		if _, res, err := tr.Reserve(ctx, team, 1_000_000, 200_000); err != nil || res.Allowed {
			t.Fatalf("expected denial on repeated over-cap attempt %d: res=%+v err=%v", i, res, err)
		}
	}

	// A team allowed to spend exactly up to the cap should still be able to
	// reserve the remainder — proving the three denied attempts above left
	// no residue behind.
	_, res, err := tr.Reserve(ctx, team, 1_000_000, 100_000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !res.Allowed || res.SpentMicros != 1_000_000 {
		t.Fatalf("res = %+v, want Allowed with SpentMicros=1000000", res)
	}
}

func TestReconcileRefundsUnusedEstimate(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	reservation, _, err := tr.Reserve(ctx, team, 1_000_000, 500_000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := reservation.Reconcile(ctx, 300_000); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, err := rdb.Get(ctx, periodKey(team, currentPeriod(time.Now()))).Int64()
	if err != nil {
		t.Fatalf("reading spend after reconcile: %v", err)
	}
	if got != 300_000 {
		t.Errorf("spend after reconcile = %d, want 300000", got)
	}
}

func TestReconcileDebitsUnderestimate(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	reservation, _, err := tr.Reserve(ctx, team, 1_000_000, 300_000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := reservation.Reconcile(ctx, 450_000); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, err := rdb.Get(ctx, periodKey(team, currentPeriod(time.Now()))).Int64()
	if err != nil {
		t.Fatalf("reading spend after reconcile: %v", err)
	}
	if got != 450_000 {
		t.Errorf("spend after reconcile = %d, want 450000", got)
	}
}

// A nil *Reservation is what a denied or errored Reserve returns, and the
// Step 4.2 handler defers Reconcile unconditionally the same way Step 3.3's
// TPM reconcile does — this must not panic.
func TestReconcileOnNilReservationIsSafe(t *testing.T) {
	var r *Reservation
	if err := r.Reconcile(context.Background(), 100); err != nil {
		t.Errorf("Reconcile on nil receiver returned %v, want nil", err)
	}
}

func TestReserveSetsTTL(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, res, err := tr.Reserve(ctx, team, 1_000_000, 1); err != nil || !res.Allowed {
		t.Fatalf("Reserve: res=%+v err=%v", res, err)
	}

	ttl, err := rdb.TTL(ctx, periodKey(team, currentPeriod(time.Now()))).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	// Loose bound: periodGrace alone is 48h, and the exact value depends on
	// where in the current month the test happens to run, up to just under
	// two months' worth of days plus grace.
	if ttl < 24*time.Hour || ttl > 62*24*time.Hour {
		t.Errorf("TTL = %v, want something between 1 and ~62 days", ttl)
	}
}

// This is Step 4.2's central correctness claim, the budget equivalent of
// ratelimit's TestConsumeConcurrentIsExact: many concurrent reservations
// against one team's cap must never let the confirmed total exceed it. A
// naive GET-then-conditionally-SET across two round trips would race here;
// reserveScript's atomicity is what this test actually exercises.
func TestReserveConcurrentIsExact(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)

	const budgetCap = 1_000_000
	const perAttempt = 30_000
	const attempts = 100 // 100 * 30_000 = 3_000_000, well over the cap

	var allowed int64
	var wg sync.WaitGroup
	wg.Add(attempts)

	for range attempts {
		go func() {
			defer wg.Done()
			_, res, err := tr.Reserve(context.Background(), team, budgetCap, perAttempt)
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			if res.Allowed {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	wantAllowed := int64(budgetCap / perAttempt) // 33
	if allowed != wantAllowed {
		t.Errorf("allowed %d of %d concurrent reservations against a %d cap in %d-sized steps, want exactly %d",
			allowed, attempts, budgetCap, perAttempt, wantAllowed)
	}

	got, err := rdb.Get(context.Background(), periodKey(team, currentPeriod(time.Now()))).Int64()
	if err != nil {
		t.Fatalf("reading final spend: %v", err)
	}
	if got != wantAllowed*perAttempt {
		t.Errorf("final spend = %d, want %d", got, wantAllowed*perAttempt)
	}
}

func TestReserveRejectsInvalidArguments(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	tests := map[string]struct {
		cap, estimated int64
	}{
		"zero cap":        {0, 1},
		"negative cap":    {-1, 1},
		"negative amount": {1000, -1},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := tr.Reserve(ctx, team, tc.cap, tc.estimated); err == nil {
				t.Fatal("Reserve succeeded, want a validation error")
			}
		})
	}
}

// --- Step 4.3: admin read/reset ---------------------------------------------

func TestSpentReportsZeroForUntouchedTeam(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	got, err := tr.Spent(ctx, team)
	if err != nil {
		t.Fatalf("Spent: %v", err)
	}
	if got != 0 {
		t.Errorf("Spent() = %d, want 0 for a team with no reservations yet", got)
	}
}

func TestSpentReflectsReservations(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, res, err := tr.Reserve(ctx, team, 1_000_000, 250_000); err != nil || !res.Allowed {
		t.Fatalf("Reserve: res=%+v err=%v", res, err)
	}

	got, err := tr.Spent(ctx, team)
	if err != nil {
		t.Fatalf("Spent: %v", err)
	}
	if got != 250_000 {
		t.Errorf("Spent() = %d, want 250000", got)
	}
}

func TestResetClearsSpend(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if _, res, err := tr.Reserve(ctx, team, 1_000_000, 500_000); err != nil || !res.Allowed {
		t.Fatalf("Reserve: res=%+v err=%v", res, err)
	}

	if err := tr.Reset(ctx, team); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	got, err := tr.Spent(ctx, team)
	if err != nil {
		t.Fatalf("Spent: %v", err)
	}
	if got != 0 {
		t.Errorf("Spent() after Reset = %d, want 0", got)
	}

	// A team reset back to zero should be able to spend up to the full cap
	// again immediately — proving Reset deleted the key rather than leaving
	// some other residue behind.
	_, res, err := tr.Reserve(ctx, team, 1_000_000, 1_000_000)
	if err != nil {
		t.Fatalf("Reserve after reset: %v", err)
	}
	if !res.Allowed {
		t.Fatal("a full-cap reservation was denied immediately after Reset")
	}
}

// Resetting a team that never spent anything must be a harmless no-op, not
// an error — an admin might reset defensively without checking first.
func TestResetOnUntouchedTeamIsHarmless(t *testing.T) {
	tr, rdb := newTestTracker(t)
	team := testTeamID(t, rdb)
	ctx := context.Background()

	if err := tr.Reset(ctx, team); err != nil {
		t.Fatalf("Reset on untouched team: %v", err)
	}
	got, err := tr.Spent(ctx, team)
	if err != nil {
		t.Fatalf("Spent: %v", err)
	}
	if got != 0 {
		t.Errorf("Spent() = %d, want 0", got)
	}
}

func TestCurrentPeriodFormat(t *testing.T) {
	got := currentPeriod(time.Date(2026, time.March, 15, 12, 0, 0, 0, time.UTC))
	if got != "2026-03" {
		t.Errorf("currentPeriod = %q, want %q", got, "2026-03")
	}
}

func TestTTLUntilPeriodEndCrossesYearBoundary(t *testing.T) {
	// 2026-12-15 -> next period starts 2027-01-01, plus periodGrace.
	now := time.Date(2026, time.December, 15, 0, 0, 0, 0, time.UTC)
	got := ttlUntilPeriodEnd(now)

	wantWithoutGrace := time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC).Sub(now)
	want := wantWithoutGrace + periodGrace
	if got != want {
		t.Errorf("ttlUntilPeriodEnd = %v, want %v", got, want)
	}
}
