package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Error responses mirror OpenAI's envelope, for the same reason the success
// path does: a client's existing error handling should keep working.

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

// statusForKind maps a provider failure onto the status the client should see.
//
// The interesting case is KindAuthFailed. It becomes 502, not 401. A 401 tells
// the caller *their* credential was rejected, when in fact SwitchYard's upstream
// key was — the caller can do nothing about it and would waste time rotating a
// key that was never the problem. From the client's perspective a bad upstream
// credential is a broken gateway, which is exactly what 502 means.
//
// KindContentPolicy stays a 400 because the caller genuinely can change the
// request that was refused.
func statusForKind(k provider.Kind) int {
	switch k {
	case provider.KindRateLimited:
		return http.StatusTooManyRequests
	case provider.KindTimeout:
		return http.StatusGatewayTimeout
	case provider.KindInvalidRequest, provider.KindContentPolicy:
		return http.StatusBadRequest
	case provider.KindAuthFailed, provider.KindServerError, provider.KindNetworkError:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

// writeError sends an error envelope. It never fails the request further: if
// encoding somehow breaks, the status line has already gone out.
func writeError(w http.ResponseWriter, log *slog.Logger, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	body := errorBody{Error: errorDetail{Message: message, Type: errType}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The client is already receiving a response; all that is left is to
		// record that the body was truncated.
		log.Warn("writing error response body", slog.Any("error", err))
	}
}

// writeProviderError translates a provider failure into a client response.
func writeProviderError(w http.ResponseWriter, log *slog.Logger, err error) {
	var provErr *provider.Error
	if !errors.As(err, &provErr) {
		// Every path into here should carry a *provider.Error. Anything else is
		// a bug in SwitchYard, not a provider fault, so it must not masquerade
		// as one.
		log.Error("provider call failed without a typed error", slog.Any("error", err))
		writeError(w, log, http.StatusInternalServerError, "internal_error",
			"the gateway encountered an unexpected error")
		return
	}

	// Pass the provider's own backoff instruction through, so a client that
	// respects Retry-After cooperates with the upstream rather than with
	// SwitchYard's guess.
	if provErr.RetryAfter > 0 {
		seconds := int(provErr.RetryAfter.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}

	message := provErr.Message
	if message == "" {
		message = "the upstream provider returned an error"
	}

	writeError(w, log, statusForKind(provErr.Kind), string(provErr.Kind), message)
}

// writeSSEError is writeProviderError's counterpart for a stream that has
// already sent its headers and at least one event: the HTTP status line is
// long gone, so the same error envelope goes out as an SSE "data:" event
// instead of a status-coded response. See Step 2.4 in PART1_PLAN.md.
func writeSSEError(w io.Writer, err error) error {
	var provErr *provider.Error
	if !errors.As(err, &provErr) {
		return writeSSEJSON(w, errorBody{Error: errorDetail{
			Message: "the gateway encountered an unexpected error",
			Type:    "internal_error",
		}})
	}

	message := provErr.Message
	if message == "" {
		message = "the upstream provider returned an error"
	}
	return writeSSEJSON(w, errorBody{Error: errorDetail{
		Message: message,
		Type:    string(provErr.Kind),
	}})
}
