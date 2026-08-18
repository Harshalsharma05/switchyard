package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
)

// newLiveLimiter connects to a real Redis for the tests in this file that
// need genuine Reserve/Reconcile settlement — stubRateLimiter cannot
// fabricate a working *ratelimit.Reservation, since its fields are
// unexported by design (see bucket.go). Skips rather than fails when no
// Redis is reachable, the same convention ratelimit's own tests use.
func newLiveLimiter(t *testing.T) (*ratelimit.Limiter, *redis.Client) {
	t.Helper()

	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		rdb.Close()
		t.Skipf("no Redis reachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { rdb.Close() })

	return ratelimit.NewLimiter(rdb), rdb
}

// cleanupTeamBuckets deletes a test team's RPM and TPM keys so one test's
// leftover state can never bleed into another's.
func cleanupTeamBuckets(t *testing.T, rdb *redis.Client, teamID string) {
	t.Helper()
	t.Cleanup(func() {
		rdb.Del(context.Background(), "switchyard:rl:"+teamID+":rpm", "switchyard:rl:"+teamID+":tpm")
	})
}

// --- priority shedding (Step 3.5) ----------------------------------------

func TestShouldShedForPriority(t *testing.T) {
	tests := map[string]struct {
		priority  auth.Priority
		capacity  int
		remaining float64
		want      bool
	}{
		"batch well below floor is shed":     {auth.PriorityBatch, 100, 5, true},
		"batch just below floor is shed":     {auth.PriorityBatch, 100, 19.99, true},
		"batch exactly at floor is not shed": {auth.PriorityBatch, 100, 20, false},
		"batch well above floor is not shed": {auth.PriorityBatch, 100, 80, false},
		"realtime near zero is never shed":   {auth.PriorityRealtime, 100, 1, false},
		"realtime at zero is never shed":     {auth.PriorityRealtime, 100, 0, false},
		"zero capacity never sheds":          {auth.PriorityBatch, 0, 0, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			team := &auth.Team{Priority: tc.priority}
			got := shouldShedForPriority(team, tc.capacity, ratelimit.Result{Remaining: tc.remaining})
			if got != tc.want {
				t.Errorf("shouldShedForPriority = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRateLimitShedsBatchPriorityNearLimit(t *testing.T) {
	team := &auth.Team{ID: "batch-team", Priority: auth.PriorityBatch, AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"m"}, RateLimits: auth.RateLimits{RPM: 100}}
	// Bucket admitted the request (Allowed: true) but is already at 5%
	// remaining — well past the 20% shed floor.
	allowedButLow := &ratelimit.Result{Allowed: true, Remaining: 5}

	srv := newTestServerFull(t, stubResolver{prov: &provider.Mock{}}, stubAuthenticator{team: team},
		stubRateLimiter{consumeResult: allowedButLow})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "priority_shed" {
		t.Errorf("error.type = %q, want priority_shed", body.Error.Type)
	}
}

func TestRateLimitDoesNotShedRealtimeNearLimit(t *testing.T) {
	team := &auth.Team{ID: "realtime-team", Priority: auth.PriorityRealtime, AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"m"}, RateLimits: auth.RateLimits{RPM: 100}}
	// Same 5% remaining that gets a batch team shed above — realtime must
	// still go through, since the bucket itself allowed it.
	allowedButLow := &ratelimit.Result{Allowed: true, Remaining: 5}

	mock := &provider.Mock{ProviderName: "groq", Response: &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"}}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{consumeResult: allowedButLow})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: realtime priority must not be shed early", resp.StatusCode)
	}
}

func TestRateLimitDoesNotShedBatchAboveFloor(t *testing.T) {
	team := &auth.Team{ID: "batch-team", Priority: auth.PriorityBatch, AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"m"}, RateLimits: auth.RateLimits{RPM: 100}}
	// 50% remaining is comfortably above the 20% floor.
	allowedAndHealthy := &ratelimit.Result{Allowed: true, Remaining: 50}

	mock := &provider.Mock{ProviderName: "groq", Response: &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"}}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{consumeResult: allowedAndHealthy})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: 50%% remaining is above the shed floor", resp.StatusCode)
	}
}

func TestTPMShedsBatchPriorityNearLimitAndNeverCallsProvider(t *testing.T) {
	team := &auth.Team{ID: "batch-team", Priority: auth.PriorityBatch, AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"m"}, RateLimits: auth.RateLimits{TPM: 1000}}
	allowedButLow := &ratelimit.Result{Allowed: true, Remaining: 50} // 5% of 1000

	mock := &provider.Mock{ProviderName: "groq"}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{reserveResult: allowedButLow})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if mock.Attempts() != 0 {
		t.Errorf("provider was called %d times, want 0: a priority shed must reject before the provider call", mock.Attempts())
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "priority_shed" {
		t.Errorf("error.type = %q, want priority_shed", body.Error.Type)
	}
}

// Live-Redis proof of the asymmetry documented in DECISIONS.md: an RPM shed
// leaves its 1 consumed token spent, but a TPM shed refunds the whole
// reservation, since that reservation is a ceiling that nothing was ever
// generated against.
func TestTPMPriorityShedRefundsTheFullReservation(t *testing.T) {
	limiter, rdb := newLiveLimiter(t)

	team := &auth.Team{
		ID:               "proxy-priority-shed-test",
		Priority:         auth.PriorityBatch,
		AllowedProviders: []string{"groq"},
		AllowedModels:    []string{"m"},
		RateLimits:       auth.RateLimits{RPM: 1000, TPM: 1000},
	}
	cleanupTeamBuckets(t, rdb, team.ID)

	// Manually drain the TPM bucket down to 10% (100 of 1000) so the very
	// next reservation — however small — lands below the 20% shed floor.
	if _, err := limiter.Consume(context.Background(), team.ID, ratelimit.TPM, 1000, 900, time.Minute); err != nil {
		t.Fatalf("priming TPM bucket: %v", err)
	}

	mock := &provider.Mock{ProviderName: "groq"} // must never be called
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: team}, limiter)

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, body)
	}
	if mock.Attempts() != 0 {
		t.Errorf("provider was called %d times, want 0", mock.Attempts())
	}

	// The reservation this request took must have been fully refunded: the
	// bucket should still read ~100 (the 900 drained earlier, nothing more),
	// not 100-minus-the-reservation-this-request-took.
	var remaining float64
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := limiter.Consume(context.Background(), team.ID, ratelimit.TPM, 1000, 1, time.Minute)
		if err != nil {
			t.Fatalf("probing TPM bucket: %v", err)
		}
		remaining = probe.Remaining
		if remaining >= 95 && remaining <= 100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TPM bucket remaining = %v after a shed reservation, want ~99 (100 primed - 1 probe): the shed reservation was not refunded", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- RPM middleware -----------------------------------------------------

func TestRateLimitAllowsWithinCapacity(t *testing.T) {
	srv := newTestServerFull(t, stubResolver{prov: &provider.Mock{}}, stubAuthenticator{team: defaultTestTeam()}, stubRateLimiter{})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRateLimitDeniedRPMIs429WithHeaders(t *testing.T) {
	denied := &ratelimit.Result{Allowed: false, Remaining: 0, RetryAfter: 30 * time.Second}
	srv := newTestServerFull(t, stubResolver{prov: &provider.Mock{}}, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{consumeResult: denied})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if got := resp.Header.Get("X-RateLimit-Limit"); got == "" {
		t.Error("X-RateLimit-Limit header missing")
	}
	if got := resp.Header.Get("X-RateLimit-Reset"); got == "" {
		t.Error("X-RateLimit-Reset header missing")
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "rate_limit_exceeded" {
		t.Errorf("error.type = %q, want rate_limit_exceeded", body.Error.Type)
	}
}

// The plan's fail-open decision for a Redis-down RPM check: the gateway must
// never be the reason a request fails, so a Consume error lets the request
// through rather than blocking it.
func TestRateLimitFailsOpenOnRedisError(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Response:     &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"},
	}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{consumeErr: context.DeadlineExceeded})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a Redis failure must fail open, not block the request", resp.StatusCode)
	}
}

// RPM has to reject before the model allowlist and provider resolution even
// run — a resolver that errors if called is what proves the ordering, the
// same technique the Step 3.1 403 test uses.
func TestRateLimitDeniedRunsBeforeResolve(t *testing.T) {
	denied := &ratelimit.Result{Allowed: false, RetryAfter: time.Second}
	mustNotResolve := stubResolver{err: context.DeadlineExceeded}
	srv := newTestServerFull(t, mustNotResolve, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{consumeResult: denied})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
}

// --- TPM reservation (handler.go) ----------------------------------------

func TestTPMDeniedIs429AndProviderNeverCalled(t *testing.T) {
	mock := &provider.Mock{ProviderName: "groq"}
	denied := &ratelimit.Result{Allowed: false, Remaining: -5, RetryAfter: 12 * time.Second}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{reserveResult: denied})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0 (a negative balance must not be shown to the client)", got)
	}
	if mock.Attempts() != 0 {
		t.Errorf("provider was called %d times, want 0: a TPM denial must reject before the provider call", mock.Attempts())
	}
}

func TestTPMFailsOpenOnRedisError(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Response:     &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"},
	}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{reserveErr: context.DeadlineExceeded})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a Redis failure must fail open", resp.StatusCode)
	}
	if mock.Attempts() != 1 {
		t.Errorf("provider was called %d times, want 1", mock.Attempts())
	}
}

// A nil reservation — the fail-open and denied-request case alike — must
// never panic when ChatCompletions' deferred Reconcile runs against it.
// Every test above already exercises this implicitly (stubRateLimiter always
// returns nil), but this one pins it down directly against the streaming
// path too, which has its own extra exit points.
func TestNilReservationReconcileNeverPanics(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		StreamChunks: []*provider.Chunk{
			{Content: "hi", FinishReason: provider.FinishStop, Usage: &provider.Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: defaultTestTeam()}, stubRateLimiter{})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// --- real settle-up, against real Redis ----------------------------------

// stubRateLimiter cannot fabricate a working *ratelimit.Reservation — its
// fields are unexported by design (see bucket.go) — so the only way to prove
// the handler reserves the right ceiling and gives back the right unused
// amount is to run it against a real Limiter. Skips if Redis is unreachable,
// the same as ratelimit's own tests.
func TestTPMReservationSettlesAgainstRealUsage(t *testing.T) {
	limiter, rdb := newLiveLimiter(t)

	team := &auth.Team{
		ID:               "proxy-tpm-settle-test",
		AllowedProviders: []string{"groq"},
		AllowedModels:    []string{"m"},
		RateLimits:       auth.RateLimits{RPM: 1000, TPM: 1000},
	}
	cleanupTeamBuckets(t, rdb, team.ID)

	// Real usage will be 10 + 5 = 15 tokens. max_tokens: 100 plus a short
	// prompt reserves well above that, so the response should show most of
	// the reservation returned.
	mock := &provider.Mock{
		ProviderName: "groq",
		Response: &provider.Response{
			Content:      "hi",
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 10, OutputTokens: 5},
			Model:        "m",
			Provider:     "groq",
		},
	}

	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: team}, limiter)

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	// The deferred Reconcile runs inside ChatCompletions before it returns to
	// net/http, which flushes the response only once the handler has
	// returned — so by the time post() gets a response at all, Reconcile has
	// already completed. Polled anyway, briefly, so this test does not depend
	// on that buffering detail holding forever.
	var remaining float64
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := limiter.Consume(context.Background(), team.ID, ratelimit.TPM, team.RateLimits.TPM, 1, time.Minute)
		if err != nil {
			t.Fatalf("probing TPM bucket: %v", err)
		}
		remaining = probe.Remaining
		// Reserved ~100+ up front (10 prompt tokens plus max_tokens:100,
		// rounded up), used 15, so the bucket should have given back the
		// difference: capacity 1000 - 15 actual - 1 probe = 984.
		if remaining >= 980 && remaining <= 986 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TPM bucket remaining = %v after settling, want ~984 (1000 - 15 actual - 1 probe)", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Phase 3 checklist: "Failed request returns its token reservation (verify
// in redis-cli)." A provider error on the non-streaming path means
// actualTokens is never reassigned away from its zero value, so the deferred
// Reconcile in ChatCompletions gives the whole reservation back — proven
// here against real Redis rather than trusted from reading the code.
func TestFailedRequestReturnsFullReservation(t *testing.T) {
	limiter, rdb := newLiveLimiter(t)

	team := &auth.Team{
		ID:               "proxy-failed-request-refund-test",
		AllowedProviders: []string{"groq"},
		AllowedModels:    []string{"m"},
		RateLimits:       auth.RateLimits{RPM: 1000, TPM: 1000},
	}
	cleanupTeamBuckets(t, rdb, team.ID)

	mock := &provider.Mock{
		ProviderName: "groq",
		Err:          &provider.Error{Kind: provider.KindServerError, Provider: "groq", Message: "upstream exploded"},
	}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: team}, limiter)

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":200}`)
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, body)
	}

	// A ~205-token reservation (prompt estimate + max_tokens:200) was taken
	// and must come all the way back: the provider never generated anything
	// to charge for.
	var remaining float64
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := limiter.Consume(context.Background(), team.ID, ratelimit.TPM, team.RateLimits.TPM, 1, time.Minute)
		if err != nil {
			t.Fatalf("probing TPM bucket: %v", err)
		}
		remaining = probe.Remaining
		if remaining >= 995 && remaining <= 999 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TPM bucket remaining = %v after a failed request, want ~999 (1000 - 1 probe): the reservation was not fully returned", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Phase 3 checklist: "Exceeding TPM → 429, and a large max_tokens reserves
// correctly." A small max_tokens fits the team's TPM bucket; a large one on
// an otherwise-identical fresh bucket does not — proving the estimate really
// does scale with max_tokens and really does drive the admission decision,
// not just the arithmetic TestEstimateTokens checks in isolation.
func TestLargeMaxTokensExceedsTPMAndIsDenied(t *testing.T) {
	limiter, rdb := newLiveLimiter(t)

	smallTeam := &auth.Team{
		ID: "proxy-large-max-tokens-small", AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"m"},
		RateLimits: auth.RateLimits{RPM: 1000, TPM: 50},
	}
	largeTeam := &auth.Team{
		ID: "proxy-large-max-tokens-large", AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"m"},
		RateLimits: auth.RateLimits{RPM: 1000, TPM: 50},
	}
	cleanupTeamBuckets(t, rdb, smallTeam.ID)
	cleanupTeamBuckets(t, rdb, largeTeam.ID)

	mock := &provider.Mock{
		ProviderName: "groq",
		Response:     &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}, Model: "m", Provider: "groq"},
	}

	// Same 50-token bucket, same prompt. Only max_tokens differs.
	smallSrv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: smallTeam}, limiter)
	resp := post(t, smallSrv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("small max_tokens: status = %d, want 200: %s", resp.StatusCode, body)
	}

	largeSrv := newTestServerFull(t, stubResolver{prov: &provider.Mock{ProviderName: "groq"}}, stubAuthenticator{team: largeTeam}, limiter)
	resp = post(t, largeSrv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("large max_tokens: status = %d, want 429: %s", resp.StatusCode, body)
	}
}

// Phase 3 checklist: "Two concurrent clients on one team share one bucket
// correctly (run 50 parallel requests, count how many got through — the
// number should match the limit, not exceed it)." ratelimit's own
// TestConsumeConcurrentIsExact proves this at the bucket layer; this is the
// same claim proven through the full HTTP stack — router, Auth, and the RPM
// middleware included — against a real Redis. The provider is a
// zero-latency mock on purpose: real Ollama latency (seconds per call) gives
// the bucket time to refill mid-burst, which is exactly what made the
// live-curl version of this check unreliable earlier in this phase.
func TestConcurrentRequestsThroughHTTPRespectSharedRPMBucket(t *testing.T) {
	limiter, rdb := newLiveLimiter(t)

	const capacity = 50
	const attempts = 200

	team := &auth.Team{
		ID: "proxy-concurrent-rpm-test", AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"m"},
		RateLimits: auth.RateLimits{RPM: capacity, TPM: 1_000_000},
	}
	cleanupTeamBuckets(t, rdb, team.ID)

	mock := &provider.Mock{
		ProviderName: "groq",
		Response:     &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"},
	}
	srv := newTestServerFull(t, stubResolver{prov: mock}, stubAuthenticator{team: team}, limiter)

	var wg sync.WaitGroup
	var allowed, denied int64
	wg.Add(attempts)
	for range attempts {
		go func() {
			defer wg.Done()
			resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			switch resp.StatusCode {
			case http.StatusOK:
				atomic.AddInt64(&allowed, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&denied, 1)
			default:
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if allowed != capacity {
		t.Errorf("allowed %d of %d concurrent requests against a %d-capacity bucket, want exactly %d",
			allowed, attempts, capacity, capacity)
	}
	if allowed+denied != attempts {
		t.Errorf("allowed(%d) + denied(%d) = %d, want %d: some request got neither 200 nor 429", allowed, denied, allowed+denied, attempts)
	}
}
