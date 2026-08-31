package telemetry

import (
	"time"
)

// nearMissBand is how far below the threshold a miss still counts as "near".
// Wide enough to show whether lowering the threshold would buy anything,
// narrow enough that unrelated prompts do not flood the histogram.
const nearMissBand = 0.08

// CacheObserver adapts Metrics to internal/cache's Observer interface.
//
// It lives here rather than in internal/cache so that package stays free of a
// Prometheus dependency — the same one-way arrow every other package keeps
// with telemetry.
type CacheObserver struct {
	metrics   *Metrics
	threshold float64
}

func NewCacheObserver(m *Metrics, threshold float32) *CacheObserver {
	return &CacheObserver{metrics: m, threshold: float64(threshold)}
}

// ObserveLookup records one lookup outcome.
//
// tier is empty on a miss that never reached the semantic tier, which is
// reported as "none" rather than dropped: without it the denominator of the
// hit rate would silently exclude the cheapest misses.
func (o *CacheObserver) ObserveLookup(team, tier string, hit bool, similarity float32, d time.Duration) {
	if o == nil || o.metrics == nil {
		return
	}

	label := tier
	if label == "" {
		label = "none"
	}

	o.metrics.CacheLookupsTotal.WithLabelValues(team, label, resultLabel(hit)).Inc()
	o.metrics.CacheLookupDuration.WithLabelValues(label).Observe(d.Seconds())

	// Only the semantic tier actually scores anything. An exact hit is
	// definitionally 1.0, and a zero means no candidate was compared at all
	// (an empty bucket); charting either would pile up a spike that says
	// nothing about where the threshold belongs.
	if tier != "semantic" || similarity == 0 {
		return
	}

	score := float64(similarity)
	o.metrics.CacheSimilarity.Observe(score)

	if !hit && score >= o.threshold-nearMissBand && score < o.threshold {
		o.metrics.CacheNearMisses.Observe(score)
	}
}

func (o *CacheObserver) ObserveDegraded(reason string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.CacheDegradedTotal.WithLabelValues(reason).Inc()
}

func resultLabel(hit bool) string {
	if hit {
		return "hit"
	}
	return "miss"
}
