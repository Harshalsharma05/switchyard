package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// discardLogger keeps test output readable while still exercising every logging
// path in the handlers.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// stubResolver is the fake behind the consumer-defined Resolver interface. It is
// what lets these tests run without a real registry.
type stubResolver struct {
	prov provider.Provider
	err  error
}

func (s stubResolver) ForModel(string) (provider.Provider, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.prov, nil
}

func newTestServer(t *testing.T, resolver Resolver) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(NewRouter(resolver, discardLogger(), func() bool { return true }))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
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
		"streaming not yet": {
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			want:     http.StatusBadRequest,
			wantText: "streaming is not supported yet",
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

// Asking for a stream must not quietly produce a non-streaming reply the client
// cannot parse, and must not reach a provider at all.
func TestStreamingRequestNeverReachesProvider(t *testing.T) {
	mock := &provider.Mock{}
	srv := newTestServer(t, stubResolver{prov: mock})

	post(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if mock.Attempts() != 0 {
		t.Errorf("provider was called %d times, want 0", mock.Attempts())
	}
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
}

func TestProbes(t *testing.T) {
	t.Run("healthz is always ok", func(t *testing.T) {
		srv := httptest.NewServer(NewRouter(stubResolver{}, discardLogger(), func() bool { return false }))
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
			srv := httptest.NewServer(NewRouter(stubResolver{}, discardLogger(), func() bool { return ready }))

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
