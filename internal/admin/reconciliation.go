// Budget reconciliation (Part 2, Step 6.1). GET /admin/reconciliation checks
// every team's live Redis spend counter against the independent sum of its
// logged request costs for the current month, so a drift between the two is
// surfaced instead of hidden. Admin-only, mounted behind requireAdmin.
package admin

import (
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// defaultReconcileToleranceMicros is how far Redis and the request log may
// diverge before a team is flagged. An exact match is unrealistic: a request
// can be reserved in Redis but not yet written to the log, and estimate-then-
// reconcile rounding leaves sub-cent noise. $0.01 is wide enough to swallow
// that and narrow enough to catch a real accounting bug.
const defaultReconcileToleranceMicros = 10_000

type reconcileTeamView struct {
	TeamID          string   `json:"team_id"`
	RedisMicros     *int64   `json:"redis_micros"`
	RedisUSD        *float64 `json:"redis_usd"`
	LogMicros       int64    `json:"log_micros"`
	LogUSD          float64  `json:"log_usd"`
	DeltaMicros     *int64   `json:"delta_micros"`
	DeltaUSD        *float64 `json:"delta_usd"`
	WithinTolerance *bool    `json:"within_tolerance"`
}

type reconcileView struct {
	Period          string              `json:"period"`
	GeneratedAt     string              `json:"generated_at"`
	ToleranceMicros int64               `json:"tolerance_micros"`
	Degraded        bool                `json:"degraded"`
	Reconciled      bool                `json:"reconciled"`
	Teams           []reconcileTeamView `json:"teams"`
}

// handleReconciliation serves GET /admin/reconciliation.
func handleReconciliation(teams TeamStore, spend SpendReader, reqLog RequestLogReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reqLog == nil {
			writeRequestLogDisabled(w, log)
			return
		}

		now := time.Now().UTC()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

		logByTeam, err := reqLog.SpendByTeamSince(r.Context(), monthStart)
		if err != nil {
			log.ErrorContext(r.Context(), "reconciliation: summing request-log spend", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the request log")
			return
		}

		view := reconcileView{
			Period:          now.Format("2006-01"),
			GeneratedAt:     now.Format(time.RFC3339Nano),
			ToleranceMicros: defaultReconcileToleranceMicros,
			Reconciled:      true,
			Teams:           []reconcileTeamView{},
		}

		for _, t := range teams.List() {
			logMicros := logByTeam[t.ID]
			tv := reconcileTeamView{TeamID: t.ID, LogMicros: logMicros, LogUSD: microsToUSD(logMicros)}

			redisMicros, err := spend.Spent(r.Context(), t.ID)
			if err != nil {
				// One team's Redis read failing degrades the report rather than
				// failing it — the other teams' numbers are still worth showing.
				log.ErrorContext(r.Context(), "reconciliation: reading Redis spend",
					slog.String("team", t.ID), slog.Any("error", err))
				view.Degraded = true
				view.Reconciled = false
			} else {
				redisUSD := microsToUSD(redisMicros)
				delta := redisMicros - logMicros
				deltaUSD := microsToUSD(delta)
				within := delta <= defaultReconcileToleranceMicros && delta >= -defaultReconcileToleranceMicros
				tv.RedisMicros, tv.RedisUSD = &redisMicros, &redisUSD
				tv.DeltaMicros, tv.DeltaUSD = &delta, &deltaUSD
				tv.WithinTolerance = &within
				if !within {
					view.Reconciled = false
				}
			}
			view.Teams = append(view.Teams, tv)
		}

		sort.Slice(view.Teams, func(i, j int) bool { return view.Teams[i].TeamID < view.Teams[j].TeamID })
		writeJSON(w, log, http.StatusOK, view)
	}
}
