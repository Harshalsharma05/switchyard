package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testConfig returns a valid config pointed at a test server.
func testConfig(baseURL string) Config {
	return Config{
		Name:             "groq",
		BaseURL:          baseURL,
		APIKey:           "test-key",
		Timeout:          2 * time.Second,
		Models:           []string{"openai/gpt-oss-120b", "openai/gpt-oss-20b"},
		DefaultMaxTokens: 1024,
	}
}

// newTestProvider wires an adapter to an httptest server running handler.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *OpenAICompatible {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := NewOpenAICompatible(testConfig(srv.URL))
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}
	return p
}

func TestCompleteSuccess(t *testing.T) {
	var gotBody oaiChatRequest
	var gotAuth string

	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("upstream received unparseable body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"model": "openai/gpt-oss-120b",
			"choices": [{"message": {"role": "assistant", "content": "hi there"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 11, "completion_tokens": 3}
		}`)
	})

	temp := float32(0)
	resp, err := p.Complete(context.Background(), Request{
		Model:       "openai/gpt-oss-120b",
		Messages:    []Message{{Role: RoleSystem, Content: "be terse"}, {Role: RoleUser, Content: "hello"}},
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want %q", resp.Content, "hi there")
	}
	if resp.FinishReason != FinishStop {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, FinishStop)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want {11 3}", resp.Usage)
	}
	if resp.Provider != "groq" {
		t.Errorf("Provider = %q, want the instance name %q", resp.Provider, "groq")
	}
	if resp.Latency <= 0 {
		t.Error("Latency was not measured")
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-key")
	}

	// A system message must stay in the array for this dialect, not get hoisted.
	if len(gotBody.Messages) != 2 || gotBody.Messages[0].Role != "system" {
		t.Errorf("messages = %+v, want the system message inline and first", gotBody.Messages)
	}
	// Temperature 0 must survive the trip: it is meaningful, which is why the
	// canonical field is a pointer.
	if gotBody.Temperature == nil || *gotBody.Temperature != 0 {
		t.Errorf("Temperature = %v, want an explicit 0", gotBody.Temperature)
	}
}

func TestCompleteSubstitutesDefaultMaxTokens(t *testing.T) {
	var gotBody oaiChatRequest

	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	})

	if _, err := p.Complete(context.Background(), Request{
		Model:    "openai/gpt-oss-120b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotBody.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want the configured default 1024", gotBody.MaxTokens)
	}
}

// The load-bearing case: two 429s that must be classified differently, which is
// only possible by reading the body.
func TestClassifyRefinesRetryableFromBody(t *testing.T) {
	tests := map[string]struct {
		status        int
		body          string
		retryAfter    string
		wantKind      Kind
		wantRetryable bool
		wantRetryWait time.Duration
	}{
		"throttled 429 is retryable": {
			status:        429,
			body:          `{"error":{"message":"rate limit reached","type":"requests"}}`,
			retryAfter:    "7",
			wantKind:      KindRateLimited,
			wantRetryable: true,
			wantRetryWait: 7 * time.Second,
		},
		"out-of-credit 429 is not retryable": {
			status:        429,
			body:          `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`,
			wantKind:      KindRateLimited,
			wantRetryable: false,
		},
		"bad key is not retryable": {
			status:        401,
			body:          `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`,
			wantKind:      KindAuthFailed,
			wantRetryable: false,
		},
		"content filter is its own kind": {
			status:        400,
			body:          `{"error":{"message":"refused","code":"content_filter"}}`,
			wantKind:      KindContentPolicy,
			wantRetryable: false,
		},
		"malformed request is not retryable": {
			status:        400,
			body:          `{"error":{"message":"messages: field required","type":"invalid_request_error"}}`,
			wantKind:      KindInvalidRequest,
			wantRetryable: false,
		},
		"server error is retryable": {
			status:        503,
			body:          `{"error":{"message":"overloaded"}}`,
			wantKind:      KindServerError,
			wantRetryable: true,
		},
		"non-json error body still classifies": {
			status:        502,
			body:          `<html>502 Bad Gateway</html>`,
			wantKind:      KindServerError,
			wantRetryable: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})

			_, err := p.Complete(context.Background(), Request{
				Model:    "openai/gpt-oss-120b",
				Messages: []Message{{Role: RoleUser, Content: "hello"}},
			})

			var provErr *Error
			if !errors.As(err, &provErr) {
				t.Fatalf("err = %v, want a *provider.Error", err)
			}
			if provErr.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", provErr.Kind, tc.wantKind)
			}
			if provErr.Retryable != tc.wantRetryable {
				t.Errorf("Retryable = %v, want %v", provErr.Retryable, tc.wantRetryable)
			}
			if provErr.RetryAfter != tc.wantRetryWait {
				t.Errorf("RetryAfter = %v, want %v", provErr.RetryAfter, tc.wantRetryWait)
			}
			if provErr.Provider != "groq" {
				t.Errorf("Provider = %q, want %q", provErr.Provider, "groq")
			}
		})
	}
}

func TestCompleteTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	cfg := testConfig(srv.URL)
	cfg.Timeout = 20 * time.Millisecond
	p, err := NewOpenAICompatible(cfg)
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}

	_, err = p.Complete(context.Background(), Request{
		Model:    "openai/gpt-oss-120b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})

	var provErr *Error
	if !errors.As(err, &provErr) {
		t.Fatalf("err = %v, want a *provider.Error", err)
	}
	if provErr.Kind != KindTimeout {
		t.Errorf("Kind = %q, want %q", provErr.Kind, KindTimeout)
	}
	if !provErr.Retryable {
		t.Error("Retryable = false, want true: a timeout may well succeed on retry")
	}
}

// A caller deadline shorter than the instance timeout must win.
func TestCompleteHonoursCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	p, err := NewOpenAICompatible(testConfig(srv.URL)) // 2s instance timeout
	if err != nil {
		t.Fatalf("NewOpenAICompatible: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := p.Complete(ctx, Request{
		Model:    "openai/gpt-oss-120b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}); err == nil {
		t.Fatal("Complete succeeded, want a deadline error")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want the 20ms caller deadline to win over the 2s instance timeout", elapsed)
	}
}

func TestCompleteRejectsMalformedUpstreamBody(t *testing.T) {
	tests := map[string]string{
		"not json":   `<html>hello</html>`,
		"no choices": `{"model":"x","choices":[]}`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			})

			_, err := p.Complete(context.Background(), Request{
				Model:    "openai/gpt-oss-120b",
				Messages: []Message{{Role: RoleUser, Content: "hello"}},
			})

			var provErr *Error
			if !errors.As(err, &provErr) {
				t.Fatalf("err = %v, want a *provider.Error", err)
			}
			if provErr.Kind != KindServerError || !provErr.Retryable {
				t.Errorf("got %q retryable=%v, want server_error retryable=true", provErr.Kind, provErr.Retryable)
			}
		})
	}
}

func TestSupportsModel(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {})

	if !p.SupportsModel("openai/gpt-oss-20b") {
		t.Error("configured model reported as unsupported")
	}
	if p.SupportsModel("gpt-4o") {
		t.Error("unconfigured model reported as supported")
	}
}

func TestNewOpenAICompatibleValidation(t *testing.T) {
	tests := map[string]func(*Config){
		"missing name":       func(c *Config) { c.Name = "" },
		"missing base url":   func(c *Config) { c.BaseURL = "" },
		"zero timeout":       func(c *Config) { c.Timeout = 0 },
		"no models":          func(c *Config) { c.Models = nil },
		"zero max tokens":    func(c *Config) { c.DefaultMaxTokens = 0 },
		"negative max token": func(c *Config) { c.DefaultMaxTokens = -1 },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig("http://localhost")
			mutate(&cfg)

			if _, err := NewOpenAICompatible(cfg); err == nil {
				t.Error("construction succeeded, want a validation error so startup fails fast")
			}
		})
	}
}

func TestStreamDecodesChunksAndTrailingUsage(t *testing.T) {
	var gotBody oaiChatRequest

	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("upstream received unparseable body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, line := range []string{
			`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{"content":" there"},"finish_reason":"stop"}]}`,
			``,
			`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2}}`,
			``,
			`data: [DONE]`,
			``,
		} {
			io.WriteString(w, line+"\n")
			flusher.Flush()
		}
	})

	stream, err := p.Stream(context.Background(), Request{
		Model:    "openai/gpt-oss-120b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, stream)

	if !gotBody.Stream {
		t.Error("stream field not sent as true")
	}
	if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage not requested, so a real upstream would never send the usage chunk")
	}

	var content string
	var finish FinishReason
	var usage *Usage
	for _, c := range chunks {
		content += c.Content
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}

	if content != "hi there" {
		t.Errorf("accumulated content = %q, want %q", content, "hi there")
	}
	if finish != FinishStop {
		t.Errorf("FinishReason = %q, want %q", finish, FinishStop)
	}
	if usage == nil || usage.InputTokens != 11 || usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v, want {11 2}", usage)
	}
}

// A stream request that never gets past the status line must classify the
// same way a non-streaming failure does, per Step 2.4: error before the first
// byte is a normal error, not a broken StreamReader.
func TestStreamErrorBeforeFirstByte(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`)
	})

	stream, err := p.Stream(context.Background(), Request{
		Model:    "openai/gpt-oss-120b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if stream != nil {
		t.Error("got a non-nil StreamReader alongside an error")
	}

	var provErr *Error
	if !errors.As(err, &provErr) {
		t.Fatalf("err = %v, want a *provider.Error", err)
	}
	if provErr.Kind != KindAuthFailed || provErr.Retryable {
		t.Errorf("got kind=%q retryable=%v, want auth_failed retryable=false", provErr.Kind, provErr.Retryable)
	}
}
