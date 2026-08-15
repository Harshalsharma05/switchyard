package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Kind classifies why a provider call failed.
//
// It is a named string rather than an iota int for two reasons. It becomes a
// Prometheus label directly in Phase 9 without a conversion step, and its zero
// value is "" rather than a valid classification — so a Kind that nobody set
// reads as an obvious bug instead of silently meaning KindRateLimited.
type Kind string

const (
	// KindRateLimited is a 429 or provider-signalled throttle.
	KindRateLimited Kind = "rate_limited"
	// KindTimeout is a deadline exceeded before the provider responded.
	KindTimeout Kind = "timeout"
	// KindAuthFailed is a rejected or missing credential.
	KindAuthFailed Kind = "auth_failed"
	// KindContentPolicy is a provider refusing on safety grounds. This is rarely
	// inferable from a status code — OpenAI returns 400 with a code in the body,
	// Gemini returns 200 with a blockReason — so adapters set it from the body.
	KindContentPolicy Kind = "content_policy"
	// KindServerError is a 5xx from the provider.
	KindServerError Kind = "server_error"
	// KindNetworkError is a transport failure with no HTTP response at all.
	KindNetworkError Kind = "network_error"
	// KindInvalidRequest is a request the provider will never accept: malformed
	// body, unknown model, context length exceeded. Retrying it anywhere in the
	// Phase 6 chain is wasted work, which is why it needs its own kind rather
	// than falling into KindServerError.
	KindInvalidRequest Kind = "invalid_request"
)

// DefaultRetryable reports whether a Kind is worth another attempt in the
// absence of better information.
//
// This is only the default. Adapters override the Retryable field on a specific
// Error when the response body tells them more than the status code does: a 429
// from a soft rate limit is worth retrying, a 429 from an exhausted billing
// quota is not, and both arrive as the same status.
func (k Kind) DefaultRetryable() bool {
	switch k {
	case KindRateLimited, KindTimeout, KindServerError, KindNetworkError:
		return true
	default:
		return false
	}
}

// Error is the canonical provider failure.
//
// Every adapter maps its provider's error responses onto this type. Callers
// branch on Kind and Retryable rather than on status codes, so the retry,
// fallback, and circuit breaker layers never need to know which provider they
// are talking to.
type Error struct {
	Kind     Kind
	Provider string

	// Model is recorded because the Phase 7 breaker is keyed on provider+model,
	// not provider alone, and by then the failing model is otherwise lost.
	Model string

	// StatusCode is the upstream HTTP status, or 0 for transport-level failures
	// that never received a response.
	StatusCode int

	// Retryable is stored rather than derived from Kind at read time because the
	// adapter has read the response body and the retry layer has not. Deriving
	// it later throws that knowledge away.
	Retryable bool

	// RetryAfter is the provider's own backoff instruction, parsed from its
	// Retry-After header. Zero means the provider did not say. Phase 6 prefers a
	// non-zero value here over its own computed backoff.
	RetryAfter time.Duration

	// Message is a short human-readable summary, safe for logs. Adapters must
	// not put prompt or response content here.
	Message string

	// Err is the underlying cause, if any.
	Err error
}

func (e *Error) Error() string {
	target := e.Provider
	if e.Model != "" {
		target = fmt.Sprintf("%s/%s", e.Provider, e.Model)
	}

	s := fmt.Sprintf("provider %s: %s", target, e.Kind)
	if e.StatusCode != 0 {
		s += fmt.Sprintf(" (http %d)", e.StatusCode)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap exposes the underlying cause so errors.Is reaches through to sentinels
// like context.DeadlineExceeded.
func (e *Error) Unwrap() error { return e.Err }

// KindForStatus maps an upstream HTTP status onto a Kind.
//
// It is meaningful only for statuses at or above 400; anything else returns the
// zero Kind, which is deliberately not a valid classification so that misuse
// surfaces rather than being silently absorbed.
//
// Content policy violations are not derivable here — see KindContentPolicy.
func KindForStatus(status int) Kind {
	switch {
	case status < http.StatusBadRequest:
		return ""
	case status == http.StatusRequestTimeout:
		return KindTimeout
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return KindAuthFailed
	case status >= http.StatusInternalServerError:
		return KindServerError
	default:
		// Every other 4xx is something the provider will reject again no matter
		// how many times we ask: bad body, unknown model, payload too large.
		return KindInvalidRequest
	}
}

// NewHTTPError builds an Error from an upstream response status, defaulting
// Kind and Retryable from that status. Adapters call this first and then refine
// the result from the provider's error body.
func NewHTTPError(providerName, model string, status int, message string) *Error {
	kind := KindForStatus(status)
	return &Error{
		Kind:       kind,
		Provider:   providerName,
		Model:      model,
		StatusCode: status,
		Retryable:  kind.DefaultRetryable(),
		Message:    message,
	}
}

// NewTransportError builds an Error for a failure that never produced an HTTP
// response, distinguishing a timeout from a general network fault.
//
// The two checks differ on purpose: errors.Is compares against a sentinel value
// by identity, which is what context.DeadlineExceeded is; errors.As finds an
// error in the chain that satisfies an interface, which is how net.Error's
// Timeout method has to be reached.
func NewTransportError(providerName, model string, err error) *Error {
	kind := KindNetworkError

	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = KindTimeout
	case errors.As(err, &netErr) && netErr.Timeout():
		kind = KindTimeout
	}

	return &Error{
		Kind:      kind,
		Provider:  providerName,
		Model:     model,
		Retryable: kind.DefaultRetryable(),
		Err:       err,
	}
}
