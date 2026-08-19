package telemetry

import "testing"

func TestNewMetricsRegistersEveryFamily(t *testing.T) {
	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	m.RequestsTotal.WithLabelValues("acme", "groq", "gpt-oss-120b", "200").Inc()
	m.ErrorsTotal.WithLabelValues("acme", "groq", "gpt-oss-120b", "timeout").Inc()
	m.RetriesTotal.WithLabelValues("groq", "gpt-oss-120b", "timeout").Inc()
	m.FallbacksTotal.WithLabelValues("groq", "gemini", "timeout").Inc()
	m.RatelimitRejectionsTotal.WithLabelValues("acme", "rpm").Inc()
	m.BudgetRejectionsTotal.WithLabelValues("acme").Inc()
	m.BreakerTransitionsTotal.WithLabelValues("groq", "gpt-oss-120b", "closed", "open").Inc()
	m.TokensTotal.WithLabelValues("acme", "groq", "gpt-oss-120b", "input").Add(10)
	m.CostMicrodollarsTotal.WithLabelValues("acme", "groq", "gpt-oss-120b").Add(100)

	m.RequestDuration.WithLabelValues("groq", "gpt-oss-120b").Observe(0.5)
	m.GatewayOverhead.Observe(0.001)
	m.ProviderDuration.WithLabelValues("groq", "gpt-oss-120b").Observe(0.4)
	m.TimeToFirstToken.WithLabelValues("groq", "gpt-oss-120b").Observe(0.2)

	m.ProviderHealth.WithLabelValues("groq", "gpt-oss-120b").Set(2)
	m.BreakerState.WithLabelValues("groq", "gpt-oss-120b").Set(0)
	m.BudgetUtilizationRatio.WithLabelValues("acme").Set(0.5)
	m.RatelimitTokensRemaining.WithLabelValues("acme", "tpm").Set(1000)
	m.InflightRequests.WithLabelValues("groq").Set(1)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	want := []string{
		"switchyard_requests_total",
		"switchyard_errors_total",
		"switchyard_retries_total",
		"switchyard_fallbacks_total",
		"switchyard_ratelimit_rejections_total",
		"switchyard_budget_rejections_total",
		"switchyard_breaker_transitions_total",
		"switchyard_tokens_total",
		"switchyard_cost_microdollars_total",
		"switchyard_request_duration_seconds",
		"switchyard_gateway_overhead_seconds",
		"switchyard_provider_duration_seconds",
		"switchyard_time_to_first_token_seconds",
		"switchyard_provider_health",
		"switchyard_breaker_state",
		"switchyard_budget_utilization_ratio",
		"switchyard_ratelimit_tokens_remaining",
		"switchyard_inflight_requests",
	}

	got := map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("missing metric family %q", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d metric families, want %d", len(got), len(want))
	}
}

func TestNewMetricsIndependentRegistries(t *testing.T) {
	if _, err := NewMetrics(); err != nil {
		t.Fatalf("first NewMetrics: %v", err)
	}
	if _, err := NewMetrics(); err != nil {
		t.Fatalf("second NewMetrics on its own registry should not conflict: %v", err)
	}
}
