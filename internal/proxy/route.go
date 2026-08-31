package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
	"github.com/Harshalsharma05/switchyard/internal/router"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

// ComplexityRouter is the slice of internal/router this package needs.
//
// Declared here by the consumer, like Resolver and SemanticCache: the handler
// depends on being told which tier to serve from, not on how a prompt is
// scored.
type ComplexityRouter interface {
	Routes(model string) bool
	Plan(model string, req provider.Request) (router.Plan, bool)
	Policy() router.Policy
}

// errTierUnknown means the policy named a tier providers.yaml does not
// declare — a config mismatch, not anything the caller did.
var errTierUnknown = errors.New("routing tier is not declared in providers.yaml")

// errTierEmpty means the tier exists but the team may use nothing in it.
var errTierEmpty = errors.New("no permitted candidate in routing tier")

// route turns a routing request into a concrete model, rewriting req.Model in
// place. It returns false only when it has already written a response.
//
// It runs before authorizeModel and resolve so that everything downstream —
// the allowlist check, the cache key, both reservations — sees the model that
// will actually be called rather than the keyword the caller sent.
func (h *Handler) route(w http.ResponseWriter, r *http.Request, req *chatRequest) bool {
	// An explicitly named model is honoured exactly. Silently downgrading a
	// request someone made on purpose is a correctness violation, not an
	// optimisation — so this returns before the classifier is ever consulted.
	if h.routing == nil || !h.routing.Routes(req.Model) {
		return true
	}

	ctx, span := telemetry.Tracer().Start(r.Context(), "switchyard.route.classify")
	defer span.End()

	plan, ok := h.routing.Plan(req.Model, req.toProviderRequest())
	if !ok {
		return true
	}

	cand, err := h.selectCandidate(ctx, plan.Tier)
	if err != nil && !plan.Pinned {
		// Only an inferred tier may escalate. A caller who named a tier gets
		// that tier's answer, including its refusal.
		if up, canEscalate := h.routing.Policy().Escalate(plan.Tier); canEscalate {
			if upCand, upErr := h.selectCandidate(ctx, up); upErr == nil {
				cand, err = upCand, nil
				plan.Reason += " escalated=" + up
				plan.Tier = up
			}
		}
	}

	if err != nil {
		if errors.Is(err, errTierUnknown) {
			h.log.ErrorContext(ctx, "routing policy names an undeclared tier",
				slog.String("tier", plan.Tier), slog.Any("error", err))
			writeError(w, h.log, http.StatusInternalServerError, "internal_error",
				"the gateway could not resolve a routing tier")
			return false
		}
		writeError(w, h.log, http.StatusForbidden, "model_not_allowed",
			fmt.Sprintf("team %q is not permitted to use any model in tier %q",
				teamID(ctx), plan.Tier))
		return false
	}

	req.Model = cand.Model

	if m := metricsFrom(r.Context()); m != nil {
		m.routedModel = cand.Model
		m.routedTier = plan.Tier
		m.routeReason = plan.Reason
		m.routeBaselineModel = h.topTierModel()
	}

	span.SetAttributes(
		attribute.String("switchyard.route.tier", plan.Tier),
		attribute.String("switchyard.route.level", plan.Level.String()),
		attribute.String("switchyard.route.model", cand.Model),
		attribute.Bool("switchyard.route.pinned", plan.Pinned),
	)
	h.log.LogAttrs(ctx, slog.LevelDebug, "routed request",
		slog.String("request_id", RequestIDFrom(ctx)),
		slog.String("tier", plan.Tier),
		slog.String("model", cand.Model),
		slog.String("reason", plan.Reason),
	)
	return true
}

// topTierModel is the declared head of the policy's most capable tier: what a
// routed request would have used had it not been routed at all. Empty when the
// tier is unknown, which leaves Step 8.4's savings unrecorded rather than
// priced against a guess.
func (h *Handler) topTierModel() string {
	if h.routing == nil {
		return ""
	}
	top := h.resolver.TierNamed(h.routing.Policy().Complex)
	if len(top) == 0 {
		return ""
	}
	return top[0].Model
}

// selectCandidate picks the model that will serve a routed request from tier.
//
// It reuses buildChain, so routing obeys exactly the rules Part 1's fallback
// already does: the team's allowlist drops candidates outright and is never
// undone, Down providers are removed, and Degraded ones sink below Healthy.
func (h *Handler) selectCandidate(ctx context.Context, tier string) (resilience.Candidate, error) {
	candidates := h.resolver.TierNamed(tier)
	if len(candidates) == 0 {
		return resilience.Candidate{}, fmt.Errorf("%w: %q", errTierUnknown, tier)
	}

	// An empty Requested is skipped by BuildChain's own add(), so the tier's
	// declared order survives as the chain. That is this step's intra-tier
	// rule: providers.yaml's order is preference order, and routing and
	// fallback must not disagree about it.
	chain := h.buildChain(ctx, resilience.Candidate{}, candidates)
	if len(chain) == 0 {
		return resilience.Candidate{}, fmt.Errorf("%w: %q", errTierEmpty, tier)
	}

	// An open breaker removes a model from candidacy. State() rather than
	// Allow(): Allow claims the single half-open probe slot, and spending a
	// probe on a routing decision would steal it from the call that follows.
	for _, c := range chain {
		if b := h.breakerFor(c); b != nil && b.State() == resilience.StateOpen {
			continue
		}
		return c, nil
	}

	// Every candidate's breaker is open. Returning the head anyway matches
	// BuildChain's own rule 4 — health is a signal, not a verdict — and
	// runChain still gates the call itself.
	return chain[0], nil
}
