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

func ollamaConfig(baseURL string) Config {
	return Config{
		Name:    "ollama",
		BaseURL: baseURL,
		// No APIKey on purpose: Ollama takes no credential.
		Timeout:          2 * time.Second,
		Models:           []string{"llama3.2:3b"},
		DefaultMaxTokens: 512,
	}
}

func newTestOllama(t *testing.T, handler http.HandlerFunc) *Ollama {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := NewOllama(ollamaConfig(srv.URL))
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	return p
}

// Ollama streams by default. Omitting the field would return newline-delimited
// JSON that this non-streaming path cannot parse, so false must be explicit on
// the wire — not merely absent.
func TestOllamaAlwaysSendsStreamFalse(t *testing.T) {
	var raw map[string]any
	var gotAuth string
	var gotPath string

	p := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &raw)
		io.WriteString(w, `{"model":"llama3.2:3b","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`)
	})

	if _, err := p.Complete(context.Background(), Request{
		Model:    "llama3.2:3b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotPath != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", gotPath)
	}
	streamed, present := raw["stream"]
	if !present {
		t.Fatal("stream field absent; Ollama defaults to streaming and would return NDJSON")
	}
	if streamed != false {
		t.Errorf("stream = %v, want an explicit false", streamed)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none: Ollama takes no credential", gotAuth)
	}
}

func TestOllamaTranslatesRequestAndResponse(t *testing.T) {
	var got ollamaRequest

	p := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{
			"model": "llama3.2:3b",
			"message": {"role": "assistant", "content": "hello back"},
			"done": true,
			"done_reason": "stop",
			"prompt_eval_count": 18,
			"eval_count": 5
		}`)
	})

	resp, err := p.Complete(context.Background(), Request{
		Model: "llama3.2:3b",
		Messages: []Message{
			{Role: RoleSystem, Content: "be terse"},
			{Role: RoleUser, Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Ollama handles the system role natively, so it stays inline.
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" {
		t.Errorf("messages = %+v, want the system message left in the array", got.Messages)
	}
	// max_tokens is called num_predict, and lives under options.
	if got.Options.NumPredict != 512 {
		t.Errorf("options.num_predict = %d, want the configured default 512", got.Options.NumPredict)
	}

	if resp.Content != "hello back" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.FinishReason != FinishStop {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, FinishStop)
	}
	// Ollama names its counts after evaluation phases, not input/output.
	if resp.Usage.InputTokens != 18 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want {18 5}", resp.Usage)
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q, want the instance name", resp.Provider)
	}
}

func TestOllamaClassify(t *testing.T) {
	tests := map[string]struct {
		status        int
		body          string
		wantKind      Kind
		wantRetryable bool
	}{
		"model not pulled does not retry": {
			status:        404,
			body:          `{"error":"model 'llama3.2:3b' not found, try pulling it first"}`,
			wantKind:      KindInvalidRequest,
			wantRetryable: false,
		},
		"server error retries": {
			status:        500,
			body:          `{"error":"llama runner process has terminated"}`,
			wantKind:      KindServerError,
			wantRetryable: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			p := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})

			_, err := p.Complete(context.Background(), Request{
				Model:    "llama3.2:3b",
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
		})
	}
}

// Ollama is the last link in the Phase 6 fallback chain, so a server that is
// simply not running must classify cleanly rather than as a mystery.
func TestOllamaOfflineIsNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	p, err := NewOllama(ollamaConfig(url))
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}

	_, err = p.Complete(context.Background(), Request{
		Model:    "llama3.2:3b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})

	var provErr *Error
	if !errors.As(err, &provErr) {
		t.Fatalf("err = %v, want a *provider.Error", err)
	}
	if provErr.Kind != KindNetworkError {
		t.Errorf("Kind = %q, want %q", provErr.Kind, KindNetworkError)
	}
	if !provErr.Retryable {
		t.Error("Retryable = false, want true: the server may come back")
	}
	if provErr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0: no response was ever received", provErr.StatusCode)
	}
}

// Ollama's stream is newline-delimited JSON, not SSE — a different wire
// format from the other three adapters, decoded by ndjsonStreamReader rather
// than sseStreamReader.
func TestOllamaStreamDecodesNDJSONLines(t *testing.T) {
	var raw map[string]any

	p := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &raw)

		flusher := w.(http.Flusher)
		for _, line := range []string{
			`{"model":"llama3.2:3b","message":{"role":"assistant","content":"hi"},"done":false}`,
			`{"model":"llama3.2:3b","message":{"role":"assistant","content":" there"},"done":false}`,
			`{"model":"llama3.2:3b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":7,"eval_count":2}`,
		} {
			io.WriteString(w, line+"\n")
			flusher.Flush()
		}
	})

	stream, err := p.Stream(context.Background(), Request{
		Model:    "llama3.2:3b",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, stream)

	streamed, present := raw["stream"]
	if !present || streamed != true {
		t.Errorf("stream field = %v (present=%v), want an explicit true", streamed, present)
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
	if usage == nil || usage.InputTokens != 7 || usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v, want {7 2}", usage)
	}
}
