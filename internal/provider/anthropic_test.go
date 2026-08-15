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

func anthropicConfig(baseURL string) Config {
	return Config{
		Name:             "anthropic",
		BaseURL:          baseURL,
		APIKey:           "sk-ant-test",
		Timeout:          2 * time.Second,
		Models:           []string{"claude-sonnet-4-5", "claude-haiku-4-5"},
		DefaultMaxTokens: 1024,
	}
}

func newTestAnthropic(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := NewAnthropic(anthropicConfig(srv.URL))
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	return p
}

// The marquee test for this adapter: system messages must leave the array and
// become a top-level field, with the rest of the conversation intact.
func TestAnthropicHoistsSystemPrompt(t *testing.T) {
	var got anthropicRequest
	var gotHeaders http.Header

	p := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unparseable request body: %v", err)
		}
		io.WriteString(w, `{"model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":2}}`)
	})

	_, err := p.Complete(context.Background(), Request{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: RoleSystem, Content: "be terse"},
			{Role: RoleUser, Content: "hello"},
			{Role: RoleAssistant, Content: "hi"},
			{Role: RoleUser, Content: "again"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got.System != "be terse" {
		t.Errorf("system = %q, want it hoisted to the top level", got.System)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 with the system message removed", len(got.Messages))
	}
	for _, m := range got.Messages {
		if m.Role == string(RoleSystem) {
			t.Error("a system message survived in the array")
		}
	}
	if got.Messages[0].Content != "hello" || got.Messages[2].Content != "again" {
		t.Errorf("conversation order was disturbed: %+v", got.Messages)
	}

	if gotHeaders.Get("anthropic-version") != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotHeaders.Get("anthropic-version"), anthropicVersion)
	}
	if gotHeaders.Get("x-api-key") != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want the configured key", gotHeaders.Get("x-api-key"))
	}
	if gotHeaders.Get("Authorization") != "" {
		t.Error("sent a bearer token; Anthropic authenticates with x-api-key only")
	}
}

// The documented edge case: more than one system message, and one out of first
// position.
func TestAnthropicConcatenatesMultipleSystemMessages(t *testing.T) {
	var got anthropicRequest

	p := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	})

	if _, err := p.Complete(context.Background(), Request{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: RoleSystem, Content: "first"},
			{Role: RoleUser, Content: "hello"},
			{Role: RoleSystem, Content: "second"},
		},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got.System != "first\n\nsecond" {
		t.Errorf("system = %q, want both parts joined in order", got.System)
	}
	if len(got.Messages) != 1 {
		t.Errorf("messages = %d, want only the user turn", len(got.Messages))
	}
}

// max_tokens is mandatory for this API, so it must be present even when the
// caller omitted it — the reason Config.DefaultMaxTokens exists.
func TestAnthropicAlwaysSendsMaxTokens(t *testing.T) {
	var raw map[string]any

	p := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &raw)
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	})

	if _, err := p.Complete(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, present := raw["max_tokens"]; !present {
		t.Fatal("max_tokens absent; Anthropic rejects the request without it")
	}
	if raw["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, want the configured default 1024", raw["max_tokens"])
	}
}

func TestAnthropicTranslatesResponse(t *testing.T) {
	p := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		// Multiple content blocks, one of which is not text and must be skipped.
		io.WriteString(w, `{
			"model": "claude-sonnet-4-5",
			"content": [
				{"type": "text", "text": "part one "},
				{"type": "thinking", "text": "SHOULD NOT APPEAR"},
				{"type": "text", "text": "part two"}
			],
			"stop_reason": "max_tokens",
			"usage": {"input_tokens": 40, "output_tokens": 7}
		}`)
	})

	resp, err := p.Complete(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp.Content != "part one part two" {
		t.Errorf("Content = %q, want only the text blocks concatenated", resp.Content)
	}
	if resp.FinishReason != FinishLength {
		t.Errorf("FinishReason = %q, want %q for stop_reason max_tokens", resp.FinishReason, FinishLength)
	}
	if resp.Usage.InputTokens != 40 || resp.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want {40 7}", resp.Usage)
	}
	if resp.Provider != "anthropic" {
		t.Errorf("Provider = %q, want the instance name", resp.Provider)
	}
}

func TestAnthropicFinishReasons(t *testing.T) {
	tests := map[string]FinishReason{
		"end_turn":      FinishStop,
		"stop_sequence": FinishStop,
		"max_tokens":    FinishLength,
		"refusal":       FinishContentFilter,
		"something_new": FinishOther,
	}

	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := anthropicFinishReason(in); got != want {
				t.Errorf("anthropicFinishReason(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestAnthropicClassify(t *testing.T) {
	tests := map[string]struct {
		status        int
		body          string
		wantKind      Kind
		wantRetryable bool
	}{
		"overloaded 529 retries": {
			status:        529,
			body:          `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantKind:      KindServerError,
			wantRetryable: true,
		},
		"bad key does not retry": {
			status:        401,
			body:          `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			wantKind:      KindAuthFailed,
			wantRetryable: false,
		},
		"rate limit retries": {
			status:        429,
			body:          `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			wantKind:      KindRateLimited,
			wantRetryable: true,
		},
		"malformed request does not retry": {
			status:        400,
			body:          `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`,
			wantKind:      KindInvalidRequest,
			wantRetryable: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			p := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})

			_, err := p.Complete(context.Background(), Request{
				Model:    "claude-sonnet-4-5",
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
			if provErr.Model != "claude-sonnet-4-5" {
				t.Errorf("Model = %q, want it recorded for the Phase 7 breaker", provErr.Model)
			}
		})
	}
}
