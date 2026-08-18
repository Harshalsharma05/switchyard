package resilience

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// This file is Step 7.3's shared-storage layer: the Redis side of the circuit
// breaker, kept out of breaker.go so the state machine there stays a state
// machine and not a Redis client.
//
// What is shared and what is not, per the Step 7.3 decision recorded in
// breaker.go's BreakerStore doc: the *verdict* is shared, the *evidence* is
// not. A replica counts failures against its own rolling window in memory,
// and the first one to cross its threshold publishes an open episode here for
// every other replica to honour.

// keyPrefix namespaces every key this package writes, matching the convention
// internal/ratelimit ("switchyard:rl"), internal/budget, and internal/health
// ("switchyard:health") already follow, so one Redis instance can carry all
// four phases' state without collision.
const keyPrefix = "switchyard:breaker"

// stateKey and probeKey are per provider+model, which is Step 7.4's
// granularity rule expressed in the key schema: one bad model must not take
// out every model a provider serves, so they cannot share a key.
func stateKey(labels Labels) string {
	return fmt.Sprintf("%s:%s:%s", keyPrefix, labels.Provider, labels.Model)
}

func probeKey(labels Labels) string {
	return stateKey(labels) + ":probe"
}

// redisTimeout bounds every Redis call this store makes.
//
// It is far tighter than internal/health's 500ms equivalent, and deliberately
// so: health's writes happen on a background ticker where half a second costs
// nobody anything, whereas a breaker Load can land on the request path when
// the local cache expires. The project's p95 overhead budget is 10ms total,
// so a Redis call allowed to take 500ms could blow that budget fifty times
// over on its own. 50ms is still enormous next to a healthy Redis (sub-
// millisecond on the same host or network), which makes exceeding it strong
// evidence that Redis is genuinely unwell — exactly the case where the
// breaker should stop waiting and fall back to its local view.
const redisTimeout = 50 * time.Millisecond

// stateTTLSlack is how long an open episode's key outlives the cooldown it
// describes.
//
// The key has to outlast its own cooldown, or a breaker would forget it was
// open at the very moment it became eligible to probe. Past that it is a
// self-healing bound: a breaker left open by replicas that have all since
// died, or for a provider+model nobody routes to any more, eventually expires
// and the fleet fails open rather than rejecting traffic forever on the word
// of a process that no longer exists. Failing open is the same call CLAUDE.md
// makes everywhere else that the gateway's own state could become the reason
// a request fails.
const stateTTLSlack = 10 * time.Minute

// loadScript derives the shared state from one stored deadline.
//
// The state is computed rather than stored, the same shape internal/ratelimit's
// bucket takes: no key at all means closed, a deadline in the future means
// open, and a deadline in the past means the cooldown has elapsed and the
// breaker is half-open. Storing "open" as a flag that something later has to
// flip to "half-open" would need a timer somewhere; deriving it from a
// deadline needs nothing but the clock, so no replica has to own that
// transition and none can miss it.
//
// The clock is Redis's own (TIME), not the calling replica's, for the reason
// bucket.go spells out: replicas whose host clocks differ by even a few
// hundred milliseconds would otherwise disagree about whether a cooldown had
// elapsed. Redis is the one clock every replica already shares.
const loadScript = `
local key = KEYS[1]

local stored = redis.call('HMGET', key, 'until_ms', 'reopens', 'successes')
if not stored[1] then
  return {'closed', 0, 0}
end

local t = redis.call('TIME')
local now_ms = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

local until_ms  = tonumber(stored[1])
local reopens   = tonumber(stored[2]) or 0
local successes = tonumber(stored[3]) or 0

if now_ms < until_ms then
  return {'open', reopens, successes}
end
return {'half_open', reopens, successes}
`

// openScript publishes an open episode: a fresh cooldown deadline, the
// episode count the exponential backoff is computed from, and a zeroed
// success streak.
//
// It also drops the probe key. A new episode invalidates any probe that was
// in flight against the previous one — that probe was testing a recovery this
// failure has just disproved — and leaving the lock held would stall the
// first probe of the new episode until its TTL expired.
//
// This is a blind write rather than a compare-and-set. Two replicas opening
// at once are reporting the same news about the same provider, so the loser
// of the race loses nothing but a near-identical deadline.
const openScript = `
local key       = KEYS[1]
local probe_key = KEYS[2]

local cooldown_ms = tonumber(ARGV[1])
local reopens     = tonumber(ARGV[2])
local ttl_ms      = tonumber(ARGV[3])

local t = redis.call('TIME')
local now_ms = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

redis.call('HSET', key,
  'until_ms', now_ms + cooldown_ms,
  'reopens', reopens,
  'successes', 0)
redis.call('PEXPIRE', key, ttl_ms)
redis.call('DEL', probe_key)

return 1
`

// probeSuccessScript records one successful probe against the shared streak
// and closes the breaker once the streak reaches its threshold.
//
// Increment-and-test has to be atomic, which is the whole reason this is a
// script: two replicas each reading "1 success so far" and each writing back
// "2" would close a breaker on one real probe. HINCRBY inside a script gives
// the increment and the threshold test one indivisible round trip.
//
// Closing is a DEL of the state key, which loadScript reads back as closed —
// the absence of an episode *is* the closed state, so there is no separate
// value to write and no way for a stale "closed" record to linger.
//
// The probe lock is released on both paths: the probe that just reported is
// finished either way, and the next one should not have to wait out its TTL.
const probeSuccessScript = `
local key       = KEYS[1]
local probe_key = KEYS[2]

local threshold = tonumber(ARGV[1])

-- No episode to make progress against: another replica has already closed
-- this breaker, and this success is simply the good news arriving twice.
if redis.call('EXISTS', key) == 0 then
  redis.call('DEL', probe_key)
  return {'closed', 0}
end

local successes = redis.call('HINCRBY', key, 'successes', 1)

if successes >= threshold then
  redis.call('DEL', key)
  redis.call('DEL', probe_key)
  return {'closed', successes}
end

redis.call('DEL', probe_key)
return {'half_open', successes}
`

// SharedState is the fleet's view of one breaker, as loadScript derives it.
type SharedState struct {
	State State

	// Reopens is how many consecutive open episodes the fleet has seen, which
	// is what the exponential cooldown grows against. Sharing it is what stops
	// each replica from restarting that growth at zero and handing a provider
	// that keeps failing its probes a permanently short cooldown.
	Reopens int

	// Successes is the shared probe streak. It must be shared for
	// SuccessThreshold above 1 to mean anything across replicas: with a
	// fleet-wide probe lock, consecutive probes may well be served by
	// different replicas, and per-replica streaks would each sit at one
	// forever without ever closing the breaker.
	Successes int
}

// RedisStore is the Redis-backed BreakerStore, scoped to a single
// provider+model. One instance per breaker, built alongside it in Step 7.4's
// wiring.
type RedisStore struct {
	rdb      *redis.Client
	stateKey string
	probeKey string

	// Scripts are compiled once here rather than per call, the same as
	// ratelimit.Limiter: redis.Script caches the SHA and uses EVALSHA, falling
	// back to a full EVAL only when Redis reports the script unknown.
	load         *redis.Script
	open         *redis.Script
	probeSuccess *redis.Script
}

// NewRedisStore wraps an already-connected Redis client for one
// provider+model. main.go owns the client's lifecycle, exactly as it does for
// ratelimit.NewLimiter and the health Monitor; this store only borrows it.
func NewRedisStore(rdb *redis.Client, labels Labels) *RedisStore {
	return &RedisStore{
		rdb:          rdb,
		stateKey:     stateKey(labels),
		probeKey:     probeKey(labels),
		load:         redis.NewScript(loadScript),
		open:         redis.NewScript(openScript),
		probeSuccess: redis.NewScript(probeSuccessScript),
	}
}

// Load reads the fleet's current view of this breaker.
func (s *RedisStore) Load(ctx context.Context) (SharedState, error) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	raw, err := s.load.Run(ctx, s.rdb, []string{s.stateKey}).Slice()
	if err != nil {
		return SharedState{}, fmt.Errorf("resilience: loading shared breaker state for %s: %w", s.stateKey, err)
	}
	return parseSharedState(raw)
}

// Open publishes an open episode lasting cooldown, tagged with the episode
// count so every replica computes the same exponential backoff.
func (s *RedisStore) Open(ctx context.Context, cooldown time.Duration, reopens int) error {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	err := s.open.Run(ctx, s.rdb,
		[]string{s.stateKey, s.probeKey},
		cooldown.Milliseconds(), reopens, (cooldown + stateTTLSlack).Milliseconds(),
	).Err()
	if err != nil {
		return fmt.Errorf("resilience: publishing open breaker episode for %s: %w", s.stateKey, err)
	}
	return nil
}

// RecordProbeSuccess adds one success to the shared streak, closing the
// breaker if that reaches threshold. It returns the resulting shared state,
// so the caller can mirror it without a second round trip.
func (s *RedisStore) RecordProbeSuccess(ctx context.Context, threshold int) (SharedState, error) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	raw, err := s.probeSuccess.Run(ctx, s.rdb,
		[]string{s.stateKey, s.probeKey},
		threshold,
	).Slice()
	if err != nil {
		return SharedState{}, fmt.Errorf("resilience: recording probe success for %s: %w", s.stateKey, err)
	}
	return parseSharedState(raw)
}

// ClaimProbe takes the fleet-wide probe slot, returning false if another
// replica already holds it.
//
// This is Step 7.2's single-probe guard promoted from one process to the
// whole fleet, and it is the reason that guard cannot simply stay a local
// bool once replicas exist: five replicas each admitting "exactly one" probe
// is five concurrent probes against a provider that just fell over, which is
// precisely the stampede the guard was built to prevent.
//
// SET NX PX is the whole mechanism — Redis sets the key only if it is absent,
// atomically, so exactly one caller can win. ttl is the caller's ProbeTimeout,
// which does double duty here: as the abandoned-probe safety valve it already
// was locally, and as the lock expiry that stops a replica dying mid-probe
// from holding the slot forever.
func (s *RedisStore) ClaimProbe(ctx context.Context, ttl time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	// The value is unread — only the key's presence carries meaning — but a
	// timestamp makes `redis-cli GET` during an incident say when the probe
	// started rather than just "1".
	ok, err := s.rdb.SetNX(ctx, s.probeKey, time.Now().UTC().Format(time.RFC3339Nano), ttl).Result()
	if err != nil {
		return false, fmt.Errorf("resilience: claiming probe slot for %s: %w", s.stateKey, err)
	}
	return ok, nil
}

// Reset clears every trace of this breaker's episode, which is what Step
// 7.4's manual admin reset needs: an operator who already knows a provider is
// healthy should not have to wait out a cooldown to prove it.
//
// Deleting both keys is the whole operation — loadScript reads a missing
// state key as closed, and a missing probe key as a free slot.
func (s *RedisStore) Reset(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()

	if err := s.rdb.Del(ctx, s.stateKey, s.probeKey).Err(); err != nil {
		return fmt.Errorf("resilience: resetting breaker state for %s: %w", s.stateKey, err)
	}
	return nil
}

// parseSharedState decodes the {state, reopens, successes} triple both
// scripts return. Lua's reply protocol turns its strings into Go strings and
// its numbers into int64, so the shapes below are what a correct script
// always produces — anything else is a script bug worth reporting loudly
// rather than defaulting past.
func parseSharedState(raw []any) (SharedState, error) {
	if len(raw) < 2 {
		return SharedState{}, fmt.Errorf("resilience: unexpected breaker script result shape %v", raw)
	}

	name, ok := raw[0].(string)
	if !ok {
		return SharedState{}, fmt.Errorf("resilience: unexpected breaker state type %T", raw[0])
	}
	state, ok := parseState(name)
	if !ok {
		return SharedState{}, fmt.Errorf("resilience: unrecognised breaker state %q", name)
	}

	out := SharedState{State: state}

	// probeSuccessScript returns {state, successes}; loadScript returns
	// {state, reopens, successes}. The two-element form leaves Reopens zero,
	// which is correct for it: a closing or still-probing breaker's episode
	// count is read from Load, not inferred here.
	switch len(raw) {
	case 2:
		n, err := parseScriptInt(raw[1])
		if err != nil {
			return SharedState{}, fmt.Errorf("resilience: parsing probe successes: %w", err)
		}
		out.Successes = n
	default:
		reopens, err := parseScriptInt(raw[1])
		if err != nil {
			return SharedState{}, fmt.Errorf("resilience: parsing reopen count: %w", err)
		}
		successes, err := parseScriptInt(raw[2])
		if err != nil {
			return SharedState{}, fmt.Errorf("resilience: parsing probe successes: %w", err)
		}
		out.Reopens, out.Successes = reopens, successes
	}

	return out, nil
}

func parseScriptInt(v any) (int, error) {
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("expected an integer, got %T", v)
	}
	return int(n), nil
}

// parseState is State.String's inverse, for reading a state name back out of
// a Lua reply — the same pairing health.parseStatus has with Status.String.
func parseState(s string) (State, bool) {
	switch s {
	case "closed":
		return StateClosed, true
	case "open":
		return StateOpen, true
	case "half_open":
		return StateHalfOpen, true
	default:
		return State(0), false
	}
}
