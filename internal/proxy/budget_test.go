package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/budget"
	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// newTestServerWithBudget is newTestServerFull's sibling for tests that need
// to control the budget and cost dependencies too, not just rate limiting —
// newTestServerFull hardcodes a permissive stub for both.
func newTestServerWithBudget(t *testing.T, resolver Resolver, authr Authenticator, limiter RateLimiter, budgetTracker BudgetTracker, calc CostCalculator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(resolver, authr, limiter, budgetTracker, calc, stubHealthRecorder{}, nil, nil, nil, noRetryConfig(t), nil, discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

// newLiveTracker connects to a real Redis for the tests in this file that
// need genuine Reserve/Reconcile settlement — stubBudgetTracker cannot
// fabricate a working *budget.Reservation, since its fields are unexported
// by design (see tracker.go). Skips rather than fails when no Redis is
// reachable, the same convention ratelimit_test.go's newLiveLimiter uses.
func newLiveTracker(t *testing.T) (*budget.Tracker, *redis.Client) {
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

	return budget.NewTracker(rdb), rdb
}

// budgetKeyFor mirrors tracker.go's unexported periodKey/currentPeriod, since
// this file is a different package and cannot call them directly.
func budgetKeyFor(teamID string) string {
	return "switchyard:budget:" + teamID + ":" + time.Now().UTC().Format("2006-01")
}

func cleanupTeamBudget(t *testing.T, rdb *redis.Client, teamID string) {
	t.Helper()
	t.Cleanup(func() {
		rdb.Del(context.Background(), budgetKeyFor(teamID))
	})
}

// --- pure helpers ---------------------------------------------------------

func TestUtilization(t *testing.T) {
	tests := map[string]struct {
		spent, cap int64
		want       float64
	}{
		"half spent":       {50, 100, 0.5},
		"nothing spent":    {0, 100, 0},
		"fully spent":      {100, 100, 1.0},
		"over cap":         {150, 100, 1.5},
		"zero cap is zero": {50, 0, 0},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := utilization(tc.spent, tc.cap); got != tc.want {
				t.Errorf("utilization(%d, %d) = %v, want %v", tc.spent, tc.cap, got, tc.want)
			}
		})
	}
}

func TestFormatUSD(t *testing.T) {
	tests := map[string]struct {
		micros int64
		want   string
	}{
		"whole dollar":     {1_000_000, "$1.00"},
		"fractional cents": {1_234_560, "$1.23"},
		"zero":             {0, "$0.00"},
		"under a cent":     {5_000, "$0.01"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := formatUSD(tc.micros); got != tc.want {
				t.Errorf("formatUSD(%d) = %q, want %q", tc.micros, got, tc.want)
			}
		})
	}
}

// --- budget check (handler.go's reserveBudget) -----------------------------

func TestBudgetDeniedIs402AndProviderNeverCalled(t *testing.T) {
	mock := &provider.Mock{ProviderName: "groq"}
	team := defaultTestTeam()
	team.MonthlyBudgetMicros = 10_000_000
	denied := &budget.Result{Allowed: false, SpentMicros: 9_500_000}

	srv := newTestServerWithBudget(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{}, stubBudgetTracker{result: denied}, stubCostCalculator{})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	if mock.Attempts() != 0 {
		t.Errorf("provider was called %d times, want 0: a budget denial must reject before the provider call", mock.Attempts())
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "budget_exceeded" {
		t.Errorf("error.type = %q, want budget_exceeded", body.Error.Type)
	}
}

// The plan's fail-*closed* decision for budget, the deliberate opposite of
// RPM/TPM's fail-open: an unreachable Redis must block the request rather
// than let it through, since money spent is not recoverable.
func TestBudgetFailsClosedOnRedisError(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Response:     &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"},
	}
	srv := newTestServerWithBudget(t, stubResolver{prov: mock}, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{}, stubBudgetTracker{err: context.DeadlineExceeded}, stubCostCalculator{})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a Redis failure must fail closed here, unlike rate limiting", resp.StatusCode)
	}
	if mock.Attempts() != 0 {
		t.Errorf("provider was called %d times, want 0", mock.Attempts())
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "budget_check_unavailable" {
		t.Errorf("error.type = %q, want budget_check_unavailable", body.Error.Type)
	}
}

func TestBudgetWarningHeaderAtOrAboveThreshold(t *testing.T) {
	mock := &provider.Mock{ProviderName: "groq", Response: &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"}}
	team := defaultTestTeam()
	team.MonthlyBudgetMicros = 10_000_000
	allowedNearCap := &budget.Result{Allowed: true, SpentMicros: 8_500_000} // 85%

	srv := newTestServerWithBudget(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{}, stubBudgetTracker{result: allowedNearCap}, stubCostCalculator{})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderBudgetWarning); got != "true" {
		t.Errorf("%s = %q, want \"true\"", HeaderBudgetWarning, got)
	}
}

func TestBudgetNoWarningHeaderBelowThreshold(t *testing.T) {
	mock := &provider.Mock{ProviderName: "groq", Response: &provider.Response{Content: "hi", FinishReason: provider.FinishStop, Model: "m", Provider: "groq"}}
	team := defaultTestTeam()
	team.MonthlyBudgetMicros = 10_000_000
	allowedComfortable := &budget.Result{Allowed: true, SpentMicros: 5_000_000} // 50%

	srv := newTestServerWithBudget(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{}, stubBudgetTracker{result: allowedComfortable}, stubCostCalculator{})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderBudgetWarning); got != "" {
		t.Errorf("%s = %q, want empty (below the warning threshold)", HeaderBudgetWarning, got)
	}
}

// A nil reservation — the denied-request case — must never panic when
// ChatCompletions' deferred Reconcile runs against it. stubBudgetTracker's
// default (nil result) already exercises the allowed-but-nil case through
// every other test in this package; this pins down the denied case
// explicitly.
func TestNilBudgetReservationReconcileNeverPanics(t *testing.T) {
	mock := &provider.Mock{ProviderName: "groq"}
	denied := &budget.Result{Allowed: false, SpentMicros: 100}

	srv := newTestServerWithBudget(t, stubResolver{prov: mock}, stubAuthenticator{team: defaultTestTeam()},
		stubRateLimiter{}, stubBudgetTracker{result: denied}, stubCostCalculator{})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
}

// --- real settle-up, against real Redis and real pricing --------------------

// stubBudgetTracker cannot fabricate a working *budget.Reservation — its
// fields are unexported by design — so the only way to prove the handler
// reserves a sane estimate and settles it down to the real cost is to run it
// against a real Tracker and a real Calculator. Priced so the up-front
// estimate (prompt tokens plus a 100-token max_tokens ceiling) and the real
// usage the mock returns (10 in, 5 out) land on visibly different costs.
func TestBudgetReservationSettlesAgainstRealUsage(t *testing.T) {
	tracker, rdb := newLiveTracker(t)

	team := &auth.Team{
		ID:                  "proxy-budget-settle-test",
		AllowedProviders:    []string{"groq"},
		AllowedModels:       []string{"m"},
		RateLimits:          auth.RateLimits{RPM: 1000, TPM: 1000},
		MonthlyBudgetMicros: 1_000_000,
	}
	cleanupTeamBudget(t, rdb, team.ID)

	calc := budget.NewCalculator(map[string]budget.Pricing{
		"m": {InputPer1M: 1_000_000, OutputPer1M: 2_000_000},
	})

	mock := &provider.Mock{
		ProviderName: "groq",
		Response: &provider.Response{
			Content: "hi", FinishReason: provider.FinishStop,
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
			Model: "m", Provider: "groq",
		},
	}

	srv := newTestServerWithBudget(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{}, tracker, calc)

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	// Real cost: 10*1 + 5*2 = 20 micros. Reconcile settles the counter down
	// to exactly that, regardless of how large the up-front estimate was.
	const want = int64(20)
	var got int64
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := rdb.Get(context.Background(), budgetKeyFor(team.ID)).Int64()
		if err != nil {
			t.Fatalf("reading settled spend: %v", err)
		}
		got = v
		if got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settled spend = %d, want %d", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Phase 4 checklist: a request that would push spend over the cap is denied
// before the provider is ever called, proven end to end against real Redis
// rather than a stub told what to answer.
func TestBudgetDeniedEndToEndAgainstRealRedis(t *testing.T) {
	tracker, rdb := newLiveTracker(t)

	team := &auth.Team{
		ID:                  "proxy-budget-deny-test",
		AllowedProviders:    []string{"groq"},
		AllowedModels:       []string{"m"},
		RateLimits:          auth.RateLimits{RPM: 1000, TPM: 1000},
		MonthlyBudgetMicros: 100, // 100 micro-dollars: even a tiny estimate exceeds it
	}
	cleanupTeamBudget(t, rdb, team.ID)

	calc := budget.NewCalculator(map[string]budget.Pricing{
		"m": {InputPer1M: 1_000_000, OutputPer1M: 1_000_000},
	})

	mock := &provider.Mock{ProviderName: "groq"} // must never be called

	srv := newTestServerWithBudget(t, stubResolver{prov: mock}, stubAuthenticator{team: team},
		stubRateLimiter{}, tracker, calc)

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 402: %s", resp.StatusCode, body)
	}
	if mock.Attempts() != 0 {
		t.Errorf("provider was called %d times, want 0", mock.Attempts())
	}

	// The denied attempt must have rolled back to nothing: no residue left in
	// the counter for a request that was never actually made.
	got, err := rdb.Get(context.Background(), budgetKeyFor(team.ID)).Int64()
	if err != nil {
		t.Fatalf("reading spend after denial: %v", err)
	}
	if got != 0 {
		t.Errorf("spend after denial = %d, want 0 (the denied reservation must roll back cleanly)", got)
	}
}
