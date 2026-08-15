package proxy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// maxRequestBytes bounds an inbound body. A gateway accepting unbounded JSON is
// a memory exhaustion vector, and no legitimate chat request approaches this.
const maxRequestBytes = 1 << 20 // 1 MiB

// Resolver is the slice of the provider registry this handler needs.
//
// It is declared here, by the consumer, rather than taken as a *provider.Registry:
// the handler needs one method, and depending on the interface rather than the
// concrete type is what lets tests inject a fake and what will let Phase 6 slot a
// fallback-aware resolver in front of the registry without touching this file.
type Resolver interface {
	ForModel(model string) (provider.Provider, error)
}

// Handler serves the public API.
type Handler struct {
	resolver Resolver
	log      *slog.Logger
}

// NewHandler wires the handler with its dependencies. Nothing here reads
// package-level state.
func NewHandler(resolver Resolver, log *slog.Logger) *Handler {
	return &Handler{resolver: resolver, log: log}
}

// ChatCompletions serves POST /v1/chat/completions, non-streaming.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}

	if req.Stream {
		// Rejected explicitly rather than served non-streaming: silently
		// ignoring the flag would hand a client that asked for a stream a
		// response it cannot parse.
		writeError(w, h.log, http.StatusBadRequest, "invalid_request_error",
			"streaming is not supported yet")
		return
	}

	prov, err := h.resolver.ForModel(req.Model)
	if err != nil {
		if errors.Is(err, provider.ErrModelNotSupported) {
			writeError(w, h.log, http.StatusNotFound, "model_not_found",
				"no provider is configured to serve model "+strconv.Quote(req.Model))
			return
		}
		h.log.ErrorContext(r.Context(), "resolving model", slog.Any("error", err))
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not resolve a provider for this model")
		return
	}

	metrics := metricsFrom(r.Context())

	callStart := time.Now()
	resp, err := prov.Complete(r.Context(), req.toProviderRequest())
	callElapsed := time.Since(callStart)

	if err != nil {
		if metrics != nil {
			metrics.providerName = prov.Name()
			// No Response means no measured round trip, so the wall time around
			// the call stands in. On a failure path the adapter's translation
			// work is negligible, so this is close enough not to distort the
			// overhead number.
			metrics.providerTime = callElapsed
		}

		h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider call failed",
			slog.String("request_id", RequestIDFrom(r.Context())),
			slog.String("provider", prov.Name()),
			slog.String("model", req.Model),
			slog.Any("error", err),
		)
		writeProviderError(w, h.log, err)
		return
	}

	if metrics != nil {
		metrics.providerName = resp.Provider
		metrics.servedModel = resp.Model
		// resp.Latency is the HTTP round trip alone, measured inside the
		// adapter. Using it rather than callElapsed means the adapter's own
		// request and response translation counts as gateway overhead — which
		// it is. That makes the headline number larger, and honest.
		metrics.providerTime = resp.Latency
	}

	h.writeSuccess(w, r, resp)
}

// decode reads and validates the request body, writing the client error itself
// and reporting whether the caller should continue.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request) (chatRequest, bool) {
	var req chatRequest

	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeError(w, h.log, http.StatusUnsupportedMediaType, "invalid_request_error",
			"Content-Type must be application/json")
		return req, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	dec := json.NewDecoder(r.Body)
	// Unknown fields are rejected so a caller misspelling max_tokens learns
	// about it instead of silently getting the default.
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, h.log, http.StatusRequestEntityTooLarge, "invalid_request_error",
				"request body exceeds the 1 MiB limit")
			return req, false
		}
		writeError(w, h.log, http.StatusBadRequest, "invalid_request_error",
			"request body is not valid JSON: "+err.Error())
		return req, false
	}

	if err := req.validate(); err != nil {
		writeError(w, h.log, http.StatusBadRequest, "invalid_request_error", err.Error())
		return req, false
	}

	return req, true
}

func (h *Handler) writeSuccess(w http.ResponseWriter, r *http.Request, resp *provider.Response) {
	requestID := RequestIDFrom(r.Context())
	body := toChatResponse(requestID, time.Now().Unix(), resp)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Headers and status are already on the wire, so there is no way to
		// turn this into an error response. Record it and move on.
		h.log.ErrorContext(r.Context(), "writing response body",
			slog.String("request_id", requestID),
			slog.Any("error", err),
		)
	}
}
