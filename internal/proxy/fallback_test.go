package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/budget"
	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
)

// stubHealthOracle is the fake behind HealthOracle. An unnamed provider reads
// as healthy, which is both health.Status's zero value and the optimistic
// default the real Monitor applies to a name it does not track.
type stubHealthOracle struct {
	status map[string]health.Status
}

func (s stubHealthOracle) Status(providerName string) health.Status {
	return s.status[providerName]
}

// fastTier is the two-provider chain every test below routes against:
// "fast-a" on groq first, "fast-b" on ollama behind it.
var (
	primary  = resilience.Candidate{Provider: "groq", Model: "fast-a"}
	fallback = resilience.Candidate{Provider: "ollama", Model: "fast-b"}
	fastTier = []resilience.Candidate{primary, fallback}
)

// tieredTeam may use both providers and both models — the permissive case, so
// a test that is about health or failure ordering is not accidentally also
// about the allowlist.
func tieredTeam() *auth.Team {
	return &auth.Team{
		ID:               "tiered-team",
		AllowedProviders: []string{"groq", "ollama"},
		AllowedModels:    []string{"fast-a", "fast-b"},
	}
}

// newTieredServer wires a resolver over two mocks that share one tier, plus
// an optional health oracle.
func newTieredServer(t *testing.T, team *auth.Team, oracle HealthOracle, groq, ollama provider.Provider) *httptest.Server {
	t.Helper()

	resolver := stubResolver{
		byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": ollama},
		tier:    fastTier,
	}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: team}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, oracle,
		noRetryConfig(t), discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

func okMock(name, model string) *provider.Mock {
	return &provider.Mock{
		ProviderName: name,
		Response: &provider.Response{
			Content:      "hi",
			FinishReason: provider.FinishStop,
			Model:        model,
			Provider:     name,
		},
	}
}

// TestFallbackOnPrimaryFailure is Step 6.2's core case: the requested model's
// provider fails, the next entry in its tier serves the request, and the
// caller gets a 200 that says so.
func TestFallbackOnPrimaryFailure(t *testing.T) {
	groq := &provider.Mock{
		ProviderName: "groq",
		Err:          &provider.Error{Kind: provider.KindServerError, Provider: "groq", Retryable: true},
	}
	ollama := okMock("ollama", "fast-b")

	srv := newTieredServer(t, tieredTeam(), nil, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the fallback candidate should have served this", resp.StatusCode)
	}
	if groq.Attempts() != 1 {
		t.Errorf("primary attempts = %d, want 1", groq.Attempts())
	}
	if ollama.Attempts() != 1 {
		t.Errorf("fallback attempts = %d, want 1", ollama.Attempts())
	}

	wantHeaders := map[string]string{
		HeaderRequestedModel: "fast-a",
		HeaderServedModel:    "fast-b",
		HeaderProvider:       "ollama",
		HeaderFallback:       "true",
	}
	for header, want := range wantHeaders {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// The outbound request must name the model the fallback provider actually
	// serves, not the one the caller asked for.
	if reqs := ollama.Requests(); len(reqs) != 1 || reqs[0].Model != "fast-b" {
		t.Errorf("fallback received model %v, want fast-b", reqs)
	}
}

// TestNoFallbackHeadersOnDirectServe covers the ordinary path: the requested
// model served it, so X-Switchyard-Fallback says false rather than being
// absent — a caller should not have to read an absent header as a negative.
func TestNoFallbackHeadersOnDirectServe(t *testing.T) {
	groq := okMock("groq", "fast-a")
	ollama := okMock("ollama", "fast-b")

	srv := newTieredServer(t, tieredTeam(), nil, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderFallback); got != "false" {
		t.Errorf("%s = %q, want false", HeaderFallback, got)
	}
	if got := resp.Header.Get(HeaderServedModel); got != "fast-a" {
		t.Errorf("%s = %q, want fast-a", HeaderServedModel, got)
	}
	if ollama.Attempts() != 0 {
		t.Errorf("fallback attempts = %d, want 0: nothing failed", ollama.Attempts())
	}
}

// TestFallbackSkipsDownProvider proves the health signal reorders the chain
// before any call is made: a down primary is not even tried when a healthy
// alternative exists.
func TestFallbackSkipsDownProvider(t *testing.T) {
	groq := okMock("groq", "fast-a")
	ollama := okMock("ollama", "fast-b")
	oracle := stubHealthOracle{status: map[string]health.Status{"groq": health.StatusDown}}

	srv := newTieredServer(t, tieredTeam(), oracle, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if groq.Attempts() != 0 {
		t.Errorf("down provider attempts = %d, want 0: a down provider must be skipped, not tried", groq.Attempts())
	}
	if got := resp.Header.Get(HeaderProvider); got != "ollama" {
		t.Errorf("%s = %q, want ollama", HeaderProvider, got)
	}
}

// TestFallbackRespectsTeamAllowlist is the compliance case from Step 6.2: a
// team not permitted to use the fallback provider gets an error rather than a
// response from a provider it may not use — even though that provider is up
// and would have answered.
func TestFallbackRespectsTeamAllowlist(t *testing.T) {
	groq := &provider.Mock{
		ProviderName: "groq",
		Err:          &provider.Error{Kind: provider.KindServerError, Provider: "groq", Retryable: true},
	}
	ollama := okMock("ollama", "fast-b")

	restricted := &auth.Team{
		ID:               "groq-only",
		AllowedProviders: []string{"groq"},
		AllowedModels:    []string{"fast-a"},
	}

	srv := newTieredServer(t, restricted, nil, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = %d, want an error: the only healthy option is one this team may not use", resp.StatusCode)
	}
	if ollama.Attempts() != 0 {
		t.Fatalf("forbidden provider attempts = %d, want 0", ollama.Attempts())
	}
}

// TestNoTierMeansNoFallback keeps the pre-Phase-6 behaviour intact for a
// model that belongs to no tier: one provider, one failure, one error.
func TestNoTierMeansNoFallback(t *testing.T) {
	groq := &provider.Mock{
		ProviderName: "groq",
		Err:          &provider.Error{Kind: provider.KindServerError, Provider: "groq", Retryable: true},
	}
	ollama := okMock("ollama", "fast-b")

	resolver := stubResolver{byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": ollama}}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: tieredTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil,
		noRetryConfig(t), discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)

	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if ollama.Attempts() != 0 {
		t.Errorf("untiered fallback attempts = %d, want 0", ollama.Attempts())
	}
}

// failingMock always returns kind, so a chain walk against it is guaranteed
// to exhaust.
func failingMock(name string, kind provider.Kind, retryable bool) *provider.Mock {
	return &provider.Mock{
		ProviderName: name,
		Err: &provider.Error{
			Kind:      kind,
			Provider:  name,
			Message:   string(kind) + " from " + name,
			Retryable: retryable,
		},
	}
}

// newTieredServerWithRetry is newTieredServer with a caller-chosen retry
// policy, for the Step 6.3 tests that are specifically about how attempts are
// budgeted across the chain.
func newTieredServerWithRetry(t *testing.T, cfg resilience.Config, groq, ollama provider.Provider) *httptest.Server {
	t.Helper()

	resolver := stubResolver{
		byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": ollama},
		tier:    fastTier,
	}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: tieredTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil,
		cfg, discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

// TestChainExhaustionReturns503WithBreakdown is Step 6.3's error contract: a
// useful error beats a generic one, so the body names every candidate tried
// and why each failed.
func TestChainExhaustionReturns503WithBreakdown(t *testing.T) {
	groq := failingMock("groq", provider.KindServerError, true)
	ollama := failingMock("ollama", provider.KindRateLimited, true)

	srv := newTieredServer(t, tieredTeam(), nil, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body.Error.Type != "chain_exhausted" {
		t.Errorf("error.type = %q, want chain_exhausted", body.Error.Type)
	}
	if len(body.Error.Attempts) != 2 {
		t.Fatalf("attempts = %d entries, want 2: %+v", len(body.Error.Attempts), body.Error.Attempts)
	}

	// In chain order, each carrying its own failure reason rather than a
	// single flattened one.
	if got := body.Error.Attempts[0]; got.Provider != "groq" || got.Type != string(provider.KindServerError) {
		t.Errorf("attempts[0] = %+v, want groq/server_error", got)
	}
	if got := body.Error.Attempts[1]; got.Provider != "ollama" || got.Type != string(provider.KindRateLimited) {
		t.Errorf("attempts[1] = %+v, want ollama/rate_limited", got)
	}
}

// TestSingleCandidateFailureKeepsItsOwnStatus is the other half of that
// contract: with nothing to fall back to there is no breakdown to give, and
// the provider's own mapped status says more than a flat 503 would.
func TestSingleCandidateFailureKeepsItsOwnStatus(t *testing.T) {
	groq := failingMock("groq", provider.KindRateLimited, true)

	resolver := stubResolver{byModel: map[string]provider.Provider{"fast-a": groq}}
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: tieredTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil,
		noRetryConfig(t), discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)

	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a lone candidate keeps its own mapping", resp.StatusCode)
	}
}

// TestTotalAttemptsAreBoundedAcrossTheChain is CLAUDE.md's retry
// amplification warning made concrete: with a 3-attempt retry policy and a
// 4-attempt chain budget, the primary spends three and the fallback gets the
// one that is left — not three of its own.
func TestTotalAttemptsAreBoundedAcrossTheChain(t *testing.T) {
	cfg, err := resilience.NewConfig(3, time.Millisecond, 4)
	if err != nil {
		t.Fatalf("resilience.NewConfig() error: %v", err)
	}

	groq := failingMock("groq", provider.KindServerError, true)
	ollama := failingMock("ollama", provider.KindServerError, true)

	srv := newTieredServerWithRetry(t, cfg, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if groq.Attempts() != 3 {
		t.Errorf("primary attempts = %d, want 3: the primary keeps its full retry allowance", groq.Attempts())
	}
	if ollama.Attempts() != 1 {
		t.Errorf("fallback attempts = %d, want 1: only the remaining budget is left to spend", ollama.Attempts())
	}
}

// TestNoFallbackOnCallerFault covers the eligibility rule: a request the
// provider will never accept fails identically everywhere, so the walk stops
// at the first candidate and the caller gets that error immediately rather
// than the same answer several round trips later.
func TestNoFallbackOnCallerFault(t *testing.T) {
	groq := failingMock("groq", provider.KindInvalidRequest, false)
	ollama := okMock("ollama", "fast-b")

	srv := newTieredServer(t, tieredTeam(), nil, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ollama.Attempts() != 0 {
		t.Errorf("fallback attempts = %d, want 0: a malformed request fails the same way everywhere", ollama.Attempts())
	}
}

// TestFallbackOnUpstreamAuthFailure is the deliberate asymmetry between the
// retry rule and the fallback rule: a rejected credential is never retried
// against the provider that rejected it, but it is exactly the case another
// provider can answer.
func TestFallbackOnUpstreamAuthFailure(t *testing.T) {
	groq := failingMock("groq", provider.KindAuthFailed, false)
	ollama := okMock("ollama", "fast-b")

	srv := newTieredServer(t, tieredTeam(), nil, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a dead upstream key must not take the tier down", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderProvider); got != "ollama" {
		t.Errorf("%s = %q, want ollama", HeaderProvider, got)
	}
}

// --- Step 6.4: cost implications of a fallback ------------------------------

// modelCostCalculator prices per model, which is what makes a fallback's cost
// change observable at all — stubCostCalculator returns one figure for
// everything.
type modelCostCalculator struct {
	perToken map[string]int64
}

func (c modelCostCalculator) Cost(model string, inputTokens, outputTokens int) (int64, error) {
	rate, ok := c.perToken[model]
	if !ok {
		return 0, fmt.Errorf("no pricing for model %q", model)
	}
	return rate * int64(inputTokens+outputTokens), nil
}

// recordingBudgetTracker logs every Reserve call so a test can assert that
// the fallback re-check happened and for how much. denyAfter makes the Nth
// call onward refuse, which is how the "team cannot afford the fallback" case
// is exercised without a real Redis.
type recordingBudgetTracker struct {
	denyAfter int // 0 means never deny

	calls []int64
}

func (b *recordingBudgetTracker) Reserve(_ context.Context, _ string, _, estimatedMicros int64) (*budget.Reservation, budget.Result, error) {
	b.calls = append(b.calls, estimatedMicros)
	if b.denyAfter > 0 && len(b.calls) >= b.denyAfter {
		return nil, budget.Result{Allowed: false, SpentMicros: 900}, nil
	}
	// A nil *budget.Reservation is safe to hand back: its Reconcile is a no-op
	// on a nil receiver by design, and this package cannot fabricate a working
	// one since the fields are unexported.
	return nil, budget.Result{Allowed: true, SpentMicros: estimatedMicros}, nil
}

// newCostAwareServer wires the two mocks behind a per-model price list and a
// budget tracker the test can inspect.
func newCostAwareServer(t *testing.T, tracker BudgetTracker, calc CostCalculator, logTo *slog.Logger, groq, ollama provider.Provider) *httptest.Server {
	t.Helper()

	resolver := stubResolver{
		byModel: map[string]provider.Provider{"fast-a": groq, "fast-b": ollama},
		tier:    fastTier,
	}
	team := tieredTeam()
	team.MonthlyBudgetMicros = 1_000_000

	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: team}, stubRateLimiter{},
		tracker, calc, stubHealthRecorder{}, nil,
		noRetryConfig(t), logTo, func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

// TestFallbackToPricierModelBillsAtServedPrice is Step 6.4's first
// requirement: the request is billed for what actually served it, not for
// what was asked for.
func TestFallbackToPricierModelBillsAtServedPrice(t *testing.T) {
	groq := failingMock("groq", provider.KindServerError, true)
	ollama := &provider.Mock{
		ProviderName: "ollama",
		Response: &provider.Response{
			Content:      "hi",
			FinishReason: provider.FinishStop,
			Model:        "fast-b",
			Provider:     "ollama",
			Usage:        provider.Usage{InputTokens: 10, OutputTokens: 10},
		},
	}

	// fast-b costs ten times what fast-a does per token.
	calc := modelCostCalculator{perToken: map[string]int64{"fast-a": 1, "fast-b": 10}}

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	srv := newCostAwareServer(t, &recordingBudgetTracker{}, calc, log, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// 20 tokens at fast-b's rate, not fast-a's: 200 micro-dollars, not 20.
	if want := "\"cost_micros\":200"; !strings.Contains(logs.String(), want) {
		t.Errorf("request log missing %s; the fallback was billed at the requested model's price\n%s", want, logs.String())
	}
}

// TestFallbackLogsCostDelta covers Step 6.4's second requirement: every
// fallback event says what the move is estimated to cost against the model
// the caller asked for.
func TestFallbackLogsCostDelta(t *testing.T) {
	groq := failingMock("groq", provider.KindServerError, true)
	ollama := okMock("ollama", "fast-b")
	calc := modelCostCalculator{perToken: map[string]int64{"fast-a": 1, "fast-b": 10}}

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	srv := newCostAwareServer(t, &recordingBudgetTracker{}, calc, log, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "falling back to next candidate") {
		t.Fatalf("no fallback log line emitted\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "estimated_cost_delta_micros") {
		t.Errorf("fallback log line carries no cost delta\n%s", logs.String())
	}
}

// TestPricierFallbackReservesOnlyTheDifference is Step 6.4's third
// requirement. The budget check re-runs for the costlier model, and it
// reserves the difference rather than the whole estimate again — the part
// already held must not be charged twice.
func TestPricierFallbackReservesOnlyTheDifference(t *testing.T) {
	groq := failingMock("groq", provider.KindServerError, true)
	ollama := okMock("ollama", "fast-b")
	calc := modelCostCalculator{perToken: map[string]int64{"fast-a": 1, "fast-b": 10}}

	tracker := &recordingBudgetTracker{}
	srv := newCostAwareServer(t, tracker, calc, discardLogger(), groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(tracker.calls) != 2 {
		t.Fatalf("budget reserve calls = %v, want 2: one for the request, one for the pricier fallback", tracker.calls)
	}

	base, topUp := tracker.calls[0], tracker.calls[1]
	// fast-b is 10x fast-a on the same estimated token ceiling, so the top-up
	// is nine times the original hold — the difference, not the whole thing.
	if want := base * 9; topUp != want {
		t.Errorf("top-up = %d, want %d (the difference between the two estimates)", topUp, want)
	}
}

// TestCheaperFallbackTakesNoTopUp is the other side of that rule: moving down
// the tier to something cheaper needs no second Redis round trip at all,
// because the existing reservation already covers it.
func TestCheaperFallbackTakesNoTopUp(t *testing.T) {
	groq := failingMock("groq", provider.KindServerError, true)
	ollama := okMock("ollama", "fast-b")
	calc := modelCostCalculator{perToken: map[string]int64{"fast-a": 10, "fast-b": 1}}

	tracker := &recordingBudgetTracker{}
	srv := newCostAwareServer(t, tracker, calc, discardLogger(), groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(tracker.calls) != 1 {
		t.Errorf("budget reserve calls = %v, want 1: a cheaper fallback needs no re-check", tracker.calls)
	}
}

// TestUnaffordableFallbackIsDeniedNot503 covers the denial path: the team
// cannot afford the only remaining candidate, so the answer is Step 4.2's
// 402 rather than a chain-exhausted 503 that would invite a retry against a
// cap that is not going to move.
func TestUnaffordableFallbackIsDeniedNot503(t *testing.T) {
	groq := failingMock("groq", provider.KindServerError, true)
	ollama := okMock("ollama", "fast-b")
	calc := modelCostCalculator{perToken: map[string]int64{"fast-a": 1, "fast-b": 10}}

	// The first Reserve (the request itself) is allowed; the second — the
	// fallback's top-up — is refused.
	tracker := &recordingBudgetTracker{denyAfter: 2}
	srv := newCostAwareServer(t, tracker, calc, discardLogger(), groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	if ollama.Attempts() != 0 {
		t.Errorf("unaffordable candidate attempts = %d, want 0: it must be skipped before it is called", ollama.Attempts())
	}
}

// TestStreamFallsBackBeforeFirstByte is Step 6.3's streaming boundary seen
// from Step 6.2's side: a stream that fails to connect may still fall back,
// because nothing has reached the client yet.
func TestStreamFallsBackBeforeFirstByte(t *testing.T) {
	groq := &provider.Mock{
		ProviderName: "groq",
		StreamErr:    &provider.Error{Kind: provider.KindServerError, Provider: "groq", Retryable: true},
	}
	ollama := &provider.Mock{
		ProviderName: "ollama",
		StreamChunks: []*provider.Chunk{
			{Content: "hello"},
			{FinishReason: provider.FinishStop, Usage: &provider.Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}

	srv := newTieredServer(t, tieredTeam(), nil, groq, ollama)
	resp := post(t, srv, `{"model":"fast-a","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderFallback); got != "true" {
		t.Errorf("%s = %q, want true", HeaderFallback, got)
	}
	if got := resp.Header.Get(HeaderServedModel); got != "fast-b" {
		t.Errorf("%s = %q, want fast-b", HeaderServedModel, got)
	}
	if ollama.StreamAttempts() != 1 {
		t.Errorf("fallback stream attempts = %d, want 1", ollama.StreamAttempts())
	}
}
