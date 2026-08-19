package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// HealthOracle is the slice of health.Monitor this package needs for Step
// 6.2's ordering: one read of a provider's current status, no I/O.
//
// Declared here by the consumer, like Resolver and CostCalculator, and kept
// separate from HealthRecorder even though health.Monitor and health.Recorder
// are related: recording is a write on the request path, this is a read, and
// a test that fakes one rarely wants to fake the other.
type HealthOracle interface {
	Status(providerName string) health.Status
}

// Breakers is the slice of resilience.BreakerRegistry this package needs:
// look up the breaker guarding one provider+model. Declared by the consumer,
// like Resolver and HealthOracle above.
//
// It hands back the concrete *resilience.Breaker rather than an interface of
// its own three methods, because a Go interface returning an interface would
// not be satisfied structurally by the registry's concrete return type. Tests
// build real Breakers over fake stores, which is the better fake anyway —
// the state machine under test is the one that ships.
type Breakers interface {
	For(labels resilience.Labels) *resilience.Breaker
}

// errBreakerOpen marks a candidate that was skipped without being called.
//
// It is a plain sentinel rather than a *provider.Error because no provider
// was involved: inventing a provider failure here would put a fabricated
// upstream status into the chain breakdown and into Phase 5's passive health
// window, both of which are supposed to record what providers actually did.
var errBreakerOpen = errors.New("circuit breaker is open for this provider and model")

// chainFailure is one candidate's outcome after its retries were exhausted.
// Step 6.3 turns a slice of these into the 503 body that says what was tried
// and why each attempt failed; Step 6.2 only needs the last error to report.
type chainFailure struct {
	candidate resilience.Candidate
	attempts  int
	err       error
}

// chainFor builds the ordered candidate list for a request whose primary
// provider has already resolved.
//
// The team comes from the request context rather than a parameter because
// every caller is downstream of Auth, and threading it through would let a
// future call site forget it — the allowlist is the one input here that must
// never be silently absent.
func (h *Handler) chainFor(ctx context.Context, requested resilience.Candidate) []resilience.Candidate {
	var allowed func(providerName, model string) bool
	if team := TeamFrom(ctx); team != nil {
		// Both halves, not just the model: a team's allowed_providers is what
		// Step 6.2's compliance rule is written against, and a fallback can
		// move a request to a provider the team never listed even when the
		// model itself is fine.
		allowed = func(providerName, model string) bool {
			return team.AllowsProvider(providerName) && team.AllowsModel(model)
		}
	}

	var statusOf func(string) health.Status
	if h.health != nil {
		statusOf = h.health.Status
	}

	return resilience.BuildChain(resilience.ChainInput{
		Requested: requested,
		Tier:      h.resolver.TierFor(requested.Model),
		StatusOf:  statusOf,
		Allowed:   allowed,
	})
}

// runChain walks chain in order, running call under Step 6.1's retry policy
// for each candidate and moving to the next only once that candidate's
// retries are exhausted. That is Step 6.3's ordering rule — retry the primary
// first, then fall back — expressed as the nesting of the two loops rather
// than as a flag.
//
// It is a plain function rather than a method because Go does not allow type
// parameters on methods, and the two call sites need different return types:
// a *provider.Response for the non-streaming path, a provider.StreamReader
// for the streaming one. call keeps its own timing and health-recording
// closure, since what counts as "provider time" differs between them.
//
// Each attempt derives its own timeout from ctx inside the adapter (see
// provider/http.go), so a candidate that times out leaves ctx itself alive
// for the next one — the "fallback needs a fresh context" trap does not
// apply here. A cancelled ctx does stop the walk: that means the client is
// gone or the caller's deadline has passed, and trying another provider on
// its behalf would only cost the gateway a request nobody is waiting for.
func runChain[T any](
	ctx context.Context,
	h *Handler,
	chain []resilience.Candidate,
	cb *chainBudget,
	call func(ctx context.Context, prov provider.Provider, cand resilience.Candidate) (T, error),
) (result T, served resilience.Candidate, failures []chainFailure, err error) {
	failures = make([]chainFailure, 0, len(chain))

	// spent is Step 6.3's chain-wide attempt budget in flight. It bounds the
	// whole request rather than each candidate, so a long tier cannot
	// multiply the retry policy into a load spike against providers that are
	// already struggling.
	spent := 0

	for i, cand := range chain {
		remaining := h.retryConfig.MaxTotalAttempts - spent
		if remaining <= 0 {
			break
		}

		if i > 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				break
			}
		}

		// Step 7.4: an open breaker skips this candidate outright — before the
		// budget re-check, before resolution, and above all before any
		// provider call. That ordering is the point of the bullet: the win is
		// not just avoiding a request that would fail, it is avoiding the
		// timeout the caller would otherwise wait out first. A dead provider
		// costs the full request timeout per attempt; an open breaker costs a
		// cached state read.
		//
		// A claimed HalfOpen probe is admitted here and reported below, once,
		// after resilience.Do has finished with this candidate. Note that Do
		// may retry inside a single probe, so one probe can be more than one
		// upstream call — bounded by MaxAttempts and still gated by the single
		// probe slot, but worth knowing when reading a recovering provider's
		// access log.
		breaker := h.breakerFor(cand)
		if breaker != nil {
			breakerCtx, breakerSpan := telemetry.Tracer().Start(ctx, "switchyard.breaker.check")
			beforeState := breaker.State()
			allowed := breaker.Allow(breakerCtx)
			h.recordBreakerState(cand, beforeState, breaker.State())
			breakerSpan.SetAttributes(
				semconv.GenAIProviderNameKey.String(cand.Provider),
				semconv.GenAIRequestModel(cand.Model),
				attribute.Bool("switchyard.breaker.allowed", allowed),
			)
			breakerSpan.End()
			if !allowed {
				h.log.LogAttrs(ctx, slog.LevelInfo, "skipping candidate on open circuit breaker",
					slog.String("request_id", RequestIDFrom(ctx)),
					slog.String("provider", cand.Provider),
					slog.String("model", cand.Model),
				)
				failures = append(failures, chainFailure{candidate: cand, err: errBreakerOpen})
				continue
			}
		}

		// Step 6.4's re-check, before any call is made: a candidate the team
		// cannot afford is recorded and skipped, not attempted. The next
		// entry is often a cheaper one, so a denial here is a reason to keep
		// walking rather than to fail the request.
		costDelta, budgetErr := cb.admit(ctx, cand.Model)
		if budgetErr != nil {
			h.log.LogAttrs(ctx, slog.LevelWarn, "skipping fallback candidate on budget",
				slog.String("request_id", RequestIDFrom(ctx)),
				slog.String("provider", cand.Provider),
				slog.String("model", cand.Model),
				slog.Int64("estimated_cost_delta_micros", costDelta),
				slog.Any("error", budgetErr),
			)
			failures = append(failures, chainFailure{candidate: cand, err: budgetErr})
			continue
		}

		if i > 0 {
			logFallback(ctx, h.log, h.promMetrics, chain[0], cand, costDelta, failures)
		}

		prov, resolveErr := h.resolver.ForModel(cand.Model)
		if resolveErr != nil {
			// Config validation makes this unreachable: every tier entry names
			// a model some enabled provider serves. Recorded rather than
			// ignored so an unreachable case that becomes reachable shows up
			// in the failure breakdown instead of silently shortening the chain.
			h.log.ErrorContext(ctx, "resolving fallback candidate",
				slog.String("provider", cand.Provider),
				slog.String("model", cand.Model),
				slog.Any("error", resolveErr))
			failures = append(failures, chainFailure{candidate: cand, err: resolveErr})
			continue
		}

		// The per-candidate cap is narrowed to whatever the chain-wide budget
		// has left. The primary therefore keeps its full retry allowance and
		// later candidates spend what remains — see Config.MaxTotalAttempts
		// for why the budget goes to the option most likely to work.
		cfg := h.retryConfig
		if remaining < cfg.MaxAttempts {
			cfg.MaxAttempts = remaining
		}

		isFallback := i > 0
		value, attemptErr, attempts := resilience.Do(ctx, cfg, h.log, h.promMetrics,
			resilience.Labels{Provider: cand.Provider, Model: cand.Model},
			func(attemptCtx context.Context, n int) (T, error) {
				spanCtx, span := telemetry.Tracer().Start(attemptCtx, "switchyard.provider.call")
				span.SetAttributes(
					semconv.GenAIProviderNameKey.String(cand.Provider),
					semconv.GenAIRequestModel(cand.Model),
					attribute.Int("switchyard.attempt", n),
					attribute.Bool("fallback", isFallback),
				)

				if h.promMetrics != nil {
					h.promMetrics.InflightRequests.WithLabelValues(cand.Provider).Inc()
					defer h.promMetrics.InflightRequests.WithLabelValues(cand.Provider).Dec()
				}

				result, err := call(spanCtx, prov, cand)

				if err != nil {
					span.RecordError(err)
					span.SetStatus(codes.Error, err.Error())
					kind := failureKind(err)
					span.SetAttributes(attribute.String("switchyard.error.kind", kind))
					if h.promMetrics != nil {
						h.promMetrics.ErrorsTotal.WithLabelValues(teamID(ctx), cand.Provider, cand.Model, kind).Inc()
					}
				} else if resp, ok := any(result).(*provider.Response); ok {
					span.SetAttributes(
						semconv.GenAIUsageInputTokens(resp.Usage.InputTokens),
						semconv.GenAIUsageOutputTokens(resp.Usage.OutputTokens),
					)
				}

				span.End()
				return result, err
			},
		)
		spent += attempts

		// The breaker hears one verdict per candidate, not one per retry.
		// Reporting each retry separately would let a single client request
		// contribute a whole threshold's worth of failures on its own, and in
		// HalfOpen it would report several outcomes for a probe that was
		// claimed once.
		if breaker != nil {
			beforeState := breaker.State()
			if attemptErr == nil {
				breaker.RecordSuccess(ctx)
			} else {
				breaker.RecordFailure(ctx)
			}
			h.recordBreakerState(cand, beforeState, breaker.State())
		}

		if attemptErr == nil {
			return value, cand, failures, nil
		}

		failures = append(failures, chainFailure{candidate: cand, attempts: attempts, err: attemptErr})

		// A failure the request itself caused fails the same way everywhere,
		// so the walk stops here and the caller gets that error directly
		// rather than a chain-exhausted one several round trips later.
		if !resilience.ShouldFallback(attemptErr) {
			break
		}
	}

	var zero T
	return zero, resilience.Candidate{}, failures, lastChainError(failures)
}

// logFallback records the moment a request leaves one candidate for the next,
// naming why the previous one was abandoned. This is the line that makes a
// fallback visible in the logs without reading the response headers.
//
// costDelta is Step 6.4's requirement: what this fallback is estimated to
// cost against the requested model, in micro-dollars — positive when the
// chain has moved to something pricier, negative when it has moved down to a
// cheaper option. It is an estimate rather than the real figure because the
// real one does not exist yet; the actual cost is priced from the provider's
// own usage report once the response lands.
func logFallback(ctx context.Context, log *slog.Logger, metrics *telemetry.Metrics, requested, next resilience.Candidate, costDelta int64, failures []chainFailure) {
	prev := failures[len(failures)-1]
	reason := failureKind(prev.err)

	log.LogAttrs(ctx, slog.LevelWarn, "falling back to next candidate",
		slog.String("request_id", RequestIDFrom(ctx)),
		slog.String("requested_model", requested.Model),
		slog.String("from_provider", prev.candidate.Provider),
		slog.String("from_model", prev.candidate.Model),
		slog.String("to_provider", next.Provider),
		slog.String("to_model", next.Model),
		slog.Int("attempts", prev.attempts),
		slog.String("reason", reason),
		slog.Int64("estimated_cost_delta_micros", costDelta),
	)

	if metrics != nil {
		metrics.FallbacksTotal.WithLabelValues(prev.candidate.Provider, next.Provider, reason).Inc()
	}
}

// failureKind names a failure by its provider.Error kind, so the fallback log
// line says "rate_limited" rather than repeating a full error string that is
// already logged by the retry loop.
func failureKind(err error) string {
	if errors.Is(err, errBreakerOpen) {
		return "circuit_breaker_open"
	}
	var provErr *provider.Error
	if errors.As(err, &provErr) {
		return string(provErr.Kind)
	}
	return "unknown"
}

// breakerFor returns the breaker guarding one candidate, or nil when this
// handler was built without a registry — the same optional-dependency shape
// h.health uses, so tests predating Phase 7 need not grow one.
func (h *Handler) breakerFor(cand resilience.Candidate) *resilience.Breaker {
	if h.breakers == nil {
		return nil
	}
	return h.breakers.For(resilience.Labels{Provider: cand.Provider, Model: cand.Model})
}

func (h *Handler) recordBreakerState(cand resilience.Candidate, before, after resilience.State) {
	if h.promMetrics == nil {
		return
	}
	h.promMetrics.BreakerState.WithLabelValues(cand.Provider, cand.Model).Set(breakerStateValue(after))
	if before != after {
		h.promMetrics.BreakerTransitionsTotal.WithLabelValues(cand.Provider, cand.Model, before.String(), after.String()).Inc()
	}
}

func breakerStateValue(s resilience.State) float64 {
	switch s {
	case resilience.StateClosed:
		return 0
	case resilience.StateHalfOpen:
		return 1
	case resilience.StateOpen:
		return 2
	default:
		return -1
	}
}

// writeChainError picks how a failed chain is reported.
//
// Three cases, and the distinction matters to whoever reads the response:
//
//   - The walk stopped on a failure the request itself caused (a malformed
//     body, a content policy refusal). Nothing was exhausted — the gateway
//     chose to stop — so the caller gets that provider's own mapped status,
//     which is the 400 they need to act on.
//   - One candidate, no alternatives. There is no breakdown to give, and the
//     provider's own status (429, 504, 502) says more than a flat 503 would.
//   - Several candidates all failed. That is Step 6.3's exhausted chain, and
//     it gets the 503 with the per-candidate breakdown.
func (h *Handler) writeChainError(w http.ResponseWriter, requestedModel string, failures []chainFailure, err error) {
	// Checked ahead of everything else: a chain that ran out of *affordable*
	// options is a spend problem, and a 503 would tell the caller to retry
	// against a cap that will still be there next time. Step 4.2's 402 is the
	// honest answer.
	var denied *budgetDeniedError
	if errors.As(err, &denied) {
		writeBudgetExceededError(w, h.log, h.promMetrics, denied.teamID, denied.spentMicros, denied.capMicros)
		return
	}

	if len(failures) > 1 && resilience.ShouldFallback(err) {
		writeChainExhaustedError(w, h.log, requestedModel, failures)
		return
	}

	// One candidate, and its breaker was open. There is no upstream status to
	// report because nothing upstream was contacted, and writeProviderError's
	// fallback for an unclassified error is a 500 — which would blame the
	// gateway for an internal fault when this is in fact the gateway working
	// exactly as designed. 503 is the honest answer: no capacity right now,
	// try later.
	if errors.Is(err, errBreakerOpen) {
		writeError(w, h.log, http.StatusServiceUnavailable, "circuit_breaker_open",
			fmt.Sprintf("the circuit breaker for model %s is open after repeated upstream failures; no request was sent",
				strconv.Quote(requestedModel)))
		return
	}

	writeProviderError(w, h.log, err)
}

// lastAttempted names the candidate whose failure the client is about to be
// told about — the last one tried. The zero Candidate is returned for an
// empty slice, which the caller renders as an empty provider name rather than
// inventing one.
func lastAttempted(failures []chainFailure) resilience.Candidate {
	if len(failures) == 0 {
		return resilience.Candidate{}
	}
	return failures[len(failures)-1].candidate
}

// totalAttempts sums the provider calls made across every candidate, which is
// the number CLAUDE.md's retry-amplification warning is about: retries
// multiplied by chain length, not per-layer.
func totalAttempts(failures []chainFailure) int {
	n := 0
	for _, f := range failures {
		n += f.attempts
	}
	return n
}

// teamID reads the authenticated team's ID for an error message. Every caller
// runs behind Auth, so the fallback string only guards against a wiring
// mistake.
func teamID(ctx context.Context) string {
	if team := TeamFrom(ctx); team != nil {
		return team.ID
	}
	return "unknown"
}

// lastChainError returns the final candidate's error, which is what the
// client sees when the whole chain fails. Step 6.3 replaces this with a 503
// carrying every entry in failures; until then the last real provider error
// is a more useful answer than a synthesized one, because it still maps to a
// meaningful status through writeProviderError.
func lastChainError(failures []chainFailure) error {
	if len(failures) == 0 {
		return errors.New("fallback chain was empty")
	}
	return failures[len(failures)-1].err
}
