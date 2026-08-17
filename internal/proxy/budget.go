package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

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

// writeBudgetUnavailableError sends a 503 when the budget check itself could
// not run — Redis unreachable, most likely. Budget enforcement fails
// *closed* (see BudgetTracker's implementation for why), so an unverifiable
// spend cap blocks the request rather than letting it through.
func writeBudgetUnavailableError(w http.ResponseWriter, log *slog.Logger) {
	writeError(w, log, http.StatusServiceUnavailable, "budget_check_unavailable",
		"the gateway could not verify this team's budget and is failing closed; try again shortly")
}
