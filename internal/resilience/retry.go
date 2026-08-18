// Package resilience implements Phase 6 and Phase 7: retry with backoff,
// fallback chains, and (hand-written elsewhere, per CLAUDE.md) the circuit
// breaker. Step 6.1 is the retry half: retrying the *same* provider instance
// a bounded number of times before internal/proxy gives up on it or, from
// Step 6.2 onward, falls back to the next one in its tier.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Config controls Step 6.1's retry loop. It is process-level (env vars in
// cmd/gateway), not a configs/providers.yaml field — the plan describes one
// cap and one base delay, not something that varies per provider.
type Config struct {
	// MaxAttempts is the total number of tries, including the first —
	// MaxAttempts: 3 means up to 2 retries. The plan's example is 3.
	MaxAttempts int

	// BaseDelay is the backoff unit full jitter multiplies by 2^n. The plan
	// gives no example value; a few tens of milliseconds is enough to absorb
	// a brief provider hiccup without noticeably slowing a request that only
	// needed one retry.
	BaseDelay time.Duration

	// MaxTotalAttempts is Step 6.3's chain-wide ceiling: the most provider
	// calls one client request may cause, summed across every candidate in
	// its fallback chain. MaxAttempts alone bounds a single provider;
	// without this, a 3-attempt retry policy against a 5-entry tier is 15
	// upstream calls, which is exactly the retry amplification CLAUDE.md
	// warns about — the struggling provider fleet gets hit hardest at the
	// moment it can least afford it.
	//
	// The primary spends its full retry allowance first, so what remains is
	// spent one attempt at a time walking down the chain. That ordering is
	// deliberate: a fallback is already evidence the first choice is
	// unhealthy, so the budget belongs to the option most likely to work.
	MaxTotalAttempts int
}

// NewConfig validates and returns a Config, the same fail-fast-at-boot shape
// health.NewMonitor and health.NewChecker use for their own settings.
func NewConfig(maxAttempts int, baseDelay time.Duration, maxTotalAttempts int) (Config, error) {
	cfg := Config{MaxAttempts: maxAttempts, BaseDelay: baseDelay, MaxTotalAttempts: maxTotalAttempts}
	if maxAttempts < 1 {
		return Config{}, fmt.Errorf("resilience: max attempts must be at least 1, got %d", maxAttempts)
	}
	if baseDelay <= 0 {
		return Config{}, fmt.Errorf("resilience: base delay must be positive, got %s", baseDelay)
	}
	// Rejected rather than silently clamped: a total below the per-provider
	// cap would make MaxAttempts unreachable, so the two settings would
	// disagree about what the retry policy is and the file would document a
	// policy the gateway does not run.
	if maxTotalAttempts < maxAttempts {
		return Config{}, fmt.Errorf(
			"resilience: max total attempts (%d) must be at least max attempts (%d)",
			maxTotalAttempts, maxAttempts,
		)
	}
	return cfg, nil
}

// ShouldFallback reports whether a failed attempt is worth trying against a
// different provider.
//
// The split is by *whose fault the failure is*, which is a different question
// from Retryable's "will the same provider answer differently in a moment."
// A rate limit, a 5xx, a timeout, or a transport failure are all conditions
// another provider may not share. So is an auth failure: that is SwitchYard's
// own upstream credential being rejected, not the caller's, and a working
// provider is exactly the right answer to a dead API key — which is why this
// is not simply Retryable reused, since auth failures are deliberately not
// retryable against the provider that just rejected the key.
//
// A malformed request or a content policy refusal, on the other hand, is
// about the request itself. It will fail identically everywhere, so walking
// the chain would multiply load for a guaranteed failure and make the caller
// wait several round trips for the same 400 they would have got immediately.
func ShouldFallback(err error) bool {
	provErr := asProviderError(err)
	if provErr == nil {
		// Not a classified provider failure — a resolve error, or a bug. Left
		// eligible on the optimistic reading: an unclassified failure is more
		// likely something about this provider than about the request.
		return true
	}

	switch provErr.Kind {
	case provider.KindInvalidRequest, provider.KindContentPolicy:
		return false
	default:
		return true
	}
}

// Labels attach to every log line Do emits, so a retry is traceable back to
// what it was retrying without the caller repeating that context at each
// call site.
type Labels struct {
	Provider string
	Model    string
}

// maxBackoffExponent bounds 2^n in fullJitter regardless of how large
// MaxAttempts is configured. base << 63 would overflow time.Duration's int64
// long before any real deployment sets MaxAttempts that high; this is a
// defensive ceiling, not a value any sane config reaches.
const maxBackoffExponent = 20

// Do executes attempt up to cfg.MaxAttempts times, retrying only while the
// error is a *provider.Error with Retryable set — auth failures, content
// policy refusals, and malformed requests (Step 6.1's "never retry" list)
// are never *provider.Error with Retryable true, so they fall out on the
// first try with no special-casing here. attempt's second argument is the
// 1-indexed try number.
//
// Between attempts, Do sleeps for the provider's own Retry-After when the
// failed attempt's error carries one, or a full-jitter exponential backoff
// otherwise — and that sleep races the caller's context, so a deadline
// firing mid-backoff stops retrying immediately rather than waiting out the
// remaining delay. The returned error is always the last attempt's real
// error, never a bare context error: internal/proxy's writeProviderError
// requires a *provider.Error to build a meaningful response, and a plain
// context.DeadlineExceeded would read as an unclassified internal bug
// instead of the provider failure that actually caused it.
func Do[T any](ctx context.Context, cfg Config, log *slog.Logger, labels Labels, attempt func(ctx context.Context, n int) (T, error)) (result T, err error, attempts int) {
	for n := 1; n <= cfg.MaxAttempts; n++ {
		result, err = attempt(ctx, n)
		attempts = n
		if err == nil {
			return result, nil, attempts
		}

		if n == cfg.MaxAttempts {
			break
		}

		provErr := asProviderError(err)
		if provErr == nil || !provErr.Retryable {
			break
		}

		delay := backoffDelay(cfg.BaseDelay, n, provErr)
		log.LogAttrs(ctx, slog.LevelInfo, "retrying provider call",
			slog.String("provider", labels.Provider),
			slog.String("model", labels.Model),
			slog.Int("attempt", n),
			slog.Int("next_attempt", n+1),
			slog.Duration("delay", delay),
			slog.String("reason", string(provErr.Kind)),
		)

		select {
		case <-ctx.Done():
			return result, err, attempts
		case <-time.After(delay):
		}
	}

	return result, err, attempts
}

func asProviderError(err error) *provider.Error {
	var provErr *provider.Error
	if errors.As(err, &provErr) {
		return provErr
	}
	return nil
}

// backoffDelay honours the provider's own Retry-After when the failed
// attempt supplied one — cooperating with an upstream's stated recovery time
// beats guessing — and falls back to full jitter otherwise: sleep =
// rand(0, base * 2^n). Full jitter, rather than a fixed exponential delay, is
// what stops many clients that all started retrying at once from staying
// synchronized attempt after attempt and re-creating the exact load spike
// that triggered the backoff.
func backoffDelay(base time.Duration, n int, provErr *provider.Error) time.Duration {
	if provErr.RetryAfter > 0 {
		return provErr.RetryAfter
	}
	return fullJitter(base, n-1)
}

func fullJitter(base time.Duration, exponent int) time.Duration {
	if exponent < 0 {
		exponent = 0
	}
	if exponent > maxBackoffExponent {
		exponent = maxBackoffExponent
	}

	upper := base << uint(exponent)
	if upper <= 0 {
		return base
	}
	return time.Duration(rand.Int64N(int64(upper)))
}
