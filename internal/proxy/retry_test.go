package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
)

// retryTestConfig allows real retries — a small MaxAttempts and a tiny
// BaseDelay so these tests stay fast while still exercising resilience.Do's
// actual backoff sleep, not just its attempt-counting.
func retryTestConfig(t *testing.T) resilience.Config {
	t.Helper()
	cfg, err := resilience.NewConfig(3, time.Millisecond, 3)
	if err != nil {
		t.Fatalf("resilience.NewConfig() error: %v", err)
	}
	return cfg
}

func newTestServerWithRetry(t *testing.T, resolver Resolver, retryConfig resilience.Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(resolver, stubAuthenticator{team: defaultTestTeam()}, stubRateLimiter{},
		stubBudgetTracker{}, stubCostCalculator{}, stubHealthRecorder{}, nil, retryConfig, discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

// TestChatCompletionsRetriesThenSucceeds is Step 6.1's core case end to end:
// a retryable failure (KindServerError) followed by a success is retried
// against the same provider and the caller sees the eventual success, not
// the earlier failures.
func TestChatCompletionsRetriesThenSucceeds(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		CompleteFunc: func(ctx context.Context, req provider.Request, attempt int) (*provider.Response, error) {
			if attempt < 3 {
				return nil, &provider.Error{Kind: provider.KindServerError, Provider: "groq", Retryable: true}
			}
			return &provider.Response{
				Content: "ok", FinishReason: provider.FinishStop, Model: req.Model, Provider: "groq",
				Usage: provider.Usage{InputTokens: 1, OutputTokens: 1},
			}, nil
		},
	}
	srv := newTestServerWithRetry(t, stubResolver{prov: mock}, retryTestConfig(t))

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if mock.Attempts() != 3 {
		t.Errorf("Attempts() = %d, want 3 (two retries then success)", mock.Attempts())
	}
}

// TestChatCompletionsNeverRetriesAuthFailure is Step 6.1's "auth failure is
// never retried" checklist item, verified against the real handler and a
// real HTTP round trip, not just resilience.Do in isolation.
func TestChatCompletionsNeverRetriesAuthFailure(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Err:          &provider.Error{Kind: provider.KindAuthFailed, Provider: "groq", Retryable: false},
	}
	srv := newTestServerWithRetry(t, stubResolver{prov: mock}, retryTestConfig(t))

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if mock.Attempts() != 1 {
		t.Errorf("Attempts() = %d, want exactly 1 — an auth failure must never be retried", mock.Attempts())
	}
}

// TestChatCompletionsCapsRetriesAtMaxAttempts proves a provider that never
// recovers exhausts the configured attempt cap rather than retrying forever,
// and that the caller still gets a real provider-shaped error.
func TestChatCompletionsCapsRetriesAtMaxAttempts(t *testing.T) {
	mock := &provider.Mock{
		ProviderName: "groq",
		Err:          &provider.Error{Kind: provider.KindServerError, Provider: "groq", Retryable: true},
	}
	srv := newTestServerWithRetry(t, stubResolver{prov: mock}, retryTestConfig(t))

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if mock.Attempts() != 3 {
		t.Errorf("Attempts() = %d, want capped at 3", mock.Attempts())
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != string(provider.KindServerError) {
		t.Errorf("error.type = %q, want %q", body.Error.Type, provider.KindServerError)
	}
}

// TestStreamChatCompletionsRetriesInitialConnectionOnly proves Step 6.1's
// streaming boundary: a failure connecting (before any byte reaches the
// client) is retried, but once a chunk has actually reached the client, a
// later Recv() failure is not — retrying would duplicate content the client
// already received (Step 2.4/6.3). The connection itself is retried twice
// before succeeding; once connected, the stream sends one real chunk and
// then breaks, which must end the request with an SSE error event and no
// further attempt at prov.Stream.
func TestStreamChatCompletionsRetriesInitialConnectionOnly(t *testing.T) {
	streamAttempts := 0
	mock := &provider.Mock{
		ProviderName: "groq",
		StreamFunc: func(ctx context.Context, req provider.Request) (provider.StreamReader, error) {
			streamAttempts++
			if streamAttempts < 3 {
				return nil, &provider.Error{Kind: provider.KindServerError, Provider: "groq", Retryable: true}
			}
			return &flakyStreamReader{}, nil
		},
	}
	srv := newTestServerWithRetry(t, stubResolver{prov: mock}, retryTestConfig(t))

	resp := post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()

	// The one real chunk flakyStreamReader sends before failing already
	// committed the status line as 200 — a mid-stream failure cannot change
	// it (Step 2.4), unlike a failure during the connection attempts above.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a chunk already reached the client before the mid-stream failure)", resp.StatusCode)
	}
	if streamAttempts != 3 {
		t.Errorf("StreamFunc called %d times, want 3 (two retried connection failures, then a successful connect) — a 4th call would mean the mid-stream Recv failure was wrongly retried", streamAttempts)
	}
}

// flakyStreamReader connects successfully, sends exactly one real chunk, and
// then fails — simulating a provider that accepted the connection, started
// answering, and broke partway through.
type flakyStreamReader struct {
	calls int
}

func (f *flakyStreamReader) Recv() (*provider.Chunk, error) {
	f.calls++
	if f.calls == 1 {
		return &provider.Chunk{Content: "hello"}, nil
	}
	return nil, &provider.Error{Kind: provider.KindNetworkError, Provider: "groq", Retryable: true}
}

func (f *flakyStreamReader) Close() error { return nil }
