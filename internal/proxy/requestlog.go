// Post-response request-log middleware (Part 2, Phase 1).
package proxy

import (
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

// RequestLogger is the slice of logstore.Writer this package needs.
type RequestLogger interface {
	Write(rec logstore.Record)
}

// RequestLog records one row per authenticated request once the response has
// been delivered. Requests that never authenticated have no team to attribute
// a row to and are skipped.
func RequestLog(logger RequestLogger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if logger == nil {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			m := metricsFrom(r.Context())
			if m == nil || m.teamID == "" {
				return
			}

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}

			var traceID string
			if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
				traceID = sc.TraceID().String()
			}

			logger.Write(logstore.Record{
				ID:                      RequestIDFrom(r.Context()),
				Timestamp:               m.start,
				TeamID:                  m.teamID,
				RequestedModel:          m.requestedModel,
				ServedModel:             m.servedModel,
				Provider:                m.providerName,
				StatusCode:              status,
				InputTokens:             m.usage.InputTokens,
				OutputTokens:            m.usage.OutputTokens,
				CostMicros:              m.costMicros,
				LatencyMS:               millis(time.Since(m.start)),
				OverheadMS:              millis(m.overhead()),
				Fallback:                m.fellBack,
				TraceID:                 traceID,
				FallbackCostDeltaMicros: m.fallbackCostDeltaMicros,
			})
		})
	}
}

func millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
