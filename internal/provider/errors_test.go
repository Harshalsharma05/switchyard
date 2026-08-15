package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestKindForStatus(t *testing.T) {
	tests := map[string]struct {
		status int
		want   Kind
	}{
		"bad request":            {400, KindInvalidRequest},
		"unauthorized":           {401, KindAuthFailed},
		"forbidden":              {403, KindAuthFailed},
		"model not found":        {404, KindInvalidRequest},
		"request timeout":        {408, KindTimeout},
		"payload too large":      {413, KindInvalidRequest},
		"unprocessable entity":   {422, KindInvalidRequest},
		"rate limited":           {429, KindRateLimited},
		"internal error":         {500, KindServerError},
		"bad gateway":            {502, KindServerError},
		"unavailable":            {503, KindServerError},
		"success is not a kind":  {200, ""},
		"redirect is not a kind": {302, ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := KindForStatus(tc.status); got != tc.want {
				t.Errorf("KindForStatus(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestKindDefaultRetryable(t *testing.T) {
	tests := map[string]struct {
		kind Kind
		want bool
	}{
		"rate limited is transient":    {KindRateLimited, true},
		"timeout is transient":         {KindTimeout, true},
		"server error is transient":    {KindServerError, true},
		"network error is transient":   {KindNetworkError, true},
		"auth failure is permanent":    {KindAuthFailed, false},
		"content policy is permanent":  {KindContentPolicy, false},
		"invalid request is permanent": {KindInvalidRequest, false},
		"unset kind is not retryable":  {"", false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.kind.DefaultRetryable(); got != tc.want {
				t.Errorf("Kind(%q).DefaultRetryable() = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// A 400 must not arrive as a retryable error. This is the specific hole that
// KindInvalidRequest exists to close: without it a malformed request would
// classify as KindServerError and be retried against every provider in the
// Phase 6 fallback chain, none of which can ever accept it.
func TestNewHTTPErrorDoesNotRetryClientErrors(t *testing.T) {
	err := NewHTTPError("groq", "openai/gpt-oss-120b", 400, "messages: field required")

	if err.Kind != KindInvalidRequest {
		t.Errorf("Kind = %q, want %q", err.Kind, KindInvalidRequest)
	}
	if err.Retryable {
		t.Error("Retryable = true, want false: a malformed request cannot succeed on retry")
	}
}

func TestNewHTTPErrorClassification(t *testing.T) {
	tests := map[string]struct {
		status        int
		wantKind      Kind
		wantRetryable bool
	}{
		"429 retries":  {429, KindRateLimited, true},
		"500 retries":  {500, KindServerError, true},
		"401 does not": {401, KindAuthFailed, false},
		"404 does not": {404, KindInvalidRequest, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := NewHTTPError("gemini", "gemini-2.0-flash", tc.status, "upstream said no")

			if err.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", err.Kind, tc.wantKind)
			}
			if err.Retryable != tc.wantRetryable {
				t.Errorf("Retryable = %v, want %v", err.Retryable, tc.wantRetryable)
			}
			if err.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", err.StatusCode, tc.status)
			}
			if err.Provider != "gemini" || err.Model != "gemini-2.0-flash" {
				t.Errorf("provider/model = %q/%q, want gemini/gemini-2.0-flash", err.Provider, err.Model)
			}
		})
	}
}

// timeoutErr satisfies net.Error and reports a timeout, standing in for what
// net/http returns when a dial or read deadline expires.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestNewTransportError(t *testing.T) {
	tests := map[string]struct {
		err           error
		wantKind      Kind
		wantRetryable bool
	}{
		"context deadline is a timeout": {context.DeadlineExceeded, KindTimeout, true},
		"wrapped deadline is a timeout": {fmt.Errorf("calling upstream: %w", context.DeadlineExceeded), KindTimeout, true},
		"net timeout is a timeout":      {timeoutErr{}, KindTimeout, true},
		"connection refused is network": {errors.New("dial tcp: connection refused"), KindNetworkError, true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := NewTransportError("ollama", "llama3.2:3b", tc.err)

			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Retryable != tc.wantRetryable {
				t.Errorf("Retryable = %v, want %v", got.Retryable, tc.wantRetryable)
			}
			if got.StatusCode != 0 {
				t.Errorf("StatusCode = %d, want 0: no response was ever received", got.StatusCode)
			}
		})
	}
}

// The resilience layers in Phases 6 and 7 recover the typed error through
// however many layers of fmt.Errorf wrapping sit between them and the adapter.
func TestErrorUnwrapping(t *testing.T) {
	base := NewTransportError("groq", "openai/gpt-oss-120b", context.DeadlineExceeded)
	wrapped := fmt.Errorf("resolving fallback chain: %w", fmt.Errorf("calling groq: %w", base))

	var pe *Error
	if !errors.As(wrapped, &pe) {
		t.Fatal("errors.As did not recover *provider.Error through two layers of wrapping")
	}
	if pe.Kind != KindTimeout {
		t.Errorf("Kind = %q, want %q", pe.Kind, KindTimeout)
	}

	// Unwrap must also reach past the *Error to the sentinel underneath it.
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("errors.Is could not reach context.DeadlineExceeded through *provider.Error")
	}
}
