// Package ratelimit enforces per-team request and token budgets against
// Redis using the token bucket algorithm.
//
// Note for whoever reviews this: PART1_PLAN.md and CLAUDE.md both mark this
// file "write it yourself" — it's the piece an interviewer is most likely to
// ask about on a whiteboard, and reading someone else's is a different skill
// from deriving the refill math under your own steam. This one is
// Claude-written at the repo owner's explicit request for this session, as a
// one-time exception to that rule, not a standing change to it.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// LimitType names which of a team's two independent buckets a call targets.
// See Step 3.2: requests-per-minute and tokens-per-minute are tracked
// separately, because a request can be within its RPM budget and still be
// too expensive in tokens, or vice versa.
type LimitType string

const (
	RPM LimitType = "rpm"
	TPM LimitType = "tpm"
)

// keyPrefix namespaces every key this package writes, so a Redis instance
// shared with budget and health state (Phases 4 and 5) never collides with
// rate limiting's own keys.
const keyPrefix = "switchyard:rl"

// bucketKey builds the schema Step 3.2 specifies:
// switchyard:rl:{team_id}:{rpm|tpm}.
func bucketKey(teamID string, kind LimitType) string {
	return fmt.Sprintf("%s:%s:%s", keyPrefix, teamID, kind)
}

// Result is the outcome of one Consume call.
type Result struct {
	// Allowed reports whether the requested amount was taken from the bucket.
	// If false, nothing was consumed — Remaining and RetryAfter describe the
	// bucket's state as of this call, not as of some prior successful one.
	Allowed bool

	// Remaining is the bucket's token count immediately after this call,
	// which Step 3.4's 429 response reports as X-RateLimit-Remaining.
	Remaining float64

	// RetryAfter is how long the caller should wait before the bucket would
	// hold enough tokens for this same request to succeed. It is zero when
	// Allowed is true.
	RetryAfter time.Duration
}

// consumeScript is the token bucket's entire state machine: read the
// bucket's stored tokens and last-refill time, compute how much has accrued
// since then, decide whether the request fits, and write the new state
// back — all inside one Lua script, which Redis runs to completion without
// interleaving any other command. That atomicity is the reason this can't be
// a plain GET, compute-in-Go, SET: two gateway replicas doing read-modify-write
// as separate round trips could both read the same starting balance and both
// allow a request that only one of them should have.
//
// Refill is lazy: nothing ticks in the background. Every call recomputes the
// balance from elapsed = now - stored_ts, so a bucket nobody has touched in
// an hour needs no catching up — the next call simply sees a large elapsed
// and refills to capacity in one step. This is also what makes the algorithm
// replica-safe with no coordination beyond Redis itself: there is no timer
// state anywhere to drift or duplicate.
//
// The script reads time from Redis's own clock (TIME) rather than trusting
// the caller's wall clock. If gateway replicas ran on hosts with clocks even
// a few hundred milliseconds apart, each would compute a different elapsed
// time from its own clock against the same stored timestamp, silently
// skewing how much every replica thinks the bucket has refilled. Redis is
// the single source of truth every replica already round-trips to, so its
// clock is the one both replicas agree on for free.
const consumeScript = `
local key             = KEYS[1]
local capacity        = tonumber(ARGV[1])
local refill_per_sec  = tonumber(ARGV[2])
local requested        = tonumber(ARGV[3])
local ttl_ms           = tonumber(ARGV[4])

local time = redis.call('TIME')
local now = tonumber(time[1]) + (tonumber(time[2]) / 1000000)

local stored = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(stored[1])
local ts = tonumber(stored[2])

-- No prior state: a bucket starts full, exactly as if it had been sitting
-- untouched since before the dawn of time and had long since refilled.
if tokens == nil then
  tokens = capacity
  ts = now
end

local elapsed = now - ts
if elapsed > 0 then
  tokens = math.min(capacity, tokens + elapsed * refill_per_sec)
end

local allowed = 0
local retry_after = 0

if tokens >= requested then
  tokens = tokens - requested
  allowed = 1
else
  local deficit = requested - tokens
  retry_after = deficit / refill_per_sec
end

redis.call('HMSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('PEXPIRE', key, ttl_ms)

return {allowed, tostring(tokens), tostring(retry_after)}
`

// settleScript applies a signed adjustment to a bucket, and is how a
// reservation is reconciled once real usage is known.
//
// It refills first, exactly as consumeScript does, so an adjustment never
// silently discards the tokens that accrued while the request was in flight.
//
// Two asymmetries are deliberate:
//
//   - The result is capped at capacity, so returning an unused reservation
//     can never inflate a bucket past its ceiling. Without this a team making
//     many over-reserved requests would accumulate credit and burst beyond
//     its configured limit.
//   - There is no floor. A negative delta may drive the balance below zero,
//     which is what happens when a response used more tokens than were
//     reserved. Clamping at zero would forgive the overage; letting it go
//     negative makes the team wait out the deficit, and the refill math and
//     retry-after calculation both handle negative balances unchanged.
const settleScript = `
local key             = KEYS[1]
local capacity        = tonumber(ARGV[1])
local refill_per_sec  = tonumber(ARGV[2])
local delta           = tonumber(ARGV[3])
local ttl_ms          = tonumber(ARGV[4])

local time = redis.call('TIME')
local now = tonumber(time[1]) + (tonumber(time[2]) / 1000000)

local stored = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(stored[1])
local ts = tonumber(stored[2])

-- The key expired between reserving and reconciling. A bucket with no stored
-- state is by definition full, so there is nothing to return to and nothing
-- lost by recreating it at capacity.
if tokens == nil then
  tokens = capacity
  ts = now
end

local elapsed = now - ts
if elapsed > 0 then
  tokens = math.min(capacity, tokens + elapsed * refill_per_sec)
end

tokens = math.min(capacity, tokens + delta)

redis.call('HMSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('PEXPIRE', key, ttl_ms)

return tostring(tokens)
`

// Limiter enforces token buckets in Redis. It holds no in-process state of
// its own — every bucket lives entirely in Redis — so a Limiter is safe to
// share across every goroutine handling every request, and safe to run as
// one instance per gateway replica with no coordination between them beyond
// the Redis they already share.
type Limiter struct {
	rdb     *redis.Client
	consume *redis.Script
	settle  *redis.Script
}

// NewLimiter wraps an already-connected Redis client. main.go owns the
// client's lifecycle — construction, address configuration, and closing it
// on shutdown — the same way it owns every other injected dependency; this
// package only ever borrows it.
func NewLimiter(rdb *redis.Client) *Limiter {
	return &Limiter{
		rdb:     rdb,
		consume: redis.NewScript(consumeScript),
		settle:  redis.NewScript(settleScript),
	}
}

// Consume attempts to take amount tokens from a team's bucket.
//
// capacityPerMinute is the team's configured limit (RateLimits.RPM or .TPM);
// it is passed on every call rather than stored once, so a Phase 4 admin
// PATCH that raises or lowers a team's limit takes effect on the very next
// request — the bucket's stored token count carries over unchanged, only the
// ceiling and refill rate it's measured against change.
//
// ttl bounds how long an idle bucket's key survives in Redis. It should
// comfortably exceed one refill window (60s) — Step 3.2 asks for this
// specifically so a team that stops sending traffic doesn't leave a key
// behind forever. It is sent to Redis in milliseconds (PEXPIRE, not EXPIRE):
// truncating a sub-second ttl down to whole seconds would send Redis a 0,
// and EXPIRE key 0 deletes the key immediately instead of leaving it alone.
func (l *Limiter) Consume(ctx context.Context, teamID string, kind LimitType, capacityPerMinute, amount int, ttl time.Duration) (Result, error) {
	if capacityPerMinute <= 0 {
		return Result{}, fmt.Errorf("ratelimit: capacity must be positive, got %d", capacityPerMinute)
	}
	if amount <= 0 {
		return Result{}, fmt.Errorf("ratelimit: amount must be positive, got %d", amount)
	}
	if ttl <= 0 {
		return Result{}, fmt.Errorf("ratelimit: ttl must be positive, got %s", ttl)
	}

	refillPerSecond := float64(capacityPerMinute) / 60.0

	raw, err := l.consume.Run(ctx, l.rdb,
		[]string{bucketKey(teamID, kind)},
		capacityPerMinute, refillPerSecond, amount, ttl.Milliseconds(),
	).Slice()
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: consuming %s bucket for team %q: %w", kind, teamID, err)
	}
	if len(raw) != 3 {
		return Result{}, fmt.Errorf("ratelimit: unexpected script result shape %v", raw)
	}

	allowed, ok := raw[0].(int64)
	if !ok {
		return Result{}, fmt.Errorf("ratelimit: unexpected 'allowed' type %T", raw[0])
	}

	remaining, err := parseScriptFloat(raw[1])
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: parsing remaining tokens: %w", err)
	}
	retryAfter, err := parseScriptFloat(raw[2])
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: parsing retry-after: %w", err)
	}

	return Result{
		Allowed:    allowed == 1,
		Remaining:  remaining,
		RetryAfter: time.Duration(retryAfter * float64(time.Second)),
	}, nil
}

// --- reservations -----------------------------------------------------------
//
// Step 3.3's problem: a request's output token count is unknown until the
// response finishes, but the TPM bucket has to be checked *before* the
// provider is called — otherwise a team could blow through its limit by an
// unbounded margin inside a single burst, with the charge only landing after
// the damage was done.
//
// The resolution is to charge a ceiling up front and settle up afterwards:
// reserve (estimated input + max_tokens), then return the unused portion once
// real usage is known. The reservation is always larger than the eventual
// truth, so the bucket is never under-charged while a request is in flight.

// Reservation is a handle on tokens already taken from a bucket, to be
// reconciled once the real usage is known.
type Reservation struct {
	limiter  *Limiter
	teamID   string
	kind     LimitType
	capacity int
	reserved int
	ttl      time.Duration
}

// Reserve consumes amount up front and returns a handle for settling up later.
//
// The returned Reservation is nil when the request was denied or errored:
// nothing was taken, so there is nothing to give back. Reconcile is safe to
// call on that nil handle, so a caller can defer it unconditionally without
// a nil check.
func (l *Limiter) Reserve(ctx context.Context, teamID string, kind LimitType, capacityPerMinute, amount int, ttl time.Duration) (*Reservation, Result, error) {
	res, err := l.Consume(ctx, teamID, kind, capacityPerMinute, amount, ttl)
	if err != nil || !res.Allowed {
		return nil, res, err
	}

	return &Reservation{
		limiter:  l,
		teamID:   teamID,
		kind:     kind,
		capacity: capacityPerMinute,
		reserved: amount,
		ttl:      ttl,
	}, res, nil
}

// Reconcile settles a reservation against the tokens the request actually
// used, returning the unused remainder to the bucket — or debiting the
// difference when the estimate turned out to be too low.
//
// It is written to be deferred immediately after a successful Reserve, which
// is what makes the plan's requirement hold: an error, a panic, or a client
// disconnect all still return the reservation on the way out, because a
// deferred call runs on every exit path rather than only the happy one.
//
// The nil receiver is deliberate and is ordinary Go: a method with a pointer
// receiver can be called on a nil pointer as long as it does not dereference
// it. A denied Reserve therefore needs no special handling at the call site.
func (r *Reservation) Reconcile(ctx context.Context, actual int) error {
	if r == nil {
		return nil
	}
	if actual < 0 {
		return fmt.Errorf("ratelimit: actual usage cannot be negative, got %d", actual)
	}

	// Positive: the request used less than reserved, so tokens go back.
	// Negative: it used more, so the overage is debited.
	delta := r.reserved - actual
	if delta == 0 {
		return nil
	}

	refillPerSecond := float64(r.capacity) / 60.0

	if err := r.limiter.settle.Run(ctx, r.limiter.rdb,
		[]string{bucketKey(r.teamID, r.kind)},
		r.capacity, refillPerSecond, delta, r.ttl.Milliseconds(),
	).Err(); err != nil {
		return fmt.Errorf("ratelimit: reconciling %s reservation for team %q: %w", r.kind, r.teamID, err)
	}
	return nil
}

// Reserved reports how many tokens this reservation took up front, so a
// caller can log or meter the estimate alongside the actual.
func (r *Reservation) Reserved() int {
	if r == nil {
		return 0
	}
	return r.reserved
}

// parseScriptFloat reads one of the script's stringified numeric return
// values. Lua's EVAL protocol can only return integers, strings, and a few
// other primitives — never a native float — so the script formats its floats
// as strings and this undoes that on the way back into Go.
func parseScriptFloat(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("expected a string, got %T", v)
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, fmt.Errorf("parsing %q: %w", s, err)
	}
	return f, nil
}
