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

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Harshalsharma05/switchyard/internal/budget"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/ratelimit"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// maxRequestBytes bounds an inbound body. A gateway accepting unbounded JSON is
// a memory exhaustion vector, and no legitimate chat request approaches this.
const maxRequestBytes = 1 << 20 // 1 MiB

// Resolver is the slice of the provider registry this handler needs.
//
// It is declared here, by the consumer, rather than taken as a *provider.Registry:
// the handler needs these methods, and depending on the interface rather than
// the concrete type is what lets tests inject a fake and what lets Step 6.2's
// tiers reach the handler from configs/providers.yaml without this package
// importing internal/config.
type Resolver interface {
	ForModel(model string) (provider.Provider, error)

	// DefaultMaxTokensFor returns the resolved instance's configured ceiling
	// for model, the same value substituted when a caller omits max_tokens.
	// Step 3.3's TPM reservation needs it to estimate a request's cost before
	// the provider is ever called.
	DefaultMaxTokensFor(model string) (int, bool)

	// TierFor returns the other candidates in model's fallback tier, in
	// configs/providers.yaml order, or nil for a model that belongs to no
	// tier. It sits on Resolver rather than on an interface of its own
	// because both halves answer the same question — where can this model be
	// served — and one hot-reloadable source (cmd/gateway's configStore)
	// already answers both.
	TierFor(model string) []resilience.Candidate
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

// HealthRecorder is the slice of health.Recorder this package needs: Step
// 5.2's passive signal. The handler calls Record once per real provider call
// — success or failure — so Step 5.3's status computation has live traffic to
// judge a provider by, not just the active checker's periodic pings.
//
// latency is whatever the call site considers this attempt's response time:
// full round trip for a non-streaming completion, time to first byte for a
// stream. err is the exact error Complete or Stream returned, nil on success,
// so Recorder can classify a provider.Error's Kind itself rather than the
// handler pre-deciding what counts as a timeout.
type HealthRecorder interface {
	Record(providerName string, latency time.Duration, err error)
}

// Handler serves the public API.
type Handler struct {
	resolver       Resolver
	limiter        RateLimiter
	budgetTracker  BudgetTracker
	calc           CostCalculator
	healthRecorder HealthRecorder
	health         HealthOracle
	breakers       Breakers
	chaos          *Chaos
	retryConfig    resilience.Config
	promMetrics    *telemetry.Metrics
	log            *slog.Logger
}

// NewHandler wires the handler with its dependencies. Nothing here reads
// package-level state.
//
// health may be nil, and chainFor treats that as "every provider is healthy"
// — the same optimistic default CLAUDE.md mandates for a stale health
// checker, and what keeps every pre-Phase-6 test from having to grow a
// monitor it does not care about.
//
// breakers may be nil too, on the same reasoning: no registry means no
// candidate is ever skipped, which is exactly how the chain behaved before
// Step 7.4. chaos may be nil as well — Chaos.Apply is a no-op on a nil
// receiver, so no harness means no injected faults.
func NewHandler(resolver Resolver, limiter RateLimiter, budgetTracker BudgetTracker, calc CostCalculator, healthRecorder HealthRecorder, health HealthOracle, breakers Breakers, chaos *Chaos, retryConfig resilience.Config, promMetrics *telemetry.Metrics, log *slog.Logger) *Handler {
	return &Handler{
		resolver:       resolver,
		limiter:        limiter,
		budgetTracker:  budgetTracker,
		calc:           calc,
		healthRecorder: healthRecorder,
		health:         health,
		breakers:       breakers,
		chaos:          chaos,
		retryConfig:    retryConfig,
		promMetrics:    promMetrics,
		log:            log,
	}
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

	// Recorded before anything can reject the request, so
	// X-Switchyard-Requested-Model is present on a 403 or a 429 too — the
	// responses where a caller most needs to know which model the gateway
	// thought it was being asked for.
	if m := metricsFrom(r.Context()); m != nil {
		m.requestedModel = req.Model
		m.stream = req.Stream
	}

	if !h.authorizeModel(w, r, req.Model) {
		return
	}

	resolveCtx, resolveSpan := telemetry.Tracer().Start(r.Context(), "switchyard.route.resolve")

	prov, ok := h.resolve(w, r, req.Model)
	if !ok {
		resolveSpan.End()
		return
	}

	requested := resilience.Candidate{Provider: prov.Name(), Model: req.Model}
	chain := h.chainFor(resolveCtx, requested)
	resolveSpan.End()
	if len(chain) == 0 {
		// BuildChain returns nothing only when the team's allowlist forbids
		// every option, the requested model's own provider included — which
		// authorizeModel cannot catch, since it checks allowed_models and this
		// is allowed_providers. Availability never produces an empty chain, so
		// this is always an authorization answer, never a capacity one.
		writeError(w, h.log, http.StatusForbidden, "provider_not_allowed",
			fmt.Sprintf("team %q is not permitted to use any provider that serves model %q",
				teamID(r.Context()), req.Model))
		return
	}

	reservation, ok := h.reserveTokens(w, r, req)
	if !ok {
		return
	}

	rootSpan := trace.SpanFromContext(r.Context())

	var actual int
	defer func() {
		ctx, cancel := context.WithTimeout(trace.ContextWithSpan(context.Background(), rootSpan), reconcileTimeout)
		defer cancel()
		reconcileCtx, reconcileSpan := telemetry.Tracer().Start(ctx, "switchyard.ratelimit.tpm.reconcile")
		if err := reservation.Reconcile(reconcileCtx, actual); err != nil {
			h.log.ErrorContext(r.Context(), "reconciling TPM reservation",
				slog.String("request_id", RequestIDFrom(r.Context())), slog.Any("error", err))
		}
		reconcileSpan.End()
	}()

	budgetReservation, estimatedMicros, ok := h.reserveBudget(w, r, req)
	if !ok {
		// The TPM defer above still fires with actual left at zero — a full
		// refund, correct here since no provider was ever called. Nothing
		// about this reservation itself needs unwinding: reserveBudget only
		// returns a non-nil handle when it succeeded.
		return
	}

	// Step 6.4: the reservation above is sized for the requested model's
	// price, and a fallback can serve a costlier one. chainBudget re-checks
	// the cap for each candidate that costs more, reserving only the
	// difference.
	chainBudget := &chainBudget{h: h, team: TeamFrom(r.Context()), req: req, baseMicros: estimatedMicros}

	defer func() {
		ctx, cancel := context.WithTimeout(trace.ContextWithSpan(context.Background(), rootSpan), reconcileTimeout)
		defer cancel()
		reconcileCtx, reconcileSpan := telemetry.Tracer().Start(ctx, "switchyard.budget.reconcile")
		var actualCostMicros int64
		if m := metricsFrom(r.Context()); m != nil {
			actualCostMicros = m.costMicros
		}
		if err := budgetReservation.Reconcile(reconcileCtx, actualCostMicros); err != nil {
			h.log.ErrorContext(r.Context(), "reconciling budget reservation",
				slog.String("request_id", RequestIDFrom(r.Context())), slog.Any("error", err))
		}
		chainBudget.reconcileExtras(reconcileCtx, h.log)
		reconcileSpan.End()
	}()

	if req.Stream {
		actual = h.streamChatCompletions(w, r, chain, requested, chainBudget, req)
		return
	}

	metrics := metricsFrom(r.Context())

	// totalProviderTime sums every attempt's own measured latency — every
	// failed candidate's wall-clock call time plus the eventual success's
	// resp.Latency — but never the backoff sleep resilience.Do inserts
	// between them. That sleep is SwitchYard's own retry policy running, not
	// time spent waiting on a provider, so it falls where the existing
	// overhead formula (handler time minus providerTime) already puts
	// anything not explicitly counted as provider time: gateway overhead.
	// A fallback's failed attempts are counted the same way, for the same
	// reason: the gateway really did spend that time upstream.
	var totalProviderTime time.Duration

	baseReq := req.toProviderRequest()

	resp, served, failures, err := runChain(r.Context(), h, chain, chainBudget,
		func(ctx context.Context, prov provider.Provider, cand resilience.Candidate) (*provider.Response, error) {
			// The outbound request carries the candidate's model, not the
			// caller's: a fallback still asking for the original model would
			// be rejected by a provider that does not serve it. Copying the
			// struct is enough — Messages is shared, and no attempt mutates it.
			attemptReq := baseReq
			attemptReq.Model = cand.Model

			// Step 7.5: an injected fault replaces the provider call and then
			// travels the identical path a real failure would — recorded to
			// health below, counted by the breaker in runChain, retried and
			// failed over by Phase 6. Injecting here rather than around the
			// closure is what buys that; a harness wrapped outside this would
			// skip the health recording and test a path that does not exist.
			callStart := time.Now()
			var resp *provider.Response
			err := h.chaos.Apply(ctx, cand.Provider, cand.Model)
			if err == nil {
				resp, err = prov.Complete(ctx, attemptReq)
			}
			callElapsed := time.Since(callStart)

			if err != nil {
				// No Response means no measured round trip, so the wall time
				// around the call stands in. On a failure path the adapter's
				// translation work is negligible, so this is close enough not
				// to distort the overhead number.
				totalProviderTime += callElapsed
				h.healthRecorder.Record(cand.Provider, callElapsed, err)
				return nil, err
			}

			// resp.Latency is the HTTP round trip alone, measured inside the
			// adapter. Using it rather than callElapsed means the adapter's
			// own request and response translation counts as gateway
			// overhead — which it is.
			totalProviderTime += resp.Latency
			h.healthRecorder.Record(resp.Provider, resp.Latency, nil)
			return resp, nil
		},
	)

	if err != nil {
		lastTried := lastAttempted(failures)
		if metrics != nil {
			metrics.providerName = lastTried.Provider
			metrics.providerTime = totalProviderTime
		}

		h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider call failed",
			slog.String("request_id", RequestIDFrom(r.Context())),
			slog.String("provider", lastTried.Provider),
			slog.String("model", req.Model),
			slog.Int("candidates_tried", len(failures)),
			slog.Int("attempts", totalAttempts(failures)),
			slog.Any("error", err),
		)
		h.writeChainError(w, req.Model, failures, err)
		return
	}

	if metrics != nil {
		metrics.providerName = resp.Provider
		metrics.servedModel = resp.Model
		metrics.fellBack = served != requested
		metrics.providerTime = totalProviderTime
		metrics.usage = resp.Usage
		h.recordCost(r.Context(), metrics, resp.Model, resp.Usage)
	}
	actual = resp.Usage.InputTokens + resp.Usage.OutputTokens

	_, writeSpan := telemetry.Tracer().Start(r.Context(), "switchyard.response.write")
	h.writeSuccess(w, r, resp)
	writeSpan.End()
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

	spanCtx, span := telemetry.Tracer().Start(r.Context(), "switchyard.ratelimit.tpm")
	checkCtx, cancel := context.WithTimeout(spanCtx, checkTimeout)
	defer cancel()

	reservation, res, err := h.limiter.Reserve(checkCtx, team.ID, ratelimit.TPM, team.RateLimits.TPM, amount, bucketTTL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
	if err != nil {
		// Fail open, same as the RPM middleware: Redis being unreachable must
		// never be the reason a request fails.
		h.log.ErrorContext(r.Context(), "TPM rate limit check failed; failing open",
			slog.String("team", team.ID), slog.Any("error", err))
		return nil, true
	}

	if h.promMetrics != nil {
		h.promMetrics.RatelimitTokensRemaining.WithLabelValues(team.ID, string(ratelimit.TPM)).Set(res.Remaining)
	}

	if !res.Allowed {
		writeRateLimitError(w, h.log, h.promMetrics, team.ID, ratelimit.TPM, team.RateLimits.TPM, res)
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

		writePriorityShedError(w, h.log, h.promMetrics, team.ID, ratelimit.TPM, team.RateLimits.TPM, res)
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
func (h *Handler) reserveBudget(w http.ResponseWriter, r *http.Request, req chatRequest) (*budget.Reservation, int64, bool) {
	team := TeamFrom(r.Context())
	if team == nil {
		h.log.ErrorContext(r.Context(), "chat completions reached without an authenticated team")
		writeError(w, h.log, http.StatusInternalServerError, "internal_error",
			"the gateway could not resolve the caller's team")
		return nil, 0, false
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
		return nil, 0, false
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
		return nil, 0, false
	}

	spanCtx, span := telemetry.Tracer().Start(r.Context(), "switchyard.budget.check")
	checkCtx, cancel := context.WithTimeout(spanCtx, checkTimeout)
	defer cancel()

	reservation, res, err := h.budgetTracker.Reserve(checkCtx, team.ID, team.MonthlyBudgetMicros, estimatedMicros)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
	if err != nil {
		h.log.ErrorContext(r.Context(), "budget check failed; failing closed",
			slog.String("team", team.ID), slog.Any("error", err))
		writeBudgetUnavailableError(w, h.log)
		return nil, 0, false
	}

	if !res.Allowed {
		if h.promMetrics != nil {
			h.promMetrics.BudgetUtilizationRatio.WithLabelValues(team.ID).Set(utilization(res.SpentMicros, team.MonthlyBudgetMicros))
		}
		writeBudgetExceededError(w, h.log, h.promMetrics, team.ID, res.SpentMicros, team.MonthlyBudgetMicros)
		return nil, 0, false
	}

	spentRatio := utilization(res.SpentMicros, team.MonthlyBudgetMicros)
	if h.promMetrics != nil {
		h.promMetrics.BudgetUtilizationRatio.WithLabelValues(team.ID).Set(spentRatio)
	}

	// res.SpentMicros is the total *after* admitting this request, the same
	// "after, not reconstructed to before" reading Step 3.5 already uses for
	// its own threshold check — the request that crosses the 80% line is the
	// one that gets the warning.
	if spentRatio >= budgetWarnThreshold {
		w.Header().Set(HeaderBudgetWarning, "true")
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "team approaching monthly budget",
			slog.String("team", team.ID),
			slog.Int64("spent_micros", res.SpentMicros),
			slog.Int64("cap_micros", team.MonthlyBudgetMicros),
		)
	}

	return reservation, estimatedMicros, true
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
func (h *Handler) streamChatCompletions(w http.ResponseWriter, r *http.Request, chain []resilience.Candidate, requested resilience.Candidate, chainBudget *chainBudget, req chatRequest) (actualTokens int) {
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

	// Passing ctx straight through is what makes a client disconnect cancel
	// the upstream call: net/http cancels a request's Context when the
	// client's connection closes, and that context is what Stream used to
	// build the outbound request. No extra wiring is needed for Step 2.3's
	// "client disconnect cancels the upstream provider request."
	//
	// Retry and fallback both wrap this initial connection attempt and
	// nothing past it. Once Stream succeeds, bytes can start reaching the
	// client at any moment — per Step 2.4/6.3, a mid-stream failure gets an
	// SSE error event and neither a retry nor a fallback, because either
	// would duplicate content the client already received.
	//
	// failedAttemptsTime accumulates only the attempts that errored, across
	// every candidate tried; the eventual successful attempt's own
	// connect-to-first-byte time is measured separately below (callStart,
	// reset to that attempt's start), the same "sum real provider time,
	// exclude backoff sleep" rule the non-streaming path applies.
	var failedAttemptsTime time.Duration
	var callStart time.Time

	baseReq := req.toProviderRequest()

	stream, served, failures, err := runChain(r.Context(), h, chain, chainBudget,
		func(ctx context.Context, prov provider.Provider, cand resilience.Candidate) (provider.StreamReader, error) {
			// Same substitution as the non-streaming path: the outbound
			// request names the candidate's model, not the caller's.
			attemptReq := baseReq
			attemptReq.Model = cand.Model

			// Same injection point as the non-streaming path, for the same
			// reason. It sits before Stream returns, so an injected fault is
			// always a before-the-first-byte failure — which is the only kind
			// Step 6.3 permits a fallback for anyway.
			callStart = time.Now()
			var s provider.StreamReader
			err := h.chaos.Apply(ctx, cand.Provider, cand.Model)
			if err == nil {
				s, err = prov.Stream(ctx, attemptReq)
			}
			if err != nil {
				elapsed := time.Since(callStart)
				failedAttemptsTime += elapsed
				h.healthRecorder.Record(cand.Provider, elapsed, err)
				return nil, err
			}
			return s, nil
		},
	)
	if err != nil {
		lastTried := lastAttempted(failures)
		if metrics != nil {
			metrics.providerName = lastTried.Provider
			metrics.providerTime = failedAttemptsTime
		}

		h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider stream call failed",
			slog.String("request_id", requestID),
			slog.String("provider", lastTried.Provider),
			slog.String("model", req.Model),
			slog.Int("candidates_tried", len(failures)),
			slog.Int("attempts", totalAttempts(failures)),
			slog.Any("error", err),
		)
		// Nothing has reached the client yet, so — per Step 2.4 — this is a
		// normal HTTP error response, exactly like the non-streaming path.
		h.writeChainError(w, req.Model, failures, err)
		return
	}
	defer stream.Close()

	if metrics != nil {
		metrics.providerName = served.Provider
		metrics.servedModel = served.Model
		metrics.fellBack = served != requested
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

	// firstByteLatency is what health.Recorder gets as this attempt's latency,
	// win or lose — kept independent of metrics (which can be nil if Timing
	// did not run) because Step 5.2's passive signal is a functional part of
	// Phase 5, not an observability nicety layered on top of it.
	var firstByteLatency time.Duration

	for {
		chunk, err := stream.Recv()

		// Measured once, at whatever the stream's first Recv() outcome turns
		// out to be — a chunk, a clean EOF, or an error. That makes providerTime
		// a time-to-first-byte figure and, per middleware.go's headerHook
		// comment, the overhead this request reports is a time-to-first-token
		// measurement: the right boundary for a stream, where total duration is
		// dominated by generation, not by the gateway. failedAttemptsTime folds
		// in any retried connection attempts that preceded this successful one.
		if !gotFirstByte {
			firstByteLatency = failedAttemptsTime + time.Since(callStart)
			gotFirstByte = true
			if metrics != nil {
				metrics.providerTime = firstByteLatency
			}
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
				h.healthRecorder.Record(served.Provider, firstByteLatency, err)
				h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider stream failed before any chunk",
					slog.String("request_id", requestID),
					slog.String("provider", served.Provider),
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
			h.healthRecorder.Record(served.Provider, firstByteLatency, err)
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "provider stream failed mid-stream",
				slog.String("request_id", requestID),
				slog.String("provider", served.Provider),
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

		if err := writeSSEJSON(w, toChatChunk(id, created, served.Model, chunk, !sentAny)); err != nil {
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

	// Only reached on a clean stream completion — every mid-stream failure
	// return above already recorded its own outcome and returned before here.
	h.healthRecorder.Record(served.Provider, firstByteLatency, nil)

	if metrics != nil {
		metrics.usage = usage
		// A request that never got a terminal usage-bearing chunk is priced at
		// zero rather than guessed from the estimator actualTokens falls back
		// to. That is a known gap (see DECISIONS.md): real generation happened
		// and cost something, but Step 4.1 only has the provider's own usage
		// figures to price from, and this one never arrived.
		h.recordCost(r.Context(), metrics, served.Model, usage)
	}

	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "writing stream terminator",
			slog.String("request_id", requestID), slog.Any("error", err))
		return
	}
	flusher.Flush()

	h.log.LogAttrs(r.Context(), slog.LevelInfo, "stream completed",
		slog.String("request_id", requestID),
		slog.String("provider", served.Provider),
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
