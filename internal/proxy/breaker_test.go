package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
)

// Step 7.4's proxy half: an open breaker skips a candidate in chain
// resolution, without a provider call and without waiting out a timeout.
//
// These build real resilience.Breakers rather than fakes. The state machine
// is the thing whose interaction with the chain is under test, so replacing
// it with a stub would leave the actual integration unproven.

// testBreakerConfig mirrors internal/resilience's own test values, with a
// cooldown long enough that no test here accidentally crosses into HalfOpen.
func proxyBreakerConfig() resilience.BreakerConfig {
	return resilience.BreakerConfig{
		FailureThreshold: 2,
		Window:           time.Minute,
		CooldownBase:     time.Hour,
		CooldownMax:      time.Hour,
		SuccessThreshold: 1,
		ProbeTimeout:     time.Minute,
		StateCacheTTL:    time.Millisecond,
	}
}

func newProxyBreakerRegistry(t *testing.T) *resilience.BreakerRegistry {
	t.Helper()
	r, err := resilience.NewBreakerRegistry(proxyBreakerConfig(), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("NewBreakerRegistry() error: %v", err)
	}
	return r
}

// newTieredServerWithBreakers is newTieredServer plus a breaker registry.
func newTieredServerWithBreakers(t *testing.T, team *auth.Team, breakers Breakers, groq, ollama provider.Provider) *httptest.Server {
	t.Helper()

	resolver := stubResolver{
		byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": ollama},
		tier:    fastTier,
	}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: team}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, breakers, nil,
		noRetryConfig(t), nil, nil, discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

// openBreakerFor drives one provider+model's breaker to Open through the
// registry, so the handler and the test are talking about the same instance.
func openBreakerFor(t *testing.T, r *resilience.BreakerRegistry, cand resilience.Candidate) {
	t.Helper()
	b := r.For(resilience.Labels{Provider: cand.Provider, Model: cand.Model})
	ctx := context.Background()
	for i := 0; i < proxyBreakerConfig().FailureThreshold; i++ {
		b.RecordFailure(ctx)
	}
	if got := b.State(); got != resilience.StateOpen {
		t.Fatalf("breaker for %s/%s = %v, want Open", cand.Provider, cand.Model, got)
	}
}

// TestOpenBreakerSkipsTheCandidateWithoutCallingIt is the headline: the
// primary's breaker is open, so the request falls straight through to the
// fallback and the primary's mock is never touched.
func TestOpenBreakerSkipsTheCandidateWithoutCallingIt(t *testing.T) {
	groq := okMock("groq", "fast-a")
	ollama := okMock("ollama", "fast-b")

	registry := newProxyBreakerRegistry(t)
	openBreakerFor(t, registry, primary)

	srv := newTieredServerWithBreakers(t, tieredTeam(), registry, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the fallback", resp.StatusCode)
	}
	if got := groq.Attempts(); got != 0 {
		t.Errorf("primary was called %d times, want 0 — an open breaker means no attempt at all", got)
	}
	if got := ollama.Attempts(); got != 1 {
		t.Errorf("fallback was called %d times, want 1", got)
	}
	if got := resp.Header.Get(HeaderProvider); got != "ollama" {
		t.Errorf("%s = %q, want ollama", HeaderProvider, got)
	}
}

// TestOpenBreakerCostsNoTimeoutWait is the "no timeout wait" half of the
// bullet. The primary would take far longer than this assertion allows if it
// were actually called and left to hang.
func TestOpenBreakerCostsNoTimeoutWait(t *testing.T) {
	slow := &provider.Mock{ProviderName: "groq", Models: []string{"fast-a"}, Delay: 2 * time.Second}
	ollama := okMock("ollama", "fast-b")

	registry := newProxyBreakerRegistry(t)
	openBreakerFor(t, registry, primary)

	srv := newTieredServerWithBreakers(t, tieredTeam(), registry, slow, ollama)

	start := time.Now()
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	elapsed := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed >= slow.Delay {
		t.Errorf("request took %s, want well under the skipped provider's %s delay", elapsed, slow.Delay)
	}
}

// TestClosedBreakerDoesNotInterfere guards the obvious regression: with
// everything closed, the chain behaves exactly as it did before Step 7.4.
func TestClosedBreakerDoesNotInterfere(t *testing.T) {
	groq := okMock("groq", "fast-a")
	ollama := okMock("ollama", "fast-b")

	srv := newTieredServerWithBreakers(t, tieredTeam(), newProxyBreakerRegistry(t), groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := groq.Attempts(); got != 1 {
		t.Errorf("primary was called %d times, want 1", got)
	}
	if got := ollama.Attempts(); got != 0 {
		t.Errorf("fallback was called %d times, want 0", got)
	}
}

// TestProviderFailuresOpenTheBreakerAcrossRequests proves the loop closes:
// real request failures feed the breaker, and once it trips the next request
// stops attempting that candidate.
func TestProviderFailuresOpenTheBreakerAcrossRequests(t *testing.T) {
	groq := failingMock("groq", provider.KindServerError, true)
	ollama := okMock("ollama", "fast-b")

	registry := newProxyBreakerRegistry(t)
	srv := newTieredServerWithBreakers(t, tieredTeam(), registry, groq, ollama)

	// Two failing requests is proxyBreakerConfig's FailureThreshold.
	for i := 0; i < proxyBreakerConfig().FailureThreshold; i++ {
		resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
		resp.Body.Close()
	}

	attemptsBefore := groq.Attempts()
	if attemptsBefore == 0 {
		t.Fatalf("primary was never called, want the failures that open the breaker")
	}

	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if got := groq.Attempts(); got != attemptsBefore {
		t.Errorf("primary was called %d more times after the breaker opened, want 0", got-attemptsBefore)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — the fallback should serve while the primary is broken", resp.StatusCode)
	}
}

// TestEveryCandidateBreakerOpenReturns503 covers the exhausted-chain shape
// when nothing was actually called: the breakdown must say the candidates
// were skipped, not invent an upstream failure for them.
func TestEveryCandidateBreakerOpenReturns503(t *testing.T) {
	groq := okMock("groq", "fast-a")
	ollama := okMock("ollama", "fast-b")

	registry := newProxyBreakerRegistry(t)
	openBreakerFor(t, registry, primary)
	openBreakerFor(t, registry, fallback)

	srv := newTieredServerWithBreakers(t, tieredTeam(), registry, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := groq.Attempts() + ollama.Attempts(); got != 0 {
		t.Errorf("%d provider calls were made, want 0", got)
	}

	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body.Error.Attempts) != 2 {
		t.Fatalf("breakdown has %d entries, want 2", len(body.Error.Attempts))
	}
	for _, a := range body.Error.Attempts {
		if a.Type != "circuit_breaker_open" {
			t.Errorf("attempt %s/%s type = %q, want circuit_breaker_open", a.Provider, a.Model, a.Type)
		}
		if a.Attempts != 0 {
			t.Errorf("attempt %s/%s reports %d attempts, want 0 — nothing was called", a.Provider, a.Model, a.Attempts)
		}
	}
}

// TestSoleCandidateBreakerOpenReturns503 proves a one-candidate chain whose
// breaker is open reports 503 rather than the 500 an unclassified error would
// otherwise produce. The gateway working as designed is not an internal fault.
func TestSoleCandidateBreakerOpenReturns503(t *testing.T) {
	groq := okMock("groq", "fast-a")

	registry := newProxyBreakerRegistry(t)
	openBreakerFor(t, registry, primary)

	resolver := stubResolver{byModel: map[string]provider.Provider{"fast-a": groq}}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: tieredTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, registry, nil,
		noRetryConfig(t), nil, nil, discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)

	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Error.Type != "circuit_breaker_open" {
		t.Errorf("error type = %q, want circuit_breaker_open", body.Error.Type)
	}
	if got := groq.Attempts(); got != 0 {
		t.Errorf("provider was called %d times, want 0", got)
	}
}

// TestBreakerIsPerProviderAndModelEndToEnd is Step 7.4's granularity rule
// through the whole stack: an open breaker on one model must not stop the
// gateway serving that same provider's other model.
func TestBreakerIsPerProviderAndModelEndToEnd(t *testing.T) {
	// Both models on one provider instance, so anything keyed by provider
	// alone would skip them together.
	groq := &provider.Mock{ProviderName: "groq", Models: []string{"fast-a", "fast-b"}}

	registry := newProxyBreakerRegistry(t)
	openBreakerFor(t, registry, resilience.Candidate{Provider: "groq", Model: "fast-a"})

	resolver := stubResolver{byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": groq}}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: tieredTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, registry, nil,
		noRetryConfig(t), nil, nil, discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)

	resp := post(t, srv, `{"model":"fast-b","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — fast-b's breaker is closed", resp.StatusCode)
	}
	if got := groq.Attempts(); got != 1 {
		t.Errorf("provider was called %d times, want 1", got)
	}
}
