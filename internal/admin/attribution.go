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

type attributionView struct {
	Range       string           `json:"range"`
	GeneratedAt string           `json:"generated_at"`
	Fallback    fallbackAttrView `json:"fallback"`
	Cache       any              `json:"cache"` // null until Phase 7
}

func handleAttribution(reqLog RequestLogReader, authr KeyAuthenticator, log *slog.Logger) http.HandlerFunc {
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

		writeJSON(w, log, http.StatusOK, attributionView{
			Range:       rangeKey,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Fallback: fallbackAttrView{
				ExtraMicros: attr.ExtraMicros, ExtraUSD: microsToUSD(attr.ExtraMicros),
				SavedMicros: attr.SavedMicros, SavedUSD: microsToUSD(attr.SavedMicros),
				NetMicros: attr.NetMicros(), NetUSD: microsToUSD(attr.NetMicros()),
			},
			Cache: nil,
		})
	}
}
