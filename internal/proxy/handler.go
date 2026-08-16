package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// ChatCompletions serves POST /v1/chat/completions, streaming and
// non-streaming alike — both reach this same handler behind the same
// middleware chain, per Step 2.3; only the request's stream flag decides
// which path runs.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}

	prov, ok := h.resolve(w, r, req.Model)
	if !ok {
		return
	}

	if req.Stream {
		h.streamChatCompletions(w, r, prov, req)
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
		metrics.usage = resp.Usage
	}

	h.writeSuccess(w, r, resp)
}

// resolve looks up the provider for a model, writing the client error itself
// on failure. It is shared by the streaming and non-streaming paths so model
// resolution — and its error mapping — exists in exactly one place.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, model string) (provider.Provider, bool) {
	prov, err := h.resolver.ForModel(model)
	if err != nil {
		if errors.Is(err, provider.ErrModelNotSupported) {
			writeError(w, h.log, http.StatusNotFound, "model_not_found",
				"no provider is configured to serve model "+strconv.Quote(model))
			return nil, false
		}
		h.log.ErrorContext(r.Context(), "resolving model", slog.Any("error", err))
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not resolve a provider for this model")
		return nil, false
	}
	return prov, true
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

// streamChatCompletions serves the stream:true path. It shares this file's
// middleware chain and model resolution with the non-streaming path (Step
// 2.3's "no duplicated logic" requirement) and differs only from here down:
// normalized provider.Chunks are translated to OpenAI-shaped SSE events and
// flushed to the client as they arrive, never buffered into a full response
// first.
func (h *Handler) streamChatCompletions(w http.ResponseWriter, r *http.Request, prov provider.Provider, req chatRequest) {
	metrics := metricsFrom(r.Context())
	requestID := RequestIDFrom(r.Context())

	flusher, ok := w.(http.Flusher)
	if !ok {
		// middleware.go's headerHook and recorder both forward http.Flusher
		// deliberately so this always succeeds in the real chain; this only
		// fires on a genuine server misconfiguration, never on client input.
		h.log.ErrorContext(r.Context(), "response writer does not support flushing")
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway cannot stream a response")
		return
	}

	// Passing r.Context() straight through is what makes a client disconnect
	// cancel the upstream call: net/http cancels a request's Context when the
	// client's connection closes, and that context is what Stream used to
	// build the outbound request. No extra wiring is needed for Step 2.3's
	// "client disconnect cancels the upstream provider request."
	callStart := time.Now()
	stream, err := prov.Stream(r.Context(), req.toProviderRequest())
	if err != nil {
		if metrics != nil {
			metrics.providerName = prov.Name()
			metrics.providerTime = time.Since(callStart)
		}
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider stream call failed",
			slog.String("request_id", requestID),
			slog.String("provider", prov.Name()),
			slog.String("model", req.Model),
			slog.Any("error", err),
		)
		// Nothing has reached the client yet, so — per Step 2.4 — this is a
		// normal HTTP error response, exactly like the non-streaming path.
		writeProviderError(w, h.log, err)
		return
	}
	defer stream.Close()

	if metrics != nil {
		metrics.providerName = prov.Name()
		metrics.servedModel = req.Model
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id := chatCompletionID(requestID)
	created := time.Now().Unix()

	var content strings.Builder
	var finish provider.FinishReason
	var usage provider.Usage

	// gotFirstByte and sentAny track two different things, and collapsing them
	// into one flag was a real bug caught against live Groq traffic: a
	// reasoning model sends dozens of content-free, finish-reason-free chunks
	// before any real token, and treating "we have a byte from the provider"
	// as the same moment as "we wrote something to the client" made
	// providerTime accurate but also made every one of those empty chunks
	// look like "the" first chunk once the empty-chunk filter below was added.
	gotFirstByte := false
	sentAny := false

	for {
		chunk, err := stream.Recv()

		// Measured once, at whatever the stream's first Recv() outcome turns
		// out to be — a chunk, a clean EOF, or an error. That makes providerTime
		// a time-to-first-byte figure and, per middleware.go's headerHook
		// comment, the overhead this request reports is a time-to-first-token
		// measurement: the right boundary for a stream, where total duration is
		// dominated by generation, not by the gateway.
		if !gotFirstByte && metrics != nil {
			metrics.providerTime = time.Since(callStart)
			gotFirstByte = true
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			if r.Context().Err() != nil {
				// The client is gone, which is what cancelled the upstream call.
				// Nobody is reading, so there is nothing left to write.
				h.log.LogAttrs(r.Context(), slog.LevelInfo, "client disconnected mid-stream",
					slog.String("request_id", requestID),
				)
				return
			}

			if !sentAny {
				// Nothing has reached the client yet — this is functionally a
				// Stream() failure that just happened to surface on a later Recv
				// instead, so it gets the same normal-error treatment.
				h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider stream failed before any chunk",
					slog.String("request_id", requestID),
					slog.String("provider", prov.Name()),
					slog.Any("error", err),
				)
				writeProviderError(w, h.log, err)
				return
			}

			// Step 2.4: headers and at least one chunk are already on the wire,
			// so the status line cannot change now. An SSE error event is the
			// only way left to tell the client this failed, and the stream ends
			// here — no [DONE], and no fallback: partial content already
			// reached the client, so retrying would duplicate it.
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider stream failed mid-stream",
				slog.String("request_id", requestID),
				slog.String("provider", prov.Name()),
				slog.Any("error", err),
			)
			if err := writeSSEError(w, err); err != nil {
				h.log.LogAttrs(r.Context(), slog.LevelWarn, "writing stream error event",
					slog.String("request_id", requestID), slog.Any("error", err))
			}
			flusher.Flush()
			return
		}

		content.WriteString(chunk.Content)
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}

		// A chunk with no content and no finish reason carries nothing a client
		// can use — SwitchYard never puts Usage on the wire (see toChatChunk),
		// so that is the only thing such a chunk could otherwise be for. Some
		// providers send many of these (a reasoning model's empty leading
		// deltas, or the trailing usage-only event stream_options.include_usage
		// asks OpenAI for); forwarding each as its own SSE event would still be
		// "streaming," just needlessly noisy on the wire.
		if chunk.Content == "" && chunk.FinishReason == "" {
			continue
		}

		if err := writeSSEJSON(w, toChatChunk(id, created, req.Model, chunk, !sentAny)); err != nil {
			// A write failure here almost always means the connection is gone —
			// the same condition the ctx.Done() check above exists for, just
			// observed a different way. Stop rather than keep paying for a
			// stream nobody is reading.
			h.log.LogAttrs(r.Context(), slog.LevelInfo, "writing stream chunk",
				slog.String("request_id", requestID), slog.Any("error", err))
			return
		}
		flusher.Flush()
		sentAny = true
	}

	if metrics != nil {
		metrics.usage = usage
	}

	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "writing stream terminator",
			slog.String("request_id", requestID), slog.Any("error", err))
		return
	}
	flusher.Flush()

	h.log.LogAttrs(r.Context(), slog.LevelInfo, "stream completed",
		slog.String("request_id", requestID),
		slog.String("provider", prov.Name()),
		slog.String("finish_reason", string(finish)),
		slog.Int("input_tokens", usage.InputTokens),
		slog.Int("output_tokens", usage.OutputTokens),
		slog.Int("content_bytes", content.Len()),
	)
}

// writeSSEJSON marshals v and writes it as one SSE "data:" event. The blank
// line after the payload is the SSE spec's event terminator, not cosmetic
// formatting — omitting it would merge this event with the next one for any
// spec-compliant SSE parser.
func writeSSEJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling SSE payload: %w", err)
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
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
