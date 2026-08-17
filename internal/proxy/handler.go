package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/budget"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
)

// maxRequestBytes bounds an inbound body. A gateway accepting unbounded JSON is
// a memory exhaustion vector, and no legitimate chat request approaches this.
const maxRequestBytes = 1 << 20 // 1 MiB

// Resolver is the slice of the provider registry this handler needs.
//
// It is declared here, by the consumer, rather than taken as a *provider.Registry:
// the handler needs these two methods, and depending on the interface rather
// than the concrete type is what lets tests inject a fake and what will let
// Phase 6 slot a fallback-aware resolver in front of the registry without
// touching this file.
type Resolver interface {
	ForModel(model string) (provider.Provider, error)

	// DefaultMaxTokensFor returns the resolved instance's configured ceiling
	// for model, the same value substituted when a caller omits max_tokens.
	// Step 3.3's TPM reservation needs it to estimate a request's cost before
	// the provider is ever called.
	DefaultMaxTokensFor(model string) (int, bool)
}

// CostCalculator is the slice of budget.Calculator this package needs.
//
// Declared here, by the consumer, for the same reason Resolver and
// RateLimiter are: the handler depends on turning a model and a token count
// into a price, not on how that price was loaded or computed — which is what
// lets a test inject a fake without building a real pricing table.
type CostCalculator interface {
	Cost(model string, inputTokens, outputTokens int) (int64, error)
}

// Handler serves the public API.
type Handler struct {
	resolver      Resolver
	limiter       RateLimiter
	budgetTracker BudgetTracker
	calc          CostCalculator
	log           *slog.Logger
}

// NewHandler wires the handler with its dependencies. Nothing here reads
// package-level state.
func NewHandler(resolver Resolver, limiter RateLimiter, budgetTracker BudgetTracker, calc CostCalculator, log *slog.Logger) *Handler {
	return &Handler{resolver: resolver, limiter: limiter, budgetTracker: budgetTracker, calc: calc, log: log}
}

// recordCost prices a finished request and stores it on metrics for Logger to
// report.
//
// A pricing-lookup failure is logged and left at zero rather than surfaced to
// the caller: by the time this runs, the response has already succeeded (or
// the stream has already completed) and cost accounting must never become the
// reason a request fails — the same rule CLAUDE.md states for telemetry.
func (h *Handler) recordCost(ctx context.Context, metrics *requestMetrics, model string, usage provider.Usage) {
	if metrics == nil {
		return
	}
	cost, err := h.calc.Cost(model, usage.InputTokens, usage.OutputTokens)
	if err != nil {
		h.log.ErrorContext(ctx, "computing request cost",
			slog.String("model", model), slog.Any("error", err))
		return
	}
	metrics.costMicros = cost
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

	if !h.authorizeModel(w, r, req.Model) {
		return
	}

	prov, ok := h.resolve(w, r, req.Model)
	if !ok {
		return
	}

	reservation, ok := h.reserveTokens(w, r, req)
	if !ok {
		return
	}

	// actual is set right before every return below it, so the deferred
	// Reconcile — which runs after streamChatCompletions has already
	// returned, since it is called synchronously — always settles against
	// real usage. It starts at zero on purpose: an error return that never
	// reassigns it means "nothing was generated," which is the correct
	// reservation to give back in full.
	var actual int
	defer func() {
		// Not r.Context(): a client disconnect or a request that already hit
		// its deadline both cancel that context, and Reconcile running on it
		// would fail before the Redis call could even happen — silently
		// leaking the reservation until its bucket TTL expired. See Step
		// 3.3's DECISIONS.md entry on why the defer has to survive that.
		ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		defer cancel()
		if err := reservation.Reconcile(ctx, actual); err != nil {
			h.log.ErrorContext(r.Context(), "reconciling TPM reservation",
				slog.String("request_id", RequestIDFrom(r.Context())), slog.Any("error", err))
		}
	}()

	budgetReservation, ok := h.reserveBudget(w, r, req)
	if !ok {
		// The TPM defer above still fires with actual left at zero — a full
		// refund, correct here since no provider was ever called. Nothing
		// about this reservation itself needs unwinding: reserveBudget only
		// returns a non-nil handle when it succeeded.
		return
	}

	// Mirrors the TPM defer immediately above: registered right after a
	// successful reservation, so it settles on every exit path, and reads
	// costMicros from metrics rather than a local variable because
	// recordCost — called from both the streaming and non-streaming paths
	// below — is what actually knows the real cost once usage is final.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		defer cancel()
		var actualCostMicros int64
		if m := metricsFrom(r.Context()); m != nil {
			actualCostMicros = m.costMicros
		}
		if err := budgetReservation.Reconcile(ctx, actualCostMicros); err != nil {
			h.log.ErrorContext(r.Context(), "reconciling budget reservation",
				slog.String("request_id", RequestIDFrom(r.Context())), slog.Any("error", err))
		}
	}()

	if req.Stream {
		actual = h.streamChatCompletions(w, r, prov, req)
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
		h.recordCost(r.Context(), metrics, resp.Model, resp.Usage)
	}
	actual = resp.Usage.InputTokens + resp.Usage.OutputTokens

	h.writeSuccess(w, r, resp)
}

// reserveTokens checks and reserves the team's TPM bucket ahead of a
// provider call, once the model is known to resolve and be allowed.
//
// The returned *ratelimit.Reservation is nil whenever there is nothing to
// reconcile later: either Redis was unreachable and the check failed open,
// or the request was denied (in which case ok is also false and the caller
// already wrote a response). Reservation.Reconcile is a safe no-op on a nil
// receiver, so ChatCompletions never has to branch on which case it is.
func (h *Handler) reserveTokens(w http.ResponseWriter, r *http.Request, req chatRequest) (*ratelimit.Reservation, bool) {
	team := TeamFrom(r.Context())
	if team == nil {
		h.log.ErrorContext(r.Context(), "chat completions reached without an authenticated team")
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not resolve the caller's team")
		return nil, false
	}

	defaultMaxTokens, ok := h.resolver.DefaultMaxTokensFor(req.Model)
	if !ok {
		// resolve() already succeeded for this exact model, so a miss here is
		// a wiring bug between the two Resolver methods, not a real one.
		h.log.ErrorContext(r.Context(), "resolved model has no default max tokens",
			slog.String("model", req.Model))
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not size this request's token reservation")
		return nil, false
	}

	amount := req.estimateTokens(defaultMaxTokens)

	// checkTimeout, not r.Context() directly: bounds how long a Redis problem
	// can add to this request before failing open, independent of the
	// underlying client's own retry behavior. See ratelimit.go's doc comment
	// on checkTimeout for the 3.5-second failure this measured against.
	checkCtx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()

	reservation, res, err := h.limiter.Reserve(checkCtx, team.ID, ratelimit.TPM, team.RateLimits.TPM, amount, bucketTTL)
	if err != nil {
		// Fail open, same as the RPM middleware: Redis being unreachable must
		// never be the reason a request fails.
		h.log.ErrorContext(r.Context(), "TPM rate limit check failed; failing open",
			slog.String("team", team.ID), slog.Any("error", err))
		return nil, true
	}
	if !res.Allowed {
		writeRateLimitError(w, h.log, ratelimit.TPM, team.RateLimits.TPM, res)
		return nil, false
	}

	// The bucket admitted this reservation, but a batch-priority team past
	// Step 3.5's threshold is shed anyway. Unlike RPM's fixed 1-unit cost,
	// this reservation is a ceiling (estimated input plus max_tokens) that
	// can be far larger than anything actually used — nothing was generated,
	// no provider was ever called, so real usage is unambiguously zero.
	// Reconciling with 0 gives the whole reservation back rather than
	// leaving a request that never reached a provider looking as expensive
	// as one that did.
	if shouldShedForPriority(team, team.RateLimits.TPM, res) {
		// A fresh context, not checkCtx: checkCtx was budgeted for the
		// Reserve call above and may already be close to its 200ms ceiling,
		// which could cut this refund off before it even starts. Reconcile
		// gets the same standalone budget as the deferred one in
		// ChatCompletions, for the same reason — giving back a reservation
		// has to happen regardless of how much of the check's own budget is
		// left.
		reconcileCtx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		if err := reservation.Reconcile(reconcileCtx, 0); err != nil {
			h.log.ErrorContext(r.Context(), "refunding priority-shed TPM reservation",
				slog.String("team", team.ID), slog.Any("error", err))
		}
		cancel()

		writePriorityShedError(w, h.log, ratelimit.TPM, team.RateLimits.TPM, res)
		return nil, false
	}

	return reservation, true
}

// reserveBudget checks and reserves against the team's monthly spend cap,
// once the TPM bucket has already admitted the request.
//
// Unlike reserveTokens, a Redis failure here denies the request rather than
// letting it through: per CLAUDE.md, budget enforcement fails *closed*,
// because an unrecoverable dollar is a different class of problem than a
// delayed request — the one dependency in this gateway where "never be the
// reason a request fails" is deliberately overridden.
func (h *Handler) reserveBudget(w http.ResponseWriter, r *http.Request, req chatRequest) (*budget.Reservation, bool) {
	team := TeamFrom(r.Context())
	if team == nil {
		h.log.ErrorContext(r.Context(), "chat completions reached without an authenticated team")
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not resolve the caller's team")
		return nil, false
	}

	defaultMaxTokens, ok := h.resolver.DefaultMaxTokensFor(req.Model)
	if !ok {
		// resolve() already succeeded for this exact model, so a miss here is
		// a wiring bug between Resolver's two methods, not a real one — same
		// reasoning reserveTokens gives for its own identical check.
		h.log.ErrorContext(r.Context(), "resolved model has no default max tokens",
			slog.String("model", req.Model))
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not size this request's budget reservation")
		return nil, false
	}

	estimatedMicros, err := h.calc.Cost(req.Model, req.estimateInputTokens(), req.estimateOutputCeiling(defaultMaxTokens))
	if err != nil {
		// Same "wiring bug, not a real gap" reasoning as recordCost: resolve()
		// already proved a provider serves this model, and every served model
		// has a pricing entry by construction.
		h.log.ErrorContext(r.Context(), "estimating request cost",
			slog.String("model", req.Model), slog.Any("error", err))
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not price this request")
		return nil, false
	}

	// checkTimeout, not r.Context() directly: same reasoning as
	// reserveTokens's identical use of it — bounds how long a Redis problem
	// can add to this request, independent of the underlying client's own
	// retry behavior.
	checkCtx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()

	reservation, res, err := h.budgetTracker.Reserve(checkCtx, team.ID, team.MonthlyBudgetMicros, estimatedMicros)
	if err != nil {
		h.log.ErrorContext(r.Context(), "budget check failed; failing closed",
			slog.String("team", team.ID), slog.Any("error", err))
		writeBudgetUnavailableError(w, h.log)
		return nil, false
	}

	if !res.Allowed {
		writeBudgetExceededError(w, h.log, team.ID, res.SpentMicros, team.MonthlyBudgetMicros)
		return nil, false
	}

	// res.SpentMicros is the total *after* admitting this request, the same
	// "after, not reconstructed to before" reading Step 3.5 already uses for
	// its own threshold check — the request that crosses the 80% line is the
	// one that gets the warning.
	if utilization(res.SpentMicros, team.MonthlyBudgetMicros) >= budgetWarnThreshold {
		w.Header().Set(HeaderBudgetWarning, "true")
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "team approaching monthly budget",
			slog.String("team", team.ID),
			slog.Int64("spent_micros", res.SpentMicros),
			slog.Int64("cap_micros", team.MonthlyBudgetMicros),
		)
	}

	return reservation, true
}

// authorizeModel checks the authenticated team's model allowlist.
//
// It runs after decode, which is the earliest point the requested model is
// known, and before resolve, so a team never learns whether SwitchYard could
// even route a model it isn't allowed to use. A model absent from every
// provider still gets its own distinct 404 from resolve — "not allowed" and
// "doesn't exist" are different failures and Step 3.1 asks for different
// status codes for them.
func (h *Handler) authorizeModel(w http.ResponseWriter, r *http.Request, model string) bool {
	team := TeamFrom(r.Context())
	if team == nil {
		// Every route this handler serves is mounted behind Auth, so a nil team
		// here means the chain was wired wrong, not that the caller did
		// anything wrong.
		h.log.ErrorContext(r.Context(), "chat completions reached without an authenticated team")
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not resolve the caller's team")
		return false
	}

	if !team.AllowsModel(model) {
		writeError(w, h.log, http.StatusForbidden, "model_not_allowed",
			fmt.Sprintf("team %q is not permitted to use model %q; allowed models: %s",
				team.ID, model, strings.Join(team.AllowedModels, ", ")))
		return false
	}

	return true
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
//
// The named return, actualTokens, is what ChatCompletions reconciles the TPM
// reservation against. It is set by the deferred closure below rather than
// at each individual return, since this function has several exit points and
// every one of them needs the same fallback logic applied.
func (h *Handler) streamChatCompletions(w http.ResponseWriter, r *http.Request, prov provider.Provider, req chatRequest) (actualTokens int) {
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

	defer func() {
		actualTokens = usage.InputTokens + usage.OutputTokens
		if actualTokens == 0 && content.Len() > 0 {
			// A mid-stream failure can end things before the provider's
			// terminal usage-bearing chunk ever arrives, but real generation
			// still happened and still cost something. Reconciling as a full
			// refund in that case would be wrong in the opposite direction
			// from an under-estimate: it would let a team's TPM bucket ignore
			// tokens it actually spent. Approximate from what was actually
			// written instead of trusting an absence of data as a zero.
			actualTokens = req.estimateInputTokens() + (content.Len()+charsPerToken-1)/charsPerToken
		}
	}()

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
		// Only reached on a clean stream completion — every mid-stream
		// failure return above skips this, so a request that never got a
		// terminal usage-bearing chunk is priced at zero rather than guessed
		// from the estimator actualTokens falls back to. That is a known gap
		// (see DECISIONS.md): real generation happened and cost something,
		// but Step 4.1 only has the provider's own usage figures to price
		// from, and this one never arrived.
		h.recordCost(r.Context(), metrics, req.Model, usage)
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
	return
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
