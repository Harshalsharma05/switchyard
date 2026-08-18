package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/budget"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
)

// discardLogger keeps test output readable while still exercising every logging
// path in the handlers.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// noRetryConfig is MaxAttempts: 1 — no retries at all. It's the default for
// every test in this package that isn't specifically about Step 6.1's retry
// behavior (that lives in retry_test.go), so a Mock provider's call count
// keeps meaning exactly what it always has: one call per request.
//
// The chain-wide budget is deliberately larger than the per-provider cap:
// Step 6.3's ceiling must not be what stops a fallback test from reaching
// its second candidate, or a chain assertion would be measuring the budget
// instead of the chain.
func noRetryConfig(t *testing.T) resilience.Config {
	t.Helper()
	cfg, err := resilience.NewConfig(1, time.Millisecond, 5)
	if err != nil {
		t.Fatalf("resilience.NewConfig() error: %v", err)
	}
	return cfg
}

// stubResolver is the fake behind the consumer-defined Resolver interface. It is
// what lets these tests run without a real registry.
type stubResolver struct {
	prov provider.Provider
	err  error

	// byModel and tier are Step 6.2's additions, used only by the fallback
	// tests: byModel gives each candidate its own mock so a chain walk is
	// observable, and tier is what TierFor hands to resilience.BuildChain.
	// Left nil, this resolver behaves exactly as it did before Phase 6 —
	// one provider for every model, no tier, no fallback.
	byModel map[string]provider.Provider
	tier    []resilience.Candidate
}

func (s stubResolver) ForModel(model string) (provider.Provider, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.byModel != nil {
		p, ok := s.byModel[model]
		if !ok {
			return nil, fmt.Errorf("resolving model %q: %w", model, provider.ErrModelNotSupported)
		}
		return p, nil
	}
	return s.prov, nil
}

func (s stubResolver) TierFor(string) []resilience.Candidate {
	return s.tier
}

// DefaultMaxTokensFor is fixed rather than configurable per test: every test
// that reaches reserveTokens only cares that some positive ceiling exists,
// not its exact value — the exact value is what ratelimit's own tests cover.
func (s stubResolver) DefaultMaxTokensFor(string) (int, bool) {
	return 1024, true
}

// stubRateLimiter is the fake behind the consumer-defined RateLimiter
// interface. Its zero value allows every call and returns a nil reservation —
// which Reconcile treats as a safe no-op by design (see bucket.go) — so a
// test that is not about rate limiting can ignore this entirely, the same
// role stubAuthenticator's permissive default team plays for Auth.
type stubRateLimiter struct {
	consumeResult *ratelimit.Result
	consumeErr    error
	reserveResult *ratelimit.Result
	reserveErr    error
}

func (s stubRateLimiter) Consume(context.Context, string, ratelimit.LimitType, int, int, time.Duration) (ratelimit.Result, error) {
	if s.consumeErr != nil {
		return ratelimit.Result{}, s.consumeErr
	}
	if s.consumeResult != nil {
		return *s.consumeResult, nil
	}
	return ratelimit.Result{Allowed: true, Remaining: 999}, nil
}

func (s stubRateLimiter) Reserve(context.Context, string, ratelimit.LimitType, int, int, time.Duration) (*ratelimit.Reservation, ratelimit.Result, error) {
	if s.reserveErr != nil {
		return nil, ratelimit.Result{}, s.reserveErr
	}
	if s.reserveResult != nil {
		// nil is always safe to return here regardless of Allowed: a denied
		// reservation has nothing to reconcile, and ratelimit.Reservation's
		// fields are unexported by design, so this package cannot fabricate a
		// working one anyway. TestTPMReservationSettlesAgainstRealUsage below
		// is what verifies real settle-up behavior, against a real Limiter.
		return nil, *s.reserveResult, nil
	}
	return nil, ratelimit.Result{Allowed: true, Remaining: 999}, nil
}

// stubCostCalculator is the fake behind the consumer-defined CostCalculator
// interface. Its zero value prices every model at zero, which is enough for
// every test that is not specifically about cost accounting.
type stubCostCalculator struct {
	costMicros int64
	err        error
}

func (s stubCostCalculator) Cost(string, int, int) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.costMicros, nil
}

// stubHealthRecorder is the fake behind the consumer-defined HealthRecorder
// interface. It does nothing: no test in this file is about Step 5.2's
// passive signal, that lives in internal/health's own tests against the real
// Recorder, so this only needs to satisfy the interface without panicking.
type stubHealthRecorder struct{}

func (stubHealthRecorder) Record(string, time.Duration, error) {}

// stubBudgetTracker is the fake behind the consumer-defined BudgetTracker
// interface. Its zero value allows every reservation and reports zero spend,
// which is enough for every test that is not specifically about budget
// enforcement — the same role stubRateLimiter's permissive default plays for
// RateLimiter.
type stubBudgetTracker struct {
	result *budget.Result
	err    error
}

func (s stubBudgetTracker) Reserve(context.Context, string, int64, int64) (*budget.Reservation, budget.Result, error) {
	if s.err != nil {
		return nil, budget.Result{}, s.err
	}
	if s.result != nil {
		// nil is always safe here regardless of Allowed, mirroring
		// stubRateLimiter's Reserve: budget.Reservation's fields are
		// unexported, so this package cannot fabricate a working one, and a
		// denied reservation has nothing to reconcile anyway.
		return nil, *s.result, nil
	}
	return nil, budget.Result{Allowed: true, SpentMicros: 0}, nil
}

// stubAuthenticator is the fake behind the consumer-defined Authenticator
// interface, the same role stubResolver plays for Resolver.
type stubAuthenticator struct {
	team *auth.Team
	err  error
}

func (s stubAuthenticator) Authenticate(string) (*auth.Team, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.team, nil
}

// testAPIKey is sent on every request the post() helper makes. Its value is
// never checked by stubAuthenticator — only that some non-empty bearer token
// was presented — so tests that are not about auth can ignore auth entirely.
const testAPIKey = "sk-test-key"

// defaultTestTeam is allowed every model any pre-Phase-3 test resolves
// against, including ones no stubResolver ever actually serves ("gpt-4o",
// "nope"). Those are used to prove the team-allowlist check and the
// provider-registry check are two different failures with two different
// status codes: the team has to clear the allowlist and fail at resolve
// instead, the same way a real caller's request would.
func defaultTestTeam() *auth.Team {
	return &auth.Team{
		ID:               "test-team",
		AllowedProviders: []string{"groq", "mock"},
		AllowedModels:    []string{"openai/gpt-oss-120b", "gpt-4o", "nope", "m"},
	}
}

func newTestServer(t *testing.T, resolver Resolver) *httptest.Server {
	t.Helper()
	return newTestServerWithAuth(t, resolver, stubAuthenticator{team: defaultTestTeam()})
}

func newTestServerWithAuth(t *testing.T, resolver Resolver, authr Authenticator) *httptest.Server {
	t.Helper()
	return newTestServerFull(t, resolver, authr, stubRateLimiter{})
}

func newTestServerFull(t *testing.T, resolver Resolver, authr Authenticator, limiter RateLimiter) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(NewRouter(resolver, authr, limiter, stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, nil, nil, noRetryConfig(t), discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	return postWithAuth(t, srv, body, testAPIKey)
}

func postWithAuth(t *testing.T, srv *httptest.Server, body, bearerKey string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerKey != "" {
		req.Header.Set("Authorization", "Bearer "+bearerKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestChatCompletionsSuccess(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Models:       []string{"openai/gpt-oss-120b"},
		Response: &provider.Response{
			Content:      "hello back",
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 12, OutputTokens: 4},
			Model:        "openai/gpt-oss-120b",
			Provider:     "groq",
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})
	resp := post(t, srv, `{"model":"openai/gpt-oss-120b","messages":[{"role":"user","content":"hello"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if got.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got.Object)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "hello back" {
		t.Fatalf("choices = %+v", got.Choices)
	}
	if got.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", got.Choices[0].Message.Role)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
	// Total is derived, not trusted from the provider.
	if got.Usage.TotalTokens != 16 {
		t.Errorf("total_tokens = %d, want 16", got.Usage.TotalTokens)
	}
	if !strings.HasPrefix(got.ID, "chatcmpl-") {
		t.Errorf("id = %q, want an OpenAI-shaped id", got.ID)
	}
	if resp.Header.Get(HeaderRequestID) == "" {
		t.Error("response carried no request ID header")
	}

	// The system-prompt-stays-a-message contract: the proxy must not hoist.
	reqs := mock.Requests()
	if len(reqs) != 1 || reqs[0].Model != "openai/gpt-oss-120b" {
		t.Fatalf("provider received %+v", reqs)
	}
}

func TestChatCompletionsUnknownModelIs404(t *testing.T) {
	srv := newTestServer(t, stubResolver{
		err: fmt.Errorf("resolving model %q: %w", "gpt-4o", provider.ErrModelNotSupported),
	})

	resp := post(t, srv, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if !strings.Contains(body.Error.Message, "gpt-4o") {
		t.Errorf("message = %q, want it to name the model", body.Error.Message)
	}
}

func TestAuthMissingHeaderIs401(t *testing.T) {
	srv := newTestServerWithAuth(t, stubResolver{prov: &provider.Mock{}}, stubAuthenticator{team: defaultTestTeam()})

	resp := postWithAuth(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthMalformedHeaderIs401(t *testing.T) {
	srv := newTestServerWithAuth(t, stubResolver{prov: &provider.Mock{}}, stubAuthenticator{team: defaultTestTeam()})

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz") // well-formed, but not a bearer token
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthUnknownKeyIs401(t *testing.T) {
	srv := newTestServerWithAuth(t, stubResolver{prov: &provider.Mock{}}, stubAuthenticator{err: auth.ErrUnknownKey})

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "invalid_api_key" {
		t.Errorf("error.type = %q, want invalid_api_key", body.Error.Type)
	}
}

// Proves Auth's context value actually reaches the handler, not just that a
// bad credential gets rejected: authorizeModel's real branch, not its
// nil-team wiring-bug branch, is what has to run for this to succeed.
func TestAuthTeamReachesHandler(t *testing.T) {
	restrictedTeam := &auth.Team{ID: "restricted", AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"only-this-model"}}
	srv := newTestServerWithAuth(t, stubResolver{prov: &provider.Mock{}}, stubAuthenticator{team: restrictedTeam})

	resp := post(t, srv, `{"model":"only-this-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the team's one allowed model", resp.StatusCode)
	}
}

// The allowlist check must reject before resolve ever runs — a resolver that
// errors if called is what proves that, rather than merely asserting the
// final status code.
func TestChatCompletionsModelNotAllowedIs403(t *testing.T) {
	restrictedTeam := &auth.Team{ID: "restricted", AllowedProviders: []string{"groq", "mock"}, AllowedModels: []string{"only-this-model"}}
	mustNotResolve := stubResolver{err: fmt.Errorf("resolve must not run once the allowlist check has rejected the request")}
	srv := newTestServerWithAuth(t, mustNotResolve, stubAuthenticator{team: restrictedTeam})

	resp := post(t, srv, `{"model":"some-other-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if !strings.Contains(body.Error.Message, "only-this-model") {
		t.Errorf("message = %q, want it to name the allowed model", body.Error.Message)
	}
}

// The mapping that matters most: an upstream credential failure must not be
// reported to the caller as a problem with *their* credential.
func TestUpstreamAuthFailureIsBadGatewayNotUnauthorized(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Err: &provider.Error{
			Kind:       provider.KindAuthFailed,
			Provider:   "groq",
			StatusCode: 401,
			Message:    "invalid api key",
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})
	resp := post(t, srv, `{"model":"openai/gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("returned 401: that tells the caller their own key is bad, which it is not")
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProviderErrorStatusMapping(t *testing.T) {
	tests := map[string]struct {
		kind       provider.Kind
		retryAfter string
		want       int
	}{
		"rate limited passes through": {provider.KindRateLimited, "", http.StatusTooManyRequests},
		"timeout is gateway timeout":  {provider.KindTimeout, "", http.StatusGatewayTimeout},
		"invalid request is 400":      {provider.KindInvalidRequest, "", http.StatusBadRequest},
		"content policy is 400":       {provider.KindContentPolicy, "", http.StatusBadRequest},
		"server error is 502":         {provider.KindServerError, "", http.StatusBadGateway},
		"network error is 502":        {provider.KindNetworkError, "", http.StatusBadGateway},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			mock := &provider.Mock{
				Err: &provider.Error{Kind: tc.kind, Provider: "groq", Message: "upstream said no"},
			}
			srv := newTestServer(t, stubResolver{prov: mock})

			resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}

			var body errorBody
			json.NewDecoder(resp.Body).Decode(&body)
			if body.Error.Type != string(tc.kind) {
				t.Errorf("error.type = %q, want %q", body.Error.Type, tc.kind)
			}
		})
	}
}

// A provider that told us when to come back must have that instruction reach
// the client, rather than being replaced by SwitchYard's own guess.
func TestRetryAfterIsForwarded(t *testing.T) {
	mock := &provider.Mock{
		Err: &provider.Error{
			Kind:       provider.KindRateLimited,
			Provider:   "groq",
			Message:    "slow down",
			RetryAfter: 7_000_000_000, // 7s
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})
	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	if got := resp.Header.Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want 7", got)
	}
}

func TestChatCompletionsRejects(t *testing.T) {
	tests := map[string]struct {
		body     string
		want     int
		wantText string
	}{
		"not json": {
			body: `{nope`,
			want: http.StatusBadRequest,
		},
		"no model": {
			body:     `{"messages":[{"role":"user","content":"hi"}]}`,
			want:     http.StatusBadRequest,
			wantText: "model is required",
		},
		"no messages": {
			body:     `{"model":"m","messages":[]}`,
			want:     http.StatusBadRequest,
			wantText: "at least one message",
		},
		"unknown role": {
			body:     `{"model":"m","messages":[{"role":"wizard","content":"hi"}]}`,
			want:     http.StatusBadRequest,
			wantText: "unknown role",
		},
		"negative max tokens": {
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":-5}`,
			want:     http.StatusBadRequest,
			wantText: "cannot be negative",
		},
		"temperature out of range": {
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":9}`,
			want:     http.StatusBadRequest,
			wantText: "outside the supported range",
		},
		"misspelled field": {
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_token":50}`,
			want:     http.StatusBadRequest,
			wantText: "max_token",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t, stubResolver{prov: &provider.Mock{}})

			resp := post(t, srv, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}

			if tc.wantText != "" {
				var body errorBody
				json.NewDecoder(resp.Body).Decode(&body)
				if !strings.Contains(body.Error.Message, tc.wantText) {
					t.Errorf("message = %q, want it to mention %q", body.Error.Message, tc.wantText)
				}
			}
		})
	}
}

// parseSSE splits a raw SSE body into its "data:" payloads, stripping the
// "data: " prefix and the blank-line event framing. Every streaming test
// below reads through this rather than re-deriving the SSE grammar each time.
func parseSSE(t *testing.T, body []byte) []string {
	t.Helper()

	var out []string
	for _, block := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data: ") {
			t.Fatalf("SSE block missing the data: prefix: %q", block)
		}
		out = append(out, strings.TrimPrefix(block, "data: "))
	}
	return out
}

func TestStreamChatCompletionsSuccess(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Models:       []string{"m"},
		StreamChunks: []*provider.Chunk{
			{Content: "hi"},
			{Content: " there", FinishReason: provider.FinishStop, Usage: &provider.Usage{InputTokens: 5, OutputTokens: 2}},
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})
	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if conn := resp.Header.Get("Connection"); conn != "keep-alive" {
		t.Errorf("Connection = %q, want keep-alive", conn)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	events := parseSSE(t, body)
	if len(events) != 3 {
		t.Fatalf("got %d SSE events, want 3 (two chunks + [DONE]): %q", len(events), body)
	}

	var first chatChunkResponse
	if err := json.Unmarshal([]byte(events[0]), &first); err != nil {
		t.Fatalf("decoding first chunk: %v", err)
	}
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk role = %q, want assistant", first.Choices[0].Delta.Role)
	}
	if first.Choices[0].Delta.Content != "hi" {
		t.Errorf("first chunk content = %q, want %q", first.Choices[0].Delta.Content, "hi")
	}
	if first.Choices[0].FinishReason != nil {
		t.Errorf("first chunk finish_reason = %v, want null", first.Choices[0].FinishReason)
	}

	var second chatChunkResponse
	if err := json.Unmarshal([]byte(events[1]), &second); err != nil {
		t.Fatalf("decoding second chunk: %v", err)
	}
	// Role appears once, on the first chunk only — mirroring OpenAI's dialect.
	if second.Choices[0].Delta.Role != "" {
		t.Errorf("second chunk role = %q, want omitted", second.Choices[0].Delta.Role)
	}
	if second.Choices[0].Delta.Content != " there" {
		t.Errorf("second chunk content = %q, want %q", second.Choices[0].Delta.Content, " there")
	}
	if second.Choices[0].FinishReason == nil || *second.Choices[0].FinishReason != "stop" {
		t.Errorf("second chunk finish_reason = %v, want stop", second.Choices[0].FinishReason)
	}

	if events[2] != "[DONE]" {
		t.Errorf("final event = %q, want [DONE]", events[2])
	}

	if mock.StreamAttempts() != 1 {
		t.Errorf("Stream called %d times, want 1", mock.StreamAttempts())
	}
	if mock.Attempts() != 0 {
		t.Errorf("Complete called %d times, want 0: a streaming request must never reach the non-streaming path", mock.Attempts())
	}
}

// A failure before any chunk is functionally identical to Stream() itself
// failing: nothing has reached the client, so it gets a normal status-coded
// error response rather than an SSE frame.
func TestStreamChatCompletionsErrorBeforeFirstByte(t *testing.T) {
	mock := &provider.Mock{
		StreamErr: &provider.Error{Kind: provider.KindAuthFailed, Provider: "groq", Message: "invalid api key"},
	}

	srv := newTestServer(t, stubResolver{prov: mock})
	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json: a pre-stream failure is a normal error response", ct)
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != string(provider.KindAuthFailed) {
		t.Errorf("error.type = %q, want %q", body.Error.Type, provider.KindAuthFailed)
	}
}

// failAfterOneChunkReader simulates a provider that streams one good chunk
// and then breaks — the case Step 2.4 calls out specifically, because the
// status line and at least one event are already on the wire by then.
type failAfterOneChunkReader struct {
	sent bool
}

func (r *failAfterOneChunkReader) Recv() (*provider.Chunk, error) {
	if !r.sent {
		r.sent = true
		return &provider.Chunk{Content: "partial"}, nil
	}
	return nil, &provider.Error{Kind: provider.KindServerError, Provider: "groq", Message: "connection reset", Retryable: true}
}

func (r *failAfterOneChunkReader) Close() error { return nil }

func TestStreamChatCompletionsMidStreamError(t *testing.T) {
	mock := &provider.Mock{
		StreamFunc: func(context.Context, provider.Request) (provider.StreamReader, error) {
			return &failAfterOneChunkReader{}, nil
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})
	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	// The status line was already committed to 200 by the first chunk; a
	// failure after that cannot change it.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: headers were already sent before the failure", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	events := parseSSE(t, body)
	if len(events) != 2 {
		t.Fatalf("got %d SSE events, want 2 (one chunk + one error event): %q", len(events), body)
	}

	var errEvent errorBody
	if err := json.Unmarshal([]byte(events[1]), &errEvent); err != nil {
		t.Fatalf("decoding error event: %v", err)
	}
	if errEvent.Error.Message == "" {
		t.Error("error event carried no message")
	}
	// No [DONE] after a failure: partial content already reached the client,
	// so this is not a clean completion and must not be reported as one.
	if events[1] == "[DONE]" {
		t.Error("stream ended with [DONE] instead of an error event")
	}
}

// blockingStreamReader never returns a chunk on its own; it waits for its
// context to be cancelled, which is exactly what a client disconnect should
// cause per Step 2.3.
type blockingStreamReader struct {
	ctx       context.Context
	cancelled chan struct{}
}

func (r *blockingStreamReader) Recv() (*provider.Chunk, error) {
	<-r.ctx.Done()
	close(r.cancelled)
	return nil, r.ctx.Err()
}

func (r *blockingStreamReader) Close() error { return nil }

func TestStreamClientDisconnectCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})

	mock := &provider.Mock{
		StreamFunc: func(ctx context.Context, req provider.Request) (provider.StreamReader, error) {
			close(started)
			return &blockingStreamReader{ctx: ctx, cancelled: cancelled}, nil
		},
	}

	srv := newTestServer(t, stubResolver{prov: mock})

	ctx, cancel := context.WithCancel(context.Background())
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+testAPIKey)

	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never reached Stream")
	}

	// This is standing in for the client going away: cancelling the request's
	// own context is what closes the underlying connection, which is what
	// net/http then reports through the server-side request's Context.
	cancel()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream Recv was never cancelled after the client disconnected")
	}

	<-done
}

func TestOversizedBodyIsRejected(t *testing.T) {
	srv := newTestServer(t, stubResolver{prov: &provider.Mock{}})

	huge := strings.Repeat("a", maxRequestBytes+1024)
	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"`+huge+`"}]}`)

	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want the body to be rejected", resp.StatusCode)
	}
}

func TestRoutingEdges(t *testing.T) {
	srv := newTestServer(t, stubResolver{prov: &provider.Mock{}})

	t.Run("unknown path is 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/v1/nope")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("wrong method is 405", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})

	// Phase 4's own checklist: "Admin port is not reachable from the public
	// port's network interface." Binding the two listeners to separate
	// addresses (main.go) is what makes that a network fact rather than a
	// routing convention, but the routing side of it — proxy.NewRouter never
	// mounting any /admin path at all — deserves its own direct proof rather
	// than trusting that main.go's two net.Listen calls are the whole story.
	for _, path := range []string{"/admin/teams", "/admin/teams/acme", "/admin/providers", "/admin/reload"} {
		t.Run("admin path "+path+" is not mounted on the public router", func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404: the public router must not serve any /admin path", resp.StatusCode)
			}
		})
	}
}

func TestProbes(t *testing.T) {
	t.Run("healthz is always ok", func(t *testing.T) {
		srv := httptest.NewServer(NewRouter(stubResolver{}, stubAuthenticator{}, stubRateLimiter{}, stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, nil, nil, noRetryConfig(t), discardLogger(), func() bool { return false }))
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()

		// Liveness must not depend on readiness: a process that can serve HTTP
		// is alive, and restarting it would not fix an unready dependency.
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 even when not ready", resp.StatusCode)
		}
	})

	t.Run("readyz reflects readiness", func(t *testing.T) {
		tests := map[bool]int{true: http.StatusOK, false: http.StatusServiceUnavailable}

		for ready, want := range tests {
			srv := httptest.NewServer(NewRouter(stubResolver{}, stubAuthenticator{}, stubRateLimiter{}, stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, nil, nil, noRetryConfig(t), discardLogger(), func() bool { return ready }))

			resp, err := http.Get(srv.URL + "/readyz")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			resp.Body.Close()
			srv.Close()

			if resp.StatusCode != want {
				t.Errorf("ready=%v gave status %d, want %d", ready, resp.StatusCode, want)
			}
		}
	})
}
