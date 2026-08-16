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

// The marquee streaming test: input tokens arrive at message_start, output
// tokens arrive later at message_delta, and the two must end up combined on
// one Usage despite being reported a whole stream apart.
func TestAnthropicStreamDecodesNamedEvents(t *testing.T) {
	var gotBody anthropicRequest

	p := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("upstream received unparseable body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, block := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":15,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n",
			"event: ping\ndata: {\"type\":\"ping\"}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" there\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			io.WriteString(w, block)
			flusher.Flush()
		}
	})

	stream, err := p.Stream(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, stream)

	if !gotBody.Stream {
		t.Error("stream field not sent as true")
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
	// The load-bearing assertion: input_tokens from message_start combined with
	// output_tokens from message_delta, even though Anthropic never reports both
	// together in a single event.
	if usage == nil || usage.InputTokens != 15 || usage.OutputTokens != 4 {
		t.Errorf("Usage = %+v, want {15 4}", usage)
	}
}

// Anthropic can fail mid-stream on an HTTP 200 via a named "error" event, a
// failure the status-code check in Stream can never catch because the
// response headers already said success.
func TestAnthropicStreamMidStreamErrorEvent(t *testing.T) {
	p := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
		flusher.Flush()
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n")
		flusher.Flush()
	})

	stream, err := p.Stream(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	// message_start carries no forwardable content, so Recv's internal loop
	// keeps reading and the very next event — the error — comes back from this
	// same call rather than a second one.
	_, err = stream.Recv()
	var provErr *Error
	if !errors.As(err, &provErr) {
		t.Fatalf("err = %v, want a *provider.Error from the mid-stream error event", err)
	}
	if provErr.Message != "overloaded" {
		t.Errorf("Message = %q, want the error event's message", provErr.Message)
	}
}
