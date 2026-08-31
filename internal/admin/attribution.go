// Cost attribution (Part 2, Step 6.3). GET /admin/attribution reports what
// resilience cost: the fallback cost delta, split into what fallbacks added
// and what they saved, over a range. Cache and routing savings are null until
// Phases 7 and 8 wire them. Team-scoped like /admin/costs.
package admin

import (
	"log/slog"
	"net/http"
	"time"
)

type fallbackAttrView struct {
	ExtraMicros int64   `json:"extra_micros"`
	ExtraUSD    float64 `json:"extra_usd"`
	SavedMicros int64   `json:"saved_micros"`
	SavedUSD    float64 `json:"saved_usd"`
	NetMicros   int64   `json:"net_micros"`
	NetUSD      float64 `json:"net_usd"`
}

// cacheAttrView is Step 7.6's savings panel.
//
// SavedMicros is priced from the real token counts on cache-hit rows, at the
// served model's own price — not estimated from an average request. Requests
// is the denominator that makes the number interpretable: savings without a
// hit count says nothing about whether the cache is working.
type cacheAttrView struct {
	Hits        int64   `json:"hits"`
	Misses      int64   `json:"misses"`
	HitRate     float64 `json:"hit_rate"`
	SavedMicros int64   `json:"saved_micros"`
	SavedUSD    float64 `json:"saved_usd"`
}

type attributionView struct {
	Range       string           `json:"range"`
	GeneratedAt string           `json:"generated_at"`
	Fallback    fallbackAttrView `json:"fallback"`
	Cache       *cacheAttrView   `json:"cache"`
}

func handleAttribution(reqLog RequestLogReader, calc CostCalculator, authr KeyAuthenticator, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reqLog == nil {
			writeRequestLogDisabled(w, log)
			return
		}

		team, ok := authenticate(w, r, authr, log)
		if !ok {
			return
		}

		rangeKey := r.URL.Query().Get("range")
		if rangeKey == "" {
			rangeKey = "24h"
		}
		spec, ok := costRanges[rangeKey]
		if !ok {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"range must be one of 24h, 7d, 30d")
			return
		}

		scope := team.ID
		if team.IsAdmin {
			scope = r.URL.Query().Get("team")
		} else if want := r.URL.Query().Get("team"); want != "" && want != team.ID {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"team "+team.ID+" may only read its own attribution")
			return
		}

		attr, err := reqLog.FallbackCostSince(r.Context(), time.Now().UTC().Add(-spec.lookback), scope)
		if err != nil {
			log.ErrorContext(r.Context(), "summing fallback attribution", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the request log")
			return
		}

		cacheView, err := cacheAttribution(r, reqLog, calc, spec.lookback, scope, log)
		if err != nil {
			log.ErrorContext(r.Context(), "summing cache attribution", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the request log")
			return
		}

		writeJSON(w, log, http.StatusOK, attributionView{
			Range:       rangeKey,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Fallback: fallbackAttrView{
				ExtraMicros: attr.ExtraMicros, ExtraUSD: microsToUSD(attr.ExtraMicros),
				SavedMicros: attr.SavedMicros, SavedUSD: microsToUSD(attr.SavedMicros),
				NetMicros: attr.NetMicros(), NetUSD: microsToUSD(attr.NetMicros()),
			},
			Cache: cacheView,
		})
	}
}

// cacheAttribution prices the cache's savings, or returns nil when no pricing
// table is wired — a null panel keeps Usage & Cost in its empty state rather
// than showing a confident zero.
func cacheAttribution(r *http.Request, reqLog RequestLogReader, calc CostCalculator, lookback time.Duration, scope string, log *slog.Logger) (*cacheAttrView, error) {
	if calc == nil {
		return nil, nil
	}

	saved, err := reqLog.CacheSavingsSince(r.Context(), time.Now().UTC().Add(-lookback), scope)
	if err != nil {
		return nil, err
	}

	view := &cacheAttrView{Hits: saved.Hits, Misses: saved.Misses}
	if total := saved.Hits + saved.Misses; total > 0 {
		view.HitRate = float64(saved.Hits) / float64(total)
	}

	for _, g := range saved.Groups {
		micros, err := calc.Cost(g.Model, int(g.InputTokens), int(g.OutputTokens))
		if err != nil {
			// A model that has since left configs/providers.yaml has no price.
			// Skipping it understates savings, which is the safe direction —
			// far better than failing a report that is otherwise correct.
			log.WarnContext(r.Context(), "pricing cache savings",
				slog.String("model", g.Model), slog.Any("error", err))
			continue
		}
		view.SavedMicros += micros
	}
	view.SavedUSD = microsToUSD(view.SavedMicros)
	return view, nil
}
