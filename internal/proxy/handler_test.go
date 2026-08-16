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
