package resilience

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// testConfig gives the chain-wide budget the same value as the per-provider
// cap: every test in this file exercises Do against a single provider, where
// the two are equivalent. Step 6.3's budget-splitting behaviour is tested
// where it actually applies, against a real chain in internal/proxy.
func testConfig(t *testing.T, maxAttempts int, baseDelay time.Duration) Config {
	t.Helper()
	cfg, err := NewConfig(maxAttempts, baseDelay, maxAttempts)
	if err != nil {
		t.Fatalf("NewConfig() error: %v", err)
	}
	return cfg
}

func TestNewConfigValidation(t *testing.T) {
	tests := map[string]struct {
		maxAttempts int
		baseDelay   time.Duration
		maxTotal    int
		wantErr     bool
	}{
		"valid":               {maxAttempts: 3, baseDelay: 50 * time.Millisecond, maxTotal: 5},
		"zero attempts":       {maxAttempts: 0, baseDelay: 50 * time.Millisecond, maxTotal: 5, wantErr: true},
		"negative attempts":   {maxAttempts: -1, baseDelay: 50 * time.Millisecond, maxTotal: 5, wantErr: true},
		"one attempt is ok":   {maxAttempts: 1, baseDelay: 50 * time.Millisecond, maxTotal: 1},
		"zero base delay":     {maxAttempts: 3, baseDelay: 0, maxTotal: 5, wantErr: true},
		"negative base delay": {maxAttempts: 3, baseDelay: -time.Millisecond, maxTotal: 5, wantErr: true},
		// Step 6.3: a total below the per-provider cap would make MaxAttempts
		// unreachable, so the two settings would describe different policies.
		"total below per-provider cap": {maxAttempts: 3, baseDelay: time.Millisecond, maxTotal: 2, wantErr: true},
		"total equal to cap is ok":     {maxAttempts: 3, baseDelay: time.Millisecond, maxTotal: 3},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewConfig(tt.maxAttempts, tt.baseDelay, tt.maxTotal)
			if tt.wantErr && err == nil {
				t.Fatalf("NewConfig() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("NewConfig() unexpected error: %v", err)
			}
		})
	}
}

// retryableErr and permanentErr are the two *provider.Error shapes Step 6.1's
// classification rule turns on: Retryable true is always eligible, false
// never is, regardless of Kind.
func retryableErr(kind provider.Kind) *provider.Error {
	return &provider.Error{Kind: kind, Provider: "p", Retryable: true}
}

func permanentErr(kind provider.Kind) *provider.Error {
	return &provider.Error{Kind: kind, Provider: "p", Retryable: false}
}

func TestDoSucceedsOnFirstAttempt(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, 3, time.Millisecond)
	calls := 0

	result, err, attempts := Do(ctx, cfg, discardLog(), nil, Labels{Provider: "p"}, func(ctx context.Context, n int) (string, error) {
		calls++
		return "ok", nil
	})

	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if result != "ok" {
		t.Errorf("result = %q, want ok", result)
	}
	if attempts != 1 || calls != 1 {
		t.Errorf("attempts = %d, calls = %d, want 1 and 1", attempts, calls)
	}
}

// TestDoNeverRetriesAPermanentFailure is Step 6.1's "auth failure is never
// retried" checklist item at the unit level: exactly one attempt, regardless
// of MaxAttempts.
func TestDoNeverRetriesAPermanentFailure(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, 3, time.Millisecond)
	calls := 0

	_, err, attempts := Do(ctx, cfg, discardLog(), nil, Labels{Provider: "p"}, func(ctx context.Context, n int) (string, error) {
		calls++
		return "", permanentErr(provider.KindAuthFailed)
	})

	if err == nil {
		t.Fatalf("Do() error = nil, want the permanent failure")
	}
	if attempts != 1 || calls != 1 {
		t.Errorf("attempts = %d, calls = %d, want exactly 1 (no retry)", attempts, calls)
	}
}

// TestDoRetriesUntilSuccess proves a retryable failure (429, 5xx, timeout,
// network error all share Retryable=true) is retried and a later success is
// returned once it happens.
func TestDoRetriesUntilSuccess(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, 3, time.Millisecond)
	calls := 0

	result, err, attempts := Do(ctx, cfg, discardLog(), nil, Labels{Provider: "p"}, func(ctx context.Context, n int) (string, error) {
		calls++
		if n < 3 {
			return "", retryableErr(provider.KindRateLimited)
		}
		return "ok", nil
	})

	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if result != "ok" {
		t.Errorf("result = %q, want ok", result)
	}
	if attempts != 3 || calls != 3 {
		t.Errorf("attempts = %d, calls = %d, want 3 and 3", attempts, calls)
	}
}

// TestDoCapsAtMaxAttempts proves the retry loop stops at cfg.MaxAttempts even
// though every attempt keeps failing retryably, and that the error returned
// is the real last attempt's *provider.Error, not a generic "gave up."
func TestDoCapsAtMaxAttempts(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, 3, time.Millisecond)
	calls := 0

	_, err, attempts := Do(ctx, cfg, discardLog(), nil, Labels{Provider: "p"}, func(ctx context.Context, n int) (string, error) {
		calls++
		return "", retryableErr(provider.KindServerError)
	})

	if attempts != 3 || calls != 3 {
		t.Fatalf("attempts = %d, calls = %d, want capped at 3", attempts, calls)
	}
	var provErr *provider.Error
	if !errors.As(err, &provErr) || provErr.Kind != provider.KindServerError {
		t.Errorf("err = %v, want the last attempt's *provider.Error (KindServerError)", err)
	}
}

// TestDoHonoursRetryAfterOverComputedBackoff proves a provider's own
// Retry-After wins over full jitter — set large enough that the test would
// time out if fullJitter's much smaller BaseDelay were used instead, and
// small enough the test itself stays fast.
func TestDoHonoursRetryAfterOverComputedBackoff(t *testing.T) {
	ctx := context.Background()
	// BaseDelay picked far smaller than RetryAfter: if Do used it instead,
	// this test would very likely observe an elapsed time well under
	// RetryAfter and fail the lower-bound assertion below.
	cfg := testConfig(t, 2, time.Microsecond)
	retryAfter := 40 * time.Millisecond

	start := time.Now()
	_, _, attempts := Do(ctx, cfg, discardLog(), nil, Labels{Provider: "p"}, func(ctx context.Context, n int) (string, error) {
		if n == 1 {
			return "", &provider.Error{Kind: provider.KindRateLimited, Retryable: true, RetryAfter: retryAfter}
		}
		return "ok", nil
	})
	elapsed := time.Since(start)

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if elapsed < retryAfter {
		t.Errorf("elapsed = %v, want at least RetryAfter (%v) — the provider's own backoff should have been honoured", elapsed, retryAfter)
	}
}

// TestDoStopsAtContextDeadlineDuringBackoff proves a deadline firing mid-sleep
// stops the retry loop immediately, without waiting out the rest of the
// backoff delay, and that the error returned is still the real provider
// error rather than a bare context error — writeProviderError needs a
// *provider.Error to build a meaningful response.
func TestDoStopsAtContextDeadlineDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// A backoff long enough that the 20ms deadline will fire first.
	cfg := testConfig(t, 5, time.Second)
	calls := 0

	start := time.Now()
	_, err, attempts := Do(ctx, cfg, discardLog(), nil, Labels{Provider: "p"}, func(ctx context.Context, n int) (string, error) {
		calls++
		return "", retryableErr(provider.KindServerError)
	})
	elapsed := time.Since(start)

	if attempts != 1 || calls != 1 {
		t.Fatalf("attempts = %d, calls = %d, want exactly 1 — the deadline should fire during the first backoff sleep", attempts, calls)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %v, want well under the 1s backoff — Do must not wait out the full delay past the deadline", elapsed)
	}
	var provErr *provider.Error
	if !errors.As(err, &provErr) || provErr.Kind != provider.KindServerError {
		t.Errorf("err = %v, want the real last attempt's *provider.Error, not a bare context error", err)
	}
}

// TestDoUnclassifiedErrorIsNotRetried proves an error that isn't a
// *provider.Error at all — a bug elsewhere, not a provider fault — is
// treated as non-retryable, the safe default per Step 6.1's "retry only when
// Retryable == true."
func TestDoUnclassifiedErrorIsNotRetried(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, 3, time.Millisecond)
	calls := 0

	_, err, attempts := Do(ctx, cfg, discardLog(), nil, Labels{Provider: "p"}, func(ctx context.Context, n int) (string, error) {
		calls++
		return "", errors.New("not a provider error")
	})

	if err == nil {
		t.Fatalf("Do() error = nil, want the unclassified error")
	}
	if attempts != 1 || calls != 1 {
		t.Errorf("attempts = %d, calls = %d, want exactly 1", attempts, calls)
	}
}

// TestFullJitterStaysWithinBounds proves the jittered delay is always in
// [0, base*2^n) — never negative, never at or above the theoretical
// ceiling — across a spread of exponents, and that repeated calls are not
// all identical (proving it is in fact randomized, not a fixed value dressed
// up as jitter).
func TestFullJitterStaysWithinBounds(t *testing.T) {
	base := 10 * time.Millisecond

	for exponent := 0; exponent <= 4; exponent++ {
		ceiling := base << uint(exponent)
		seen := map[time.Duration]bool{}
		for i := 0; i < 50; i++ {
			d := fullJitter(base, exponent)
			if d < 0 || d >= ceiling {
				t.Fatalf("fullJitter(%v, %d) = %v, want in [0, %v)", base, exponent, d, ceiling)
			}
			seen[d] = true
		}
		if exponent > 0 && len(seen) < 2 {
			t.Errorf("fullJitter(%v, %d) returned the same value across 50 calls, want randomized spread", base, exponent)
		}
	}
}

func TestFullJitterExponentIsClamped(t *testing.T) {
	// A very large exponent must not overflow into a negative or nonsensical
	// duration — it should behave as if clamped to maxBackoffExponent.
	d := fullJitter(time.Millisecond, 10_000)
	if d < 0 {
		t.Errorf("fullJitter with a huge exponent = %v, want non-negative", d)
	}
}
