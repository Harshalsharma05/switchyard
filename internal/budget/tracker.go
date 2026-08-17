package budget

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces every key this file writes, so a Redis instance
// shared with rate limiting and health state (Phases 3 and 5) never collides
// with budget's own keys.
const keyPrefix = "switchyard:budget"

// periodGrace pads a period key's TTL past the literal end of its calendar
// month. It is cleanup, not a billing guarantee: the cap itself is enforced
// by the value stored, not by when the key disappears. The grace window just
// means an admin report or a spot-check run shortly after a month boundary
// still finds the prior period's key instead of racing its expiry.
const periodGrace = 48 * time.Hour

// periodKey builds the schema Step 4.2 specifies:
// switchyard:budget:{team_id}:{YYYY-MM}.
func periodKey(teamID, period string) string {
	return fmt.Sprintf("%s:%s:%s", keyPrefix, teamID, period)
}

// currentPeriod names the calendar month a spend counter belongs to, in UTC.
// UTC rather than the host's local zone: a billing period needs one boundary
// every gateway replica agrees on, regardless of what timezone it happens to
// run in.
func currentPeriod(now time.Time) string {
	return now.UTC().Format("2006-01")
}

// reserveScript is Step 4.2's entire admission decision, in one atomic Redis
// round trip: increment the period's counter, and only then decide whether
// the result fits under the cap.
//
// Doing the check *after* an unconditional atomic INCRBY, rather than a GET
// then a conditional SET, is what the plan's "atomic, no read-modify-write
// race" is really asking for: two concurrent requests racing the same key
// each get a distinct, correctly serialized total from Redis, so neither can
// read a stale pre-increment value and both believe they were the one that
// fit. Bundling the check and a denied request's rollback into the same
// script — rather than a second round trip from Go — means no other command
// can ever observe the counter holding an over-cap value, even briefly, and
// caps this at exactly one round trip regardless of outcome.
//
// This mirrors ratelimit's consumeScript in spirit — one atomic decision, one
// round trip — but needs none of its refill math: a monthly spend counter has
// no lazy decay, so there is nothing here but an increment and a comparison.
const reserveScript = `
local key     = KEYS[1]
local cap     = tonumber(ARGV[1])
local amount  = tonumber(ARGV[2])
local ttl_sec = tonumber(ARGV[3])

local is_new = redis.call('EXISTS', key) == 0
local total = redis.call('INCRBY', key, amount)

if is_new then
  redis.call('EXPIRE', key, ttl_sec)
end

if total > cap then
  redis.call('DECRBY', key, amount)
  return {0, total - amount}
end

return {1, total}
`

// Result is the outcome of one budget check.
type Result struct {
	// Allowed reports whether the estimated cost fit under the team's cap.
	Allowed bool

	// SpentMicros is the team's confirmed total for the period immediately
	// after this call: including this reservation's estimate when Allowed, or
	// the pre-reservation total when denied — a denied attempt is rolled back
	// inside the same script that computed it, so nothing was ever really
	// added.
	SpentMicros int64
}

// Tracker enforces monthly spend caps in Redis.
//
// Like ratelimit.Limiter, it holds no in-process state of its own — every
// counter lives entirely in Redis — so one Tracker is safe to share across
// every goroutine handling every request, and safe to run as one instance per
// gateway replica with no coordination beyond the Redis they already share.
type Tracker struct {
	rdb     *redis.Client
	reserve *redis.Script
}

// NewTracker wraps an already-connected Redis client. main.go owns the
// client's lifecycle, the same way it owns every other injected dependency;
// this package only ever borrows it.
func NewTracker(rdb *redis.Client) *Tracker {
	return &Tracker{rdb: rdb, reserve: redis.NewScript(reserveScript)}
}

// Reservation is a handle on spend already added to a team's period counter,
// to be reconciled once the request's real cost is known.
type Reservation struct {
	tracker  *Tracker
	teamID   string
	period   string
	reserved int64
}

// Reserve atomically adds estimatedMicros to a team's spend counter for the
// current period and reports whether the result stayed within capMicros.
//
// The returned *Reservation is nil whenever there is nothing to reconcile
// later: either Redis was unreachable (err != nil) or the request was
// denied. Both cases leave nothing owed back, the same reasoning
// ratelimit.Reservation gives for its own nil case.
//
// A Redis failure here is reported to the caller as an ordinary error, not
// silently treated as allowed. Unlike ratelimit.Limiter.Reserve, which fails
// open, budget enforcement fails *closed*: an unrecoverable dollar is a
// different class of problem than a delayed request, which is why CLAUDE.md
// calls this out as the one dependency in this gateway where "never be the
// reason a request fails" is deliberately overridden.
func (t *Tracker) Reserve(ctx context.Context, teamID string, capMicros, estimatedMicros int64) (*Reservation, Result, error) {
	if capMicros <= 0 {
		return nil, Result{}, fmt.Errorf("budget: cap must be positive, got %d", capMicros)
	}
	if estimatedMicros < 0 {
		return nil, Result{}, fmt.Errorf("budget: estimated cost cannot be negative, got %d", estimatedMicros)
	}

	now := time.Now()
	period := currentPeriod(now)

	raw, err := t.reserve.Run(ctx, t.rdb,
		[]string{periodKey(teamID, period)},
		capMicros, estimatedMicros, int64(ttlUntilPeriodEnd(now).Seconds()),
	).Slice()
	if err != nil {
		return nil, Result{}, fmt.Errorf("budget: reserving spend for team %q: %w", teamID, err)
	}
	if len(raw) != 2 {
		return nil, Result{}, fmt.Errorf("budget: unexpected script result shape %v", raw)
	}

	allowed, ok := raw[0].(int64)
	if !ok {
		return nil, Result{}, fmt.Errorf("budget: unexpected 'allowed' type %T", raw[0])
	}
	spent, ok := raw[1].(int64)
	if !ok {
		return nil, Result{}, fmt.Errorf("budget: unexpected 'spent' type %T", raw[1])
	}

	if allowed != 1 {
		return nil, Result{Allowed: false, SpentMicros: spent}, nil
	}

	return &Reservation{tracker: t, teamID: teamID, period: period, reserved: estimatedMicros},
		Result{Allowed: true, SpentMicros: spent}, nil
}

// Reconcile applies the signed difference between what was reserved and what
// the request actually cost, via one more atomic INCRBY (a negative delta is
// a decrement). Unlike Reserve, this never re-checks the cap: it settles a
// number the cap decision already admitted, the same way
// ratelimit.Reservation's settle-up is not a second admission gate. A single
// INCRBY needs no script of its own here — there is no read to race against,
// only one atomic write.
//
// The nil receiver is deliberate, mirroring ratelimit.Reservation: a denied
// or errored Reserve returns a nil *Reservation, and Reconcile is safe to
// call on it unconditionally, so a caller can defer it without a nil check.
func (r *Reservation) Reconcile(ctx context.Context, actualMicros int64) error {
	if r == nil {
		return nil
	}
	if actualMicros < 0 {
		return fmt.Errorf("budget: actual cost cannot be negative, got %d", actualMicros)
	}

	delta := actualMicros - r.reserved
	if delta == 0 {
		return nil
	}

	if err := r.tracker.rdb.IncrBy(ctx, periodKey(r.teamID, r.period), delta).Err(); err != nil {
		return fmt.Errorf("budget: reconciling spend for team %q: %w", r.teamID, err)
	}
	return nil
}

// Reserved reports how many micro-dollars this reservation took up front, so
// a caller can log or meter the estimate alongside the actual.
func (r *Reservation) Reserved() int64 {
	if r == nil {
		return 0
	}
	return r.reserved
}

// Spent reads a team's confirmed spend for the current period without
// reserving anything — a plain GET, since there is nothing here to make
// atomic against a concurrent writer. Used by GET /admin/teams and
// /admin/teams/{id} to report live spend alongside a team's configured
// limits.
//
// A team with no spend this period has no key at all, which Redis reports as
// redis.Nil; that is not an error here, it is the ordinary "spent nothing
// yet" case, so it is translated to zero rather than propagated.
func (t *Tracker) Spent(ctx context.Context, teamID string) (int64, error) {
	v, err := t.rdb.Get(ctx, periodKey(teamID, currentPeriod(time.Now()))).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("budget: reading spend for team %q: %w", teamID, err)
	}
	return v, nil
}

// Reset clears a team's spend for the current period back to zero — Step
// 4.3's manual reset endpoint. It deletes the key outright rather than
// setting it to 0, so a team that has spent nothing this period and one
// whose spend was just reset are indistinguishable, which is the correct
// state to be in either way.
func (t *Tracker) Reset(ctx context.Context, teamID string) error {
	if err := t.rdb.Del(ctx, periodKey(teamID, currentPeriod(time.Now()))).Err(); err != nil {
		return fmt.Errorf("budget: resetting spend for team %q: %w", teamID, err)
	}
	return nil
}

// ttlUntilPeriodEnd is how long from now until periodGrace past the first
// instant of next month, UTC — the TTL set on a period's key the moment it is
// first created. AddDate correctly rolls December into next January, so no
// special case is needed at a year boundary. Computed fresh on every Reserve
// rather than cached, since only the call that actually creates a period's
// key ever has this value take effect (see reserveScript's is_new guard),
// and that call needs an accurate "time left in this period" measured from
// whenever in the month it happens to land, not a flat duration sized for
// the worst case.
func ttlUntilPeriodEnd(now time.Time) time.Duration {
	now = now.UTC()
	nextMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return nextMonth.Sub(now) + periodGrace
}
