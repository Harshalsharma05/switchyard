package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Harshalsharma05/switchyard/internal/cache"
)

// CacheTuner is the slice of the semantic cache this package needs.
//
// Step 7.3 asks for a tuning endpoint that replays historical requests at
// different thresholds. Real replay is impossible here on purpose: the request
// log stores no prompt text (see migrations/0001_requests.sql). The cache's own
// embedding index is the substitute — it is the only record of real traffic
// that exists, and the sweep reports its own limitation alongside the numbers.
type CacheTuner interface {
	Sweep(ctx context.Context, thresholds []float32, maxBuckets int) (*cache.SweepReport, error)

	// Step 7.4's manual invalidation. A changed system prompt or model needs no
	// purge — it produces a different fingerprint, so old entries become
	// unreachable and age out on their own. These cover the cases structure
	// cannot: a bad answer stored under a key that is still being asked for.
	PurgeTeam(ctx context.Context, teamID string) (*cache.PurgeResult, error)
	PurgePrefix(ctx context.Context, prefix string) (*cache.PurgeResult, error)
	PurgeAll(ctx context.Context) (*cache.PurgeResult, error)
}

// maxSweepThresholds bounds the query string. Each threshold is another pass
// over the scored pairs, and an operator has no reason to ask for more.
const maxSweepThresholds = 64

// tuneCache serves GET /admin/cache/tune.
//
// Admin-gated because the sweep reads across every team's cache buckets, which
// is exactly the cross-team access Step 2.1's gate exists to restrict.
func tuneCache(tuner CacheTuner, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tuner == nil {
			writeError(w, log, http.StatusNotFound, "cache_disabled", "the semantic cache is not enabled on this gateway")
			return
		}

		thresholds, err := parseThresholds(r.URL.Query().Get("thresholds"))
		if err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		maxBuckets := 0
		if raw := r.URL.Query().Get("max_buckets"); raw != "" {
			maxBuckets, err = strconv.Atoi(raw)
			if err != nil || maxBuckets <= 0 {
				writeError(w, log, http.StatusBadRequest, "invalid_request", "max_buckets must be a positive integer")
				return
			}
		}

		report, err := tuner.Sweep(r.Context(), thresholds, maxBuckets)
		if err != nil {
			log.ErrorContext(r.Context(), "sweeping cache thresholds", slog.Any("error", err))
			writeError(w, log, http.StatusServiceUnavailable, "cache_unavailable", "could not read the cache index")
			return
		}

		writeJSON(w, log, http.StatusOK, report)
	}
}

var (
	errBadThreshold      = errors.New("each threshold must be a number between -1 and 1")
	errTooManyThresholds = errors.New("too many thresholds requested")
)

// parseThresholds reads a comma-separated list, or returns nil for the sweep's
// own default range.
func parseThresholds(raw string) ([]float32, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	if len(parts) > maxSweepThresholds {
		return nil, errTooManyThresholds
	}

	out := make([]float32, 0, len(parts))
	for _, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			return nil, errBadThreshold
		}
		if v < -1 || v > 1 {
			return nil, errBadThreshold
		}
		out = append(out, float32(v))
	}
	return out, nil
}

// purgeCache serves DELETE /admin/cache.
//
// Exactly one of team, prefix, or all must be given. Requiring an explicit
// scope means an operator cannot wipe the whole cache by forgetting a query
// parameter — the destructive case has to be asked for by name.
func purgeCache(tuner CacheTuner, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tuner == nil {
			writeError(w, log, http.StatusNotFound, "cache_disabled", "the semantic cache is not enabled on this gateway")
			return
		}

		q := r.URL.Query()
		team, prefix, all := q.Get("team"), q.Get("prefix"), q.Get("all") == "true"

		scopes := 0
		for _, given := range []bool{team != "", prefix != "", all} {
			if given {
				scopes++
			}
		}
		if scopes != 1 {
			writeError(w, log, http.StatusBadRequest, "invalid_request",
				"specify exactly one of team, prefix, or all=true")
			return
		}

		var (
			result *cache.PurgeResult
			err    error
		)
		switch {
		case team != "":
			result, err = tuner.PurgeTeam(r.Context(), team)
		case prefix != "":
			result, err = tuner.PurgePrefix(r.Context(), prefix)
		default:
			result, err = tuner.PurgeAll(r.Context())
		}

		if err != nil {
			log.ErrorContext(r.Context(), "purging cache", slog.Any("error", err))
			writeError(w, log, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		log.InfoContext(r.Context(), "cache purged",
			slog.String("scope", result.Scope), slog.String("target", result.Target),
			slog.Int("entries", result.Entries))

		writeJSON(w, log, http.StatusOK, result)
	}
}
