// Quality feedback (Part 2, Step 9.3). GET /admin/quality/feedback surfaces
// the two loops that keep the cache and the router honest: near-threshold
// cache hits that scored badly (the similarity threshold may be too
// permissive) and downgraded requests that scored badly (candidate classifier
// mislabels). It surfaces the signal only — the gateway never retunes itself.
package admin

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// QualityFeedbackConfig is the tuning cmd/gateway passes from configs/quality.yaml.
type QualityFeedbackConfig struct {
	LowScoreThreshold float64
	ExampleLimit      int
}

type qualityReasonView struct {
	Reason    string  `json:"reason"`
	Scored    int64   `json:"scored"`
	AvgScore  float64 `json:"avg_score"`
	LowScored int64   `json:"low_scored"`
	MinScore  float64 `json:"min_score"`
	MaxScore  float64 `json:"max_score"`
}

type qualityExampleView struct {
	RequestID     string  `json:"request_id"`
	Timestamp     string  `json:"timestamp"`
	ServedModel   string  `json:"served_model"`
	RoutingTier   string  `json:"routing_tier"`
	RoutingReason string  `json:"routing_reason"`
	QualityScore  float64 `json:"quality_score"`
	TraceID       string  `json:"trace_id"`
}

type qualityLoopView struct {
	Reason string             `json:"reason"`
	Stat   *qualityReasonView `json:"stat"`
	Signal string             `json:"signal"`
}

type qualityFeedbackView struct {
	Range             string               `json:"range"`
	GeneratedAt       string               `json:"generated_at"`
	LowScoreThreshold float64              `json:"low_score_threshold"`
	Cache             qualityLoopView      `json:"cache"`
	Routing           qualityLoopView      `json:"routing"`
	Examples          []qualityExampleView `json:"examples"`
	ByReason          []qualityReasonView  `json:"by_reason"`
}

func handleQualityFeedback(reqLog RequestLogReader, cfg QualityFeedbackConfig, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reqLog == nil {
			writeRequestLogDisabled(w, log)
			return
		}

		rangeKey := r.URL.Query().Get("range")
		if rangeKey == "" {
			rangeKey = "7d"
		}
		spec, ok := costRanges[rangeKey]
		if !ok {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"range must be one of 24h, 7d, 30d")
			return
		}

		low := cfg.LowScoreThreshold
		if low < 1 || low > 5 {
			low = 3
		}
		if raw := r.URL.Query().Get("threshold"); raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 1 || v > 5 {
				writeError(w, log, http.StatusBadRequest, "invalid_request_error",
					"threshold must be a number between 1 and 5")
				return
			}
			low = v
		}

		fb, err := reqLog.QualityFeedbackSince(r.Context(),
			time.Now().UTC().Add(-spec.lookback), "", low, cfg.ExampleLimit)
		if err != nil {
			log.ErrorContext(r.Context(), "reading quality feedback", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the request log")
			return
		}

		byReason := make([]qualityReasonView, 0, len(fb.Reasons))
		statFor := map[string]*qualityReasonView{}
		for _, s := range fb.Reasons {
			v := qualityReasonView{
				Reason: s.Reason, Scored: s.Scored, AvgScore: round2(s.AvgScore),
				LowScored: s.LowScored, MinScore: s.MinScore, MaxScore: s.MaxScore,
			}
			byReason = append(byReason, v)
			vv := v
			statFor[s.Reason] = &vv
		}

		examples := make([]qualityExampleView, 0, len(fb.Examples))
		for _, e := range fb.Examples {
			examples = append(examples, qualityExampleView{
				RequestID: e.ID, Timestamp: e.Timestamp.Format(time.RFC3339Nano),
				ServedModel: e.ServedModel, RoutingTier: e.RoutingTier,
				RoutingReason: e.RoutingReason, QualityScore: e.QualityScore, TraceID: e.TraceID,
			})
		}

		writeJSON(w, log, http.StatusOK, qualityFeedbackView{
			Range:             rangeKey,
			GeneratedAt:       time.Now().UTC().Format(time.RFC3339Nano),
			LowScoreThreshold: low,
			Cache: qualityLoopView{
				Reason: string(reasonNearThreshold),
				Stat:   statFor[string(reasonNearThreshold)],
				Signal: cacheSignal(statFor[string(reasonNearThreshold)], low),
			},
			Routing: qualityLoopView{
				Reason: string(reasonDowngraded),
				Stat:   statFor[string(reasonDowngraded)],
				Signal: routingSignal(statFor[string(reasonDowngraded)], low, len(examples)),
			},
			Examples: examples,
			ByReason: byReason,
		})
	}
}

// reason labels mirror internal/quality's Reason constants. Duplicated rather
// than imported: this package reads them out of Postgres as plain strings and
// must not grow a dependency on the worker package for two literals.
const (
	reasonNearThreshold = "near_threshold_cache_hit"
	reasonDowngraded    = "downgraded"
)

func cacheSignal(s *qualityReasonView, low float64) string {
	if s == nil || s.Scored == 0 {
		return "No near-threshold cache hits have been scored in this range yet."
	}
	if s.LowScored == 0 {
		return fmt.Sprintf("All %d scored near-threshold cache hits held up (none at or below %.1f). No threshold change indicated.",
			s.Scored, low)
	}
	return fmt.Sprintf("%d of %d near-threshold cache hits scored at or below %.1f. The similarity threshold may be serving answers that do not hold up — raise it deliberately; the gateway does not auto-tune.",
		s.LowScored, s.Scored, low)
}

func routingSignal(s *qualityReasonView, low float64, examples int) string {
	if s == nil || s.Scored == 0 {
		return "No downgraded responses have been scored in this range yet."
	}
	if s.LowScored == 0 {
		return fmt.Sprintf("All %d scored downgrades held up (none at or below %.1f). The classifier is not over-downgrading.",
			s.Scored, low)
	}
	return fmt.Sprintf("%d of %d downgraded responses scored at or below %.1f. The %d listed below are candidate classifier mislabels — inspect each by request ID and add it to the labelled set by hand.",
		s.LowScored, s.Scored, low, examples)
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
