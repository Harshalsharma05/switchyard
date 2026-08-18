package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
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
			logFallback(ctx, h.log, chain[0], cand, costDelta, failures)
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

		value, attemptErr, attempts := resilience.Do(ctx, cfg, h.log,
			resilience.Labels{Provider: cand.Provider, Model: cand.Model},
			func(ctx context.Context, _ int) (T, error) {
				return call(ctx, prov, cand)
			},
		)
		spent += attempts
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
func logFallback(ctx context.Context, log *slog.Logger, requested, next resilience.Candidate, costDelta int64, failures []chainFailure) {
	prev := failures[len(failures)-1]

	log.LogAttrs(ctx, slog.LevelWarn, "falling back to next candidate",
		slog.String("request_id", RequestIDFrom(ctx)),
		slog.String("requested_model", requested.Model),
		slog.String("from_provider", prev.candidate.Provider),
		slog.String("from_model", prev.candidate.Model),
		slog.String("to_provider", next.Provider),
		slog.String("to_model", next.Model),
		slog.Int("attempts", prev.attempts),
		slog.String("reason", failureKind(prev.err)),
		slog.Int64("estimated_cost_delta_micros", costDelta),
	)
}

// failureKind names a failure by its provider.Error kind, so the fallback log
// line says "rate_limited" rather than repeating a full error string that is
// already logged by the retry loop.
func failureKind(err error) string {
	var provErr *provider.Error
	if errors.As(err, &provErr) {
		return string(provErr.Kind)
	}
	return "unknown"
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
		writeBudgetExceededError(w, h.log, denied.teamID, denied.spentMicros, denied.capMicros)
		return
	}

	if len(failures) > 1 && resilience.ShouldFallback(err) {
		writeChainExhaustedError(w, h.log, requestedModel, failures)
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
