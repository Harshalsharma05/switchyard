// Cost trends (Part 2, Step 6.2). GET /admin/costs aggregates the request log
// into time buckets split by provider, model, or team. Any valid key reaches
// it; a non-admin is scoped to its own team, an admin may span or narrow.
package admin

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

// costRange maps a ?range= value onto a lookback and a bucket resolution.
// 30d matches the default retention window — beyond it only requests_daily
// still holds data, which the daily bucket already reads.
var costRanges = map[string]struct {
	lookback time.Duration
	bucket   logstore.CostBucket
}{
	"24h": {24 * time.Hour, logstore.CostHourly},
	"7d":  {7 * 24 * time.Hour, logstore.CostDaily},
	"30d": {30 * 24 * time.Hour, logstore.CostDaily},
}

var costDimensions = map[string]logstore.CostDimension{
	"provider": logstore.CostByProvider,
	"model":    logstore.CostByModel,
	"team":     logstore.CostByTeam,
}

type costPointView struct {
	T           string           `json:"t"`
	TotalMicros int64            `json:"total_micros"`
	TotalUSD    float64          `json:"total_usd"`
	Breakdown   map[string]int64 `json:"breakdown"`
}

type costsView struct {
	Range       string          `json:"range"`
	Bucket      string          `json:"bucket"`
	By          string          `json:"by"`
	GeneratedAt string          `json:"generated_at"`
	Keys        []string        `json:"keys"`
	Series      []costPointView `json:"series"`
}

func handleCosts(reqLog RequestLogReader, authr KeyAuthenticator, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reqLog == nil {
			writeRequestLogDisabled(w, log)
			return
		}

		team, ok := authenticate(w, r, authr, log)
		if !ok {
			return
		}

		q := r.URL.Query()

		rangeKey := q.Get("range")
		if rangeKey == "" {
			rangeKey = "24h"
		}
		spec, ok := costRanges[rangeKey]
		if !ok {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"range must be one of 24h, 7d, 30d")
			return
		}

		byKey := q.Get("by")
		if byKey == "" {
			byKey = "provider"
		}
		dim, ok := costDimensions[byKey]
		if !ok {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"by must be one of provider, model, team")
			return
		}

		// Same scoping rule as /admin/requests: a non-admin's team comes from its
		// key, and naming another team is refused rather than ignored.
		scope := team.ID
		if team.IsAdmin {
			scope = q.Get("team")
		} else if want := q.Get("team"); want != "" && want != team.ID {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"team "+team.ID+" may only read its own costs")
			return
		}

		cells, err := reqLog.CostSeries(r.Context(), logstore.CostQuery{
			Since:     time.Now().UTC().Add(-spec.lookback),
			Bucket:    spec.bucket,
			Dimension: dim,
			TeamID:    scope,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "querying cost series", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the request log")
			return
		}

		writeJSON(w, log, http.StatusOK, buildCostsView(rangeKey, byKey, string(spec.bucket), cells))
	}
}

// buildCostsView folds the flat cells into one point per bucket, dropping
// zero-cost keys so a 402 that never reached a provider does not spawn an
// empty chart series.
func buildCostsView(rangeKey, byKey, bucket string, cells []logstore.CostCell) costsView {
	v := costsView{
		Range:       rangeKey,
		Bucket:      bucket,
		By:          byKey,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Keys:        []string{},
		Series:      []costPointView{},
	}

	keySet := map[string]struct{}{}
	byBucket := map[string]*costPointView{}
	var order []string

	for _, c := range cells {
		if c.Micros == 0 {
			continue
		}
		ts := c.Bucket.UTC().Format(time.RFC3339)
		p, ok := byBucket[ts]
		if !ok {
			p = &costPointView{T: ts, Breakdown: map[string]int64{}}
			byBucket[ts] = p
			order = append(order, ts)
		}
		p.TotalMicros += c.Micros
		p.TotalUSD = microsToUSD(p.TotalMicros)
		p.Breakdown[c.Key] += c.Micros
		keySet[c.Key] = struct{}{}
	}

	sort.Strings(order)
	for _, ts := range order {
		v.Series = append(v.Series, *byBucket[ts])
	}
	for k := range keySet {
		v.Keys = append(v.Keys, k)
	}
	sort.Strings(v.Keys)
	return v
}
