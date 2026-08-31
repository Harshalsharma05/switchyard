// Overview's data endpoint (Part 2, Step 2.3). GET /admin/summary queries
// Prometheus server-side so the frontend never has to, and merges in live
// provider health. Any valid team key reaches it; a non-admin's numbers are
// scoped to its own team.
package admin

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/summary"
)

// SummaryService is the slice of summary.Service this package needs.
type SummaryService interface {
	Build(ctx context.Context, opts summary.Options) summary.Result
}

const defaultSummaryRange = "24h"

// --- wire shape --------------------------------------------------------

type summaryView struct {
	Range       string                `json:"range"`
	GeneratedAt string                `json:"generated_at"`
	Degraded    bool                  `json:"degraded"`
	Requests    summaryRequestsView   `json:"requests"`
	OverheadMS  summaryOverheadView   `json:"overhead_ms"`
	Cost        summaryCostView       `json:"cost"`
	Cache       summaryCacheView      `json:"cache"`
	Providers   []summaryProviderView `json:"providers"`
	Series      *summarySeriesView    `json:"series"`
}

// summarySeriesView backs Overview's traffic and overhead charts. Null when
// Prometheus could not answer the range queries — the charts show an empty
// state and the KPIs still render.
type summarySeriesView struct {
	StepSeconds int                    `json:"step_seconds"`
	Traffic     []summaryTrafficPoint  `json:"traffic"`
	Overhead    []summaryOverheadPoint `json:"overhead"`
}

type summaryTrafficPoint struct {
	T     string  `json:"t"`
	Value float64 `json:"value"`
}

type summaryOverheadPoint struct {
	T   string   `json:"t"`
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
	P99 *float64 `json:"p99"`
}

type summaryRequestsView struct {
	Total     *float64 `json:"total"`
	ErrorRate *float64 `json:"error_rate"`
}

type summaryOverheadView struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
	P99 *float64 `json:"p99"`
}

type summaryCostView struct {
	TotalUSD *float64 `json:"total_usd"`
}

// summaryCacheView carries Overview's cache KPI.
//
// Enabled is a config fact — whether this gateway has a cache at all — and is
// separate from HitRate, which stays null when Prometheus has no data yet. The
// two together are what let Overview distinguish "no cache here" from "a cache
// that has not been asked anything".
type summaryCacheView struct {
	Enabled bool     `json:"enabled"`
	HitRate *float64 `json:"hit_rate"`
}

type summaryProviderView struct {
	Provider         string  `json:"provider"`
	Status           string  `json:"status"`
	ErrorRate        float64 `json:"error_rate"`
	P99LatencyMillis float64 `json:"p99_latency_ms"`
}

func toSummaryView(res summary.Result, health HealthReader, cacheEnabled bool) summaryView {
	v := summaryView{
		Range:       res.Range,
		GeneratedAt: res.GeneratedAt.Format(time.RFC3339Nano),
		Degraded:    res.Degraded,
		Requests:    summaryRequestsView{Total: res.RequestCount, ErrorRate: res.ErrorRate},
		OverheadMS:  summaryOverheadView{P50: res.OverheadP50, P95: res.OverheadP95, P99: res.OverheadP99},
		Cost:        summaryCostView{TotalUSD: res.CostUSD},
		Cache:       summaryCacheView{Enabled: cacheEnabled, HitRate: res.CacheHitRate},
		Providers:   []summaryProviderView{},
	}
	if health != nil {
		for _, s := range health.Snapshots() {
			v.Providers = append(v.Providers, summaryProviderView{
				Provider:         s.Provider,
				Status:           s.Status.String(),
				ErrorRate:        s.ErrorRate,
				P99LatencyMillis: float64(s.P99Latency) / float64(time.Millisecond),
			})
		}
	}

	if res.Series != nil {
		sv := &summarySeriesView{
			StepSeconds: res.Series.StepSeconds,
			Traffic:     make([]summaryTrafficPoint, 0, len(res.Series.Traffic)),
			Overhead:    make([]summaryOverheadPoint, 0, len(res.Series.Overhead)),
		}
		for _, p := range res.Series.Traffic {
			sv.Traffic = append(sv.Traffic, summaryTrafficPoint{T: p.T.Format(time.RFC3339), Value: p.Value})
		}
		for _, p := range res.Series.Overhead {
			sv.Overhead = append(sv.Overhead, summaryOverheadPoint{
				T: p.T.Format(time.RFC3339), P50: p.P50, P95: p.P95, P99: p.P99,
			})
		}
		v.Series = sv
	}
	return v
}

// --- handler ----------------------------------------------------------

func handleSummary(svc SummaryService, health HealthReader, cacheEnabled bool, authr KeyAuthenticator, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, log, http.StatusServiceUnavailable, "summary_disabled",
				"the summary endpoint is not configured")
			return
		}

		team, ok := authenticate(w, r, authr, log)
		if !ok {
			return
		}

		rng := r.URL.Query().Get("range")
		if rng == "" {
			rng = defaultSummaryRange
		}
		if !summary.ValidRange(rng) {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"range must be one of 1h, 24h, 7d")
			return
		}

		// Scope is the caller's own team unless it is an admin, which may pass
		// ?team= to look across teams or at one other team. A non-admin naming
		// another team is refused, not silently ignored — same rule as the
		// request-log endpoints.
		scope := team.ID
		if team.IsAdmin {
			scope = r.URL.Query().Get("team")
		} else if want := r.URL.Query().Get("team"); want != "" && want != team.ID {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"team "+team.ID+" may only read its own summary")
			return
		}

		res := svc.Build(r.Context(), summary.Options{Range: rng, TeamID: scope})
		writeJSON(w, log, http.StatusOK, toSummaryView(res, health, cacheEnabled))
	}
}
