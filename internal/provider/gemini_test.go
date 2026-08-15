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

func geminiConfig(baseURL string) Config {
	return Config{
		Name:             "gemini",
		BaseURL:          baseURL,
		APIKey:           "goog-test-key",
		Timeout:          2 * time.Second,
		Models:           []string{"gemini-2.0-flash", "gemini-2.0-flash-lite"},
		DefaultMaxTokens: 1024,
	}
}

func newTestGemini(t *testing.T, handler http.HandlerFunc) *Gemini {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := NewGemini(geminiConfig(srv.URL))
	if err != nil {
		t.Fatalf("NewGemini: %v", err)
	}
	return p
}

func TestGeminiTranslatesRequest(t *testing.T) {
	var got geminiRequest
	var gotPath, gotKeyHeader, gotQuery string

	p := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKeyHeader = r.Header.Get("x-goog-api-key")

		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unparseable request body: %v", err)
		}
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}]}`)
	})

	temp := float32(0.5)
	_, err := p.Complete(context.Background(), Request{
		Model: "gemini-2.0-flash",
		Messages: []Message{
			{Role: RoleSystem, Content: "be terse"},
			{Role: RoleUser, Content: "hello"},
			{Role: RoleAssistant, Content: "hi"},
		},
		Temperature: &temp,
		MaxTokens:   256,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// The model belongs in the path for this API, not the body.
	if gotPath != "/models/gemini-2.0-flash:generateContent" {
		t.Errorf("path = %q, want the model in the URL", gotPath)
	}
	// The credential must be a header, never a query parameter — a key in a URL
	// ends up in access logs and trace attributes.
	if gotKeyHeader != "goog-test-key" {
		t.Errorf("x-goog-api-key = %q, want the configured key", gotKeyHeader)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want the credential kept out of the URL", gotQuery)
	}

	if got.SystemInstruction == nil || got.SystemInstruction.Parts[0].Text != "be terse" {
		t.Errorf("systemInstruction = %+v, want the system message hoisted", got.SystemInstruction)
	}
	if len(got.Contents) != 2 {
		t.Fatalf("contents = %d, want 2 with the system message removed", len(got.Contents))
	}
	// The assistant is called "model" in this dialect.
	if got.Contents[1].Role != "model" {
		t.Errorf("assistant role = %q, want %q", got.Contents[1].Role, "model")
	}
	if got.Contents[0].Parts[0].Text != "hello" {
		t.Errorf("user text = %q, want it wrapped in a part", got.Contents[0].Parts[0].Text)
	}
	if got.GenerationConfig.MaxOutputTokens != 256 {
		t.Errorf("maxOutputTokens = %d, want 256", got.GenerationConfig.MaxOutputTokens)
	}
	if got.GenerationConfig.Temperature == nil || *got.GenerationConfig.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5 nested in generationConfig", got.GenerationConfig.Temperature)
	}
}

func TestGeminiTranslatesResponse(t *testing.T) {
	p := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"candidates": [{
				"content": {"parts": [{"text": "one "}, {"text": "two"}], "role": "model"},
				"finishReason": "MAX_TOKENS"
			}],
			"usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 34},
			"modelVersion": "gemini-2.0-flash-001"
		}`)
	})

	resp, err := p.Complete(context.Background(), Request{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp.Content != "one two" {
		t.Errorf("Content = %q, want the parts concatenated", resp.Content)
	}
	if resp.FinishReason != FinishLength {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, FinishLength)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 34 {
		t.Errorf("Usage = %+v, want {12 34}", resp.Usage)
	}
	// The served model is what actually answered, which here is more specific
	// than what was asked for.
	if resp.Model != "gemini-2.0-flash-001" {
		t.Errorf("Model = %q, want the served version", resp.Model)
	}
}

// The case that justifies KindContentPolicy not being derivable from a status
// code: a refusal arrives as HTTP 200.
func TestGeminiBlockedPromptIsContentPolicyDespite200(t *testing.T) {
	p := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"promptFeedback":{"blockReason":"SAFETY"},"candidates":[]}`)
	})

	_, err := p.Complete(context.Background(), Request{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})

	var provErr *Error
	if !errors.As(err, &provErr) {
		t.Fatalf("err = %v, want a *provider.Error rather than an empty success", err)
	}
	if provErr.Kind != KindContentPolicy {
		t.Errorf("Kind = %q, want %q", provErr.Kind, KindContentPolicy)
	}
	if provErr.Retryable {
		t.Error("Retryable = true; a refused prompt is refused every time")
	}
	if provErr.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200 recorded honestly", provErr.StatusCode)
	}
}

func TestGeminiFinishReasons(t *testing.T) {
	tests := map[string]FinishReason{
		"STOP":               FinishStop,
		"MAX_TOKENS":         FinishLength,
		"SAFETY":             FinishContentFilter,
		"RECITATION":         FinishContentFilter,
		"PROHIBITED_CONTENT": FinishContentFilter,
		"OTHER":              FinishOther,
	}

	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := geminiFinishReason(in); got != want {
				t.Errorf("geminiFinishReason(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestGeminiClassify(t *testing.T) {
	tests := map[string]struct {
		status        int
		body          string
		wantKind      Kind
		wantRetryable bool
	}{
		"resource exhausted retries": {
			status:        429,
			body:          `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`,
			wantKind:      KindRateLimited,
			wantRetryable: true,
		},
		"bad key does not retry": {
			status:        401,
			body:          `{"error":{"code":401,"message":"invalid key","status":"UNAUTHENTICATED"}}`,
			wantKind:      KindAuthFailed,
			wantRetryable: false,
		},
		"region restriction does not retry": {
			status:        400,
			body:          `{"error":{"code":400,"message":"free tier unavailable","status":"FAILED_PRECONDITION"}}`,
			wantKind:      KindInvalidRequest,
			wantRetryable: false,
		},
		"unavailable retries": {
			status:        503,
			body:          `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`,
			wantKind:      KindServerError,
			wantRetryable: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			p := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})

			_, err := p.Complete(context.Background(), Request{
				Model:    "gemini-2.0-flash",
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
