package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/budget"
)

// HeaderBudgetWarning is set on an otherwise-successful response once a
// team's spend for the current period has crossed budgetWarnThreshold. It
// carries no numeric value — like the rate-limit headers, "true" is the
// entire signal; the exact spend and cap are in the accompanying log line
// until Phase 9's metrics exist.
const HeaderBudgetWarning = "X-Switchyard-Budget-Warning"

// budgetWarnThreshold is Step 4.2's 80% line. Unlike the hard cap enforced
// inside budget.Tracker itself, the warning threshold is a response-shaping
// decision, not a spend-tracking one, so it lives here rather than in
// internal/budget — the same split Step 3.5 draws between ratelimit's raw
// bucket state and proxy's own batchShedFloor.
const budgetWarnThreshold = 0.8

// BudgetTracker is the slice of budget.Tracker this package needs.
//
// Declared here, by the consumer, for the same reason Resolver, RateLimiter,
// and CostCalculator are: the handler depends on reserving and reconciling
// against a team's spend, not on how that spend is stored or enforced
// atomically.
type BudgetTracker interface {
	Reserve(ctx context.Context, teamID string, capMicros, estimatedMicros int64) (*budget.Reservation, budget.Result, error)
}

// utilization reports spent as a fraction of cap, used only for the Step 4.2
// warning line — the hard 100% cap decision itself is budget.Tracker's own
// job, made atomically inside Reserve.
func utilization(spentMicros, capMicros int64) float64 {
	if capMicros <= 0 {
		return 0
	}
	return float64(spentMicros) / float64(capMicros)
}

// formatUSD renders integer micro-dollars as a human-readable dollar amount,
// for the one place this project puts a cost in front of a person rather
// than a log line or a Prometheus label: the 402 body a caller reads when
// their team is blocked.
func formatUSD(micros int64) string {
	return fmt.Sprintf("$%.2f", float64(micros)/1_000_000)
}

// writeBudgetExceededError sends a 402 for a request denied because it would
// push the team's spend for the current period over its monthly cap.
func writeBudgetExceededError(w http.ResponseWriter, log *slog.Logger, teamID string, spentMicros, capMicros int64) {
	writeError(w, log, http.StatusPaymentRequired, "budget_exceeded",
		fmt.Sprintf("team %q has spent %s of its %s monthly budget; this request was not made",
			teamID, formatUSD(spentMicros), formatUSD(capMicros)))
}

// budgetDeniedError reports that a fallback candidate was refused because its
// price would push the team past its monthly cap. It is a typed error rather
// than a sentinel because the 402 body quotes the spend and the cap, and
// those numbers are only known at the moment of the denial.
type budgetDeniedError struct {
	teamID      string
	model       string
	spentMicros int64
	capMicros   int64
}

func (e *budgetDeniedError) Error() string {
	return fmt.Sprintf("team %s cannot afford model %s: %s spent of %s cap",
		e.teamID, e.model, formatUSD(e.spentMicros), formatUSD(e.capMicros))
}

// chainBudget is Step 6.4's re-check: a fallback can move a request to a
// costlier model than the one the caller asked for, and the reservation taken
// before the chain started was sized for that original model's price.
//
// It holds the extra reservations rather than folding them into the original
// because budget.Reservation settles against what it itself reserved. The
// caller reconciles the base reservation with the request's real cost and
// each top-up with zero, which nets out to exactly the actual spend — see
// ChatCompletions' deferred settlement.
//
// A nil *chainBudget admits every candidate without checking anything, which
// is what the streaming and non-streaming paths share when no budget tracker
// is in play.
type chainBudget struct {
	h    *Handler
	team *auth.Team
	req  chatRequest

	// baseMicros is what the request already has reserved: the estimate for
	// the requested model. Only the amount a candidate exceeds this by needs
	// reserving again.
	baseMicros int64

	// extra holds every top-up taken while walking the chain. Appended on the
	// request's own goroutine, like everything else in the handler, so it
	// needs no lock.
	extra []*budget.Reservation
}

// estimateFor prices a candidate's model at the same ceiling Step 4.2 uses
// for the requested one: estimated prompt tokens plus the most the response
// is allowed to be.
func (cb *chainBudget) estimateFor(model string) (int64, error) {
	defaultMaxTokens, ok := cb.h.resolver.DefaultMaxTokensFor(model)
	if !ok {
		return 0, fmt.Errorf("no default max tokens configured for model %q", model)
	}
	return cb.h.calc.Cost(model, cb.req.estimateInputTokens(), cb.req.estimateOutputCeiling(defaultMaxTokens))
}

// admit re-checks the team's cap against a candidate's price and reports the
// estimated cost delta against the requested model — the number Step 6.4 asks
// to see logged on every fallback event, positive for a costlier fallback and
// negative for a cheaper one.
//
// A cheaper or equally priced candidate needs no second check: the existing
// reservation already covers it, and the difference comes back at settlement.
// A costlier one reserves only the difference, so a fallback never
// double-charges the part that was already held.
//
// An error means "do not use this candidate." The caller records it and moves
// on to the next, which is usually a cheaper option further down the tier —
// better than failing a request outright over a top-up the team could not
// afford. Budget enforcement fails closed here exactly as it does at Step
// 4.2: a pricing lookup that cannot be made is a candidate that cannot be
// used.
func (cb *chainBudget) admit(ctx context.Context, model string) (deltaMicros int64, err error) {
	if cb == nil || model == cb.req.Model {
		return 0, nil
	}

	estimate, err := cb.estimateFor(model)
	if err != nil {
		return 0, fmt.Errorf("pricing fallback candidate %q: %w", model, err)
	}

	delta := estimate - cb.baseMicros
	if delta <= 0 {
		return delta, nil
	}

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	reservation, res, err := cb.h.budgetTracker.Reserve(checkCtx, cb.team.ID, cb.team.MonthlyBudgetMicros, delta)
	if err != nil {
		return delta, fmt.Errorf("re-checking budget for fallback candidate %q: %w", model, err)
	}
	if !res.Allowed {
		return delta, &budgetDeniedError{
			teamID:      cb.team.ID,
			model:       model,
			spentMicros: res.SpentMicros,
			capMicros:   cb.team.MonthlyBudgetMicros,
		}
	}

	cb.extra = append(cb.extra, reservation)
	return delta, nil
}

// reconcileExtras gives back every top-up taken during the walk. Each settles
// against zero because the request's whole real cost is reconciled through
// the base reservation instead — splitting the actual across several handles
// would mean deciding which one "owns" the spend, and there is no meaningful
// answer to that.
func (cb *chainBudget) reconcileExtras(ctx context.Context, log *slog.Logger) {
	if cb == nil {
		return
	}
	for _, reservation := range cb.extra {
		if err := reservation.Reconcile(ctx, 0); err != nil {
			log.ErrorContext(ctx, "reconciling fallback budget top-up",
				slog.String("team", cb.team.ID), slog.Any("error", err))
		}
	}
}

// writeBudgetUnavailableError sends a 503 when the budget check itself could
// not run — Redis unreachable, most likely. Budget enforcement fails
// *closed* (see BudgetTracker's implementation for why), so an unverifiable
// spend cap blocks the request rather than letting it through.
func writeBudgetUnavailableError(w http.ResponseWriter, log *slog.Logger) {
	writeError(w, log, http.StatusServiceUnavailable, "budget_check_unavailable",
		"the gateway could not verify this team's budget and is failing closed; try again shortly")
}
