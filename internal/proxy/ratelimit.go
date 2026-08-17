package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
)

// RateLimiter is the slice of internal/ratelimit.Limiter this package needs.
//
// Declared here, by the consumer, for the same reason Resolver and
// Authenticator are: proxy depends on these two methods, not the concrete
// *ratelimit.Limiter, which is what lets a test inject a fake without
// touching Redis.
type RateLimiter interface {
	Consume(ctx context.Context, teamID string, kind ratelimit.LimitType, capacityPerMinute, amount int, ttl time.Duration) (ratelimit.Result, error)
	Reserve(ctx context.Context, teamID string, kind ratelimit.LimitType, capacityPerMinute, amount int, ttl time.Duration) (*ratelimit.Reservation, ratelimit.Result, error)
}

// bucketTTL bounds how long an idle team's Redis bucket key survives.
//
// Both RPM and TPM buckets refill on a 60-second window; 2 minutes
// comfortably outlives that so a bucket is never evicted mid-refill, while
// still cleaning up after a team that has gone quiet.
const bucketTTL = 2 * time.Minute

// reconcileTimeout bounds the Reserve settle-up call in handler.go.
//
// It is deliberately its own context, not derived from the request's: a
// client disconnect or a request timeout both cancel r.Context(), and if
// Reconcile ran on that same context it would fail before it could execute —
// silently leaking the reservation until its bucket TTL expired. Returning a
// reservation is a small, fast Redis round trip that has to happen
// regardless of why the request ended, which Step 3.3's DECISIONS.md entry
// calls out specifically.
const reconcileTimeout = 2 * time.Second

// checkTimeout bounds the RPM Consume, TPM Reserve, and Step 4.2 budget
// Reserve calls, independent of whatever the underlying Redis client's own
// dial/retry settings happen to be.
//
// Measured live: with go-redis's defaults, one request against a genuinely
// unreachable Redis took 3.5 seconds to fail open — the connection pool
// retries dialing internally in a way redis.Options' own timeout fields
// don't fully bound. A context deadline is the one mechanism a Go redis
// client is guaranteed to respect no matter its internal retry behavior,
// current or future, so this is the authoritative ceiling on "how long can a
// Redis problem add to this request" — fail-open is only actually fast if
// detecting the failure is fast.
const checkTimeout = 200 * time.Millisecond

// batchShedFloor is Step 3.5's threshold: once a batch-priority team's
// bucket has fewer than this fraction of capacity left, its requests are
// shed even though the bucket itself still has room. Realtime-priority
// teams are never shed early — they are only denied at true exhaustion,
// same as before this step existed.
//
// The plan's own words for this are "simplest defensible implementation,"
// explicitly ruling out a real priority queue or cross-team pooling for
// Part 1. This is a single fixed threshold read from the team's own bucket,
// nothing shared across teams.
const batchShedFloor = 0.2

// shouldShedForPriority reports whether an otherwise-allowed result should
// still be rejected under Step 3.5: team is batch-priority, and this
// request's own bucket state shows less than batchShedFloor of capacity
// remaining.
//
// res.Remaining is read as-is — the state *after* this request was
// admitted, not reconstructed to "before" — which is the simpler of two
// reasonable readings and treats the request that crosses the 80% line as
// the one that gets shed, rather than the next one after it.
func shouldShedForPriority(team *auth.Team, capacity int, res ratelimit.Result) bool {
	if team.Priority != auth.PriorityBatch || capacity <= 0 {
		return false
	}
	return res.Remaining < batchShedFloor*float64(capacity)
}

// RateLimit enforces the RPM bucket. It runs after Auth, reading the
// *auth.Team Auth attached to the context, and before anything that would
// call a provider.
//
// TPM is deliberately not checked here. Estimating a request's token cost
// needs the decoded body, and nothing before the handler has parsed one —
// the same constraint Step 3.1 hit with the model allowlist. TPM enforcement
// lives in handler.go instead, right after decode.
func RateLimit(limiter RateLimiter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			team := TeamFrom(r.Context())
			if team == nil {
				// Every route this wraps is mounted behind Auth, so a nil team
				// here means the chain was wired wrong, not that the caller
				// did anything wrong.
				log.ErrorContext(r.Context(), "rate limit middleware reached without an authenticated team")
				writeError(w, log, http.StatusInternalServerError, "internal_error",
					"the gateway could not resolve the caller's team")
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
			defer cancel()

			res, err := limiter.Consume(ctx, team.ID, ratelimit.RPM, team.RateLimits.RPM, 1, bucketTTL)
			if err != nil {
				// Fail open: the gateway must never be the reason a request
				// fails, and Redis being unreachable is exactly that class of
				// dependency failure. Logged loudly here; Phase 9 is where the
				// counter this deserves actually exists.
				log.ErrorContext(r.Context(), "RPM rate limit check failed; failing open",
					slog.String("team", team.ID), slog.Any("error", err))
				next.ServeHTTP(w, r)
				return
			}

			if !res.Allowed {
				writeRateLimitError(w, log, ratelimit.RPM, team.RateLimits.RPM, res)
				return
			}

			// The bucket admitted this request, but a batch-priority team
			// past Step 3.5's threshold is shed anyway. The token this
			// Consume already took stays spent: RPM's unit is exactly one
			// request, so "spent" here means exactly what it says — an
			// attempt was made and correctly identified as over the soft
			// threshold. Refunding it would erase the backpressure this
			// exists to apply and let a naive immediate retry look free.
			if shouldShedForPriority(team, team.RateLimits.RPM, res) {
				writePriorityShedError(w, log, ratelimit.RPM, team.RateLimits.RPM, res)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeRateLimitHeaders sets the Step 3.4 headers shared by every rate-limit
// rejection — hard exhaustion and Step 3.5's priority shed alike — so both
// call sites describe a rejection identically rather than each growing its
// own header logic. It returns the Retry-After value actually written, in
// whole seconds, so callers can fold it into their error message without
// recomputing it.
func writeRateLimitHeaders(w http.ResponseWriter, kind ratelimit.LimitType, limit int, res ratelimit.Result) int {
	retryAfter := res.RetryAfter
	if retryAfter < time.Second {
		// A bucket that just denied this call is not ready this instant;
		// advertising less than a second would invite a retry guaranteed to
		// fail again. A priority shed carries no RetryAfter of its own
		// (res.Allowed was true), so this floor is what gives it a sane
		// value too.
		retryAfter = time.Second
	}
	retryAfterSeconds := int(math.Ceil(retryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))

	remaining := res.Remaining
	if remaining < 0 {
		// A TPM bucket can go negative after Step 3.3 debits an
		// under-estimated reservation. The header is a client-facing count,
		// so it floors at zero rather than reporting a debt.
		remaining = 0
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(remaining)))

	// X-RateLimit-Reset: seconds until the bucket refills to full capacity.
	// A token bucket has no fixed window to "reset" the way a fixed-window
	// limiter does, so this is the closest honest analog — how long until
	// this team could make its largest possible request again.
	refillPerSecond := float64(limit) / 60.0
	resetSeconds := (float64(limit) - res.Remaining) / refillPerSecond
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(math.Ceil(resetSeconds))))

	return retryAfterSeconds
}

// writeRateLimitError sends a 429 for the hard-exhaustion case: the bucket
// itself could not admit this request.
func writeRateLimitError(w http.ResponseWriter, log *slog.Logger, kind ratelimit.LimitType, limit int, res ratelimit.Result) {
	retryAfterSeconds := writeRateLimitHeaders(w, kind, limit, res)
	writeError(w, log, http.StatusTooManyRequests, "rate_limit_exceeded",
		fmt.Sprintf("%s rate limit exceeded; retry after %d seconds", strings.ToUpper(string(kind)), retryAfterSeconds))
}

// writePriorityShedError sends a 429 for Step 3.5: the bucket had room for
// this request, but the caller is a batch-priority team already past the
// shed threshold, so the request is rejected to leave headroom rather than
// let batch traffic run its own bucket all the way to zero.
func writePriorityShedError(w http.ResponseWriter, log *slog.Logger, kind ratelimit.LimitType, limit int, res ratelimit.Result) {
	retryAfterSeconds := writeRateLimitHeaders(w, kind, limit, res)
	writeError(w, log, http.StatusTooManyRequests, "priority_shed",
		fmt.Sprintf("%s batch-priority traffic is shed once the bucket drops below %.0f%% remaining; retry after %d seconds or from a realtime-priority team",
			strings.ToUpper(string(kind)), batchShedFloor*100, retryAfterSeconds))
}
