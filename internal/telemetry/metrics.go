// Phase 9's Prometheus metric definitions.
package telemetry

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}

var overheadBuckets = []float64{0.0005, 0.001, 0.0015, 0.002, 0.003, 0.005, 0.0075, 0.01, 0.015, 0.02, 0.03, 0.05}

type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal            *prometheus.CounterVec
	ErrorsTotal              *prometheus.CounterVec
	RetriesTotal             *prometheus.CounterVec
	FallbacksTotal           *prometheus.CounterVec
	RatelimitRejectionsTotal *prometheus.CounterVec
	BudgetRejectionsTotal    *prometheus.CounterVec
	BreakerTransitionsTotal  *prometheus.CounterVec
	TokensTotal              *prometheus.CounterVec
	CostMicrodollarsTotal    *prometheus.CounterVec

	RequestDuration  *prometheus.HistogramVec
	GatewayOverhead  prometheus.Histogram
	ProviderDuration *prometheus.HistogramVec
	TimeToFirstToken *prometheus.HistogramVec

	ProviderHealth           *prometheus.GaugeVec
	BreakerState             *prometheus.GaugeVec
	BudgetUtilizationRatio   *prometheus.GaugeVec
	RatelimitTokensRemaining *prometheus.GaugeVec
	InflightRequests         *prometheus.GaugeVec
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

	collectors := []prometheus.Collector{
		m.RequestsTotal, m.ErrorsTotal, m.RetriesTotal, m.FallbacksTotal,
		m.RatelimitRejectionsTotal, m.BudgetRejectionsTotal, m.BreakerTransitionsTotal,
		m.TokensTotal, m.CostMicrodollarsTotal,
		m.RequestDuration, m.GatewayOverhead, m.ProviderDuration, m.TimeToFirstToken,
		m.ProviderHealth, m.BreakerState, m.BudgetUtilizationRatio,
		m.RatelimitTokensRemaining, m.InflightRequests,
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
