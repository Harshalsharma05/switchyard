// Phase 9's Prometheus metric definitions.
package telemetry

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}

var overheadBuckets = []float64{0.0005, 0.001, 0.0015, 0.002, 0.003, 0.005, 0.0075, 0.01, 0.015, 0.02, 0.03, 0.05}

// cacheLookupBuckets straddle two very different scales on purpose: the exact
// tier lands in sub-millisecond buckets, the semantic tier around 500ms once
// an embedding call is involved.
var cacheLookupBuckets = []float64{0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 2, 5}

// similarityBuckets are dense above 0.8 because that is the only region where
// the threshold decision is actually contested.
var similarityBuckets = []float64{0, 0.5, 0.6, 0.7, 0.75, 0.8, 0.85, 0.88, 0.9, 0.92, 0.94, 0.96, 0.98, 1}

type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal             *prometheus.CounterVec
	ErrorsTotal               *prometheus.CounterVec
	RetriesTotal              *prometheus.CounterVec
	FallbacksTotal            *prometheus.CounterVec
	RatelimitRejectionsTotal  *prometheus.CounterVec
	BudgetRejectionsTotal     *prometheus.CounterVec
	BreakerTransitionsTotal   *prometheus.CounterVec
	TokensTotal               *prometheus.CounterVec
	CostMicrodollarsTotal     *prometheus.CounterVec
	RequestLogRowsTotal       *prometheus.CounterVec
	RetentionRowsDeletedTotal prometheus.Counter

	RequestDuration  *prometheus.HistogramVec
	GatewayOverhead  prometheus.Histogram
	ProviderDuration *prometheus.HistogramVec
	TimeToFirstToken *prometheus.HistogramVec

	ProviderHealth           *prometheus.GaugeVec
	BreakerState             *prometheus.GaugeVec
	BudgetUtilizationRatio   *prometheus.GaugeVec
	RatelimitTokensRemaining *prometheus.GaugeVec
	InflightRequests         *prometheus.GaugeVec

	// Phase 7's semantic cache. CacheLookupsTotal is labelled by tier and
	// result so hit rate is derivable per tier — an exact hit and a semantic
	// hit cost wildly different amounts and must not be averaged together.
	CacheLookupsTotal   *prometheus.CounterVec
	CacheDegradedTotal  *prometheus.CounterVec
	CacheLookupDuration *prometheus.HistogramVec
	CacheSimilarity     prometheus.Histogram
	CacheNearMisses     prometheus.Histogram

	// Phase 9's async quality verification. QualitySamplesTotal is labelled
	// by sampling reason and outcome so "the sampled proportion matches
	// configuration" is checkable straight from the metric.
	QualitySamplesTotal *prometheus.CounterVec
	QualityScore        *prometheus.HistogramVec
	QualityQueueDepth   prometheus.Gauge

	RequestLogQueueDepth        prometheus.Gauge
	RetentionLastSweepTimestamp prometheus.Gauge
}

func NewMetrics() (*Metrics, error) {
	m := &Metrics{registry: prometheus.NewRegistry()}

	m.RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_requests_total",
		Help: "Total requests handled, by team, provider, model, and final HTTP status.",
	}, []string{"team", "provider", "model", "status"})

	m.ErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_errors_total",
		Help: "Total provider call failures, by team, provider, model, and provider.Error kind.",
	}, []string{"team", "provider", "model", "error_kind"})

	m.RetriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_retries_total",
		Help: "Total same-provider retry attempts, by provider, model, and the failure reason that triggered them.",
	}, []string{"provider", "model", "reason"})

	m.FallbacksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_fallbacks_total",
		Help: "Total fallback transitions from one candidate to the next, by source provider, destination provider, and reason.",
	}, []string{"from_provider", "to_provider", "reason"})

	m.RatelimitRejectionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_ratelimit_rejections_total",
		Help: "Total requests rejected for exceeding a rate limit, by team and limit type (rpm or tpm).",
	}, []string{"team", "limit_type"})

	m.BudgetRejectionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_budget_rejections_total",
		Help: "Total requests rejected for exceeding a team's monthly budget.",
	}, []string{"team"})

	m.BreakerTransitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_breaker_transitions_total",
		Help: "Total circuit breaker state transitions, by provider, model, source state, and destination state.",
	}, []string{"provider", "model", "from_state", "to_state"})

	m.TokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_tokens_total",
		Help: "Total tokens processed, by team, provider, model, and direction (input or output).",
	}, []string{"team", "provider", "model", "direction"})

	m.CostMicrodollarsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_cost_microdollars_total",
		Help: "Total cost in integer micro-dollars, by team, provider, and model. Compute spend rates in PromQL, not here.",
	}, []string{"team", "provider", "model"})

	m.RequestLogRowsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_requestlog_rows_total",
		Help: "Request-log rows by outcome: written, dropped (queue full), or failed (Postgres write error).",
	}, []string{"outcome"})

	m.RequestLogQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "switchyard_requestlog_queue_depth",
		Help: "Request-log rows currently buffered and awaiting a flush.",
	})

	m.RetentionRowsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "switchyard_retention_rows_deleted_total",
		Help: "Request-log detail rows rolled into the daily summary and deleted by retention.",
	})

	m.RetentionLastSweepTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "switchyard_retention_last_sweep_timestamp_seconds",
		Help: "Unix time of the last successful retention sweep. Alert when this stops advancing.",
	})

	m.RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "switchyard_request_duration_seconds",
		Help:    "End-to-end request duration, by provider and model.",
		Buckets: durationBuckets,
	}, []string{"provider", "model"})

	m.GatewayOverhead = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "switchyard_gateway_overhead_seconds",
		Help:    "Gateway-only overhead, excluding time spent in the provider. The project's headline number.",
		Buckets: overheadBuckets,
	})

	m.ProviderDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "switchyard_provider_duration_seconds",
		Help:    "Time spent waiting on the upstream provider, by provider and model.",
		Buckets: durationBuckets,
	}, []string{"provider", "model"})

	m.TimeToFirstToken = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "switchyard_time_to_first_token_seconds",
		Help:    "Time to the first streamed token, by provider and model. Streaming requests only.",
		Buckets: durationBuckets,
	}, []string{"provider", "model"})

	m.ProviderHealth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "switchyard_provider_health",
		Help: "Current health status per provider and model: 0=down, 1=degraded, 2=healthy.",
	}, []string{"provider", "model"})

	m.BreakerState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "switchyard_breaker_state",
		Help: "Current circuit breaker state per provider and model: 0=closed, 1=half-open, 2=open.",
	}, []string{"provider", "model"})

	m.BudgetUtilizationRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "switchyard_budget_utilization_ratio",
		Help: "Fraction of a team's monthly budget spent so far, in [0,1] and beyond if overspent.",
	}, []string{"team"})

	m.RatelimitTokensRemaining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "switchyard_ratelimit_tokens_remaining",
		Help: "Tokens remaining in a team's rate limit bucket, by limit type (rpm or tpm).",
	}, []string{"team", "limit_type"})

	m.InflightRequests = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "switchyard_inflight_requests",
		Help: "Requests currently in flight against a provider.",
	}, []string{"provider"})

	m.CacheLookupsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_cache_lookups_total",
		Help: "Semantic cache lookups, by team, tier (exact, semantic, none) and result (hit or miss). Overview's hit rate is derived from this.",
	}, []string{"team", "tier", "result"})

	m.CacheDegradedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_cache_degraded_total",
		Help: "Cache lookups that failed open and were served as a miss, by reason. Non-zero means the cache is silently off.",
	}, []string{"reason"})

	m.CacheLookupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "switchyard_cache_lookup_seconds",
		Help:    "Cache lookup wall time, by tier. The exact tier is the one held to the overhead budget.",
		Buckets: cacheLookupBuckets,
	}, []string{"tier"})

	m.CacheSimilarity = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "switchyard_cache_similarity",
		Help:    "Best cosine similarity found on every semantic lookup, hit or miss.",
		Buckets: similarityBuckets,
	})

	// Near-misses are scored separately from the full distribution because the
	// threshold question lives in this narrow band: these are the requests a
	// slightly lower threshold would have converted into hits.
	m.CacheNearMisses = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "switchyard_cache_near_miss_similarity",
		Help:    "Best similarity on semantic misses that scored within the near-miss band below the threshold.",
		Buckets: similarityBuckets,
	})

	m.QualitySamplesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_quality_samples_total",
		Help: "Sampled responses by reason (downgraded, near_threshold_cache_hit, flagged, routed_sample) and outcome (enqueued, dropped, scored, error).",
	}, []string{"reason", "outcome"})

	m.QualityScore = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "switchyard_quality_score",
		Help:    "LLM-as-judge score (1-5) of sampled responses, by team.",
		Buckets: []float64{1, 1.5, 2, 2.5, 3, 3.5, 4, 4.5, 5},
	}, []string{"team"})

	m.QualityQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "switchyard_quality_queue_depth",
		Help: "Samples waiting to be scored. Sustained growth means the judge cannot keep up and samples are being dropped.",
	})

	collectors := []prometheus.Collector{
		m.RequestsTotal, m.ErrorsTotal, m.RetriesTotal, m.FallbacksTotal,
		m.RatelimitRejectionsTotal, m.BudgetRejectionsTotal, m.BreakerTransitionsTotal,
		m.TokensTotal, m.CostMicrodollarsTotal, m.RequestLogRowsTotal,
		m.RetentionRowsDeletedTotal,
		m.RequestDuration, m.GatewayOverhead, m.ProviderDuration, m.TimeToFirstToken,
		m.ProviderHealth, m.BreakerState, m.BudgetUtilizationRatio,
		m.RatelimitTokensRemaining, m.InflightRequests, m.RequestLogQueueDepth,
		m.RetentionLastSweepTimestamp,
		m.CacheLookupsTotal, m.CacheDegradedTotal, m.CacheLookupDuration,
		m.CacheSimilarity, m.CacheNearMisses,
		m.QualitySamplesTotal, m.QualityScore, m.QualityQueueDepth,
	}
	for _, c := range collectors {
		if err := m.registry.Register(c); err != nil {
			return nil, fmt.Errorf("registering metric: %w", err)
		}
	}

	return m, nil
}

func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

func (m *Metrics) RegisterRuntimeCollectors() error {
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := m.registry.Register(c); err != nil {
			return fmt.Errorf("registering runtime collector: %w", err)
		}
	}
	return nil
}
