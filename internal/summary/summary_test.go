package summary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProm records every PromQL it is asked and replies with a fixed vector
// value, or a canned failure when told to.
type fakeProm struct {
	mu      sync.Mutex
	queries []string
	value   string // sample value to return
	status  int    // HTTP status; 0 means 200
}

func (f *fakeProm) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.Query().Get("query"))
		f.mu.Unlock()

		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		val := f.value
		if val == "" {
			val = "1"
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"` + val + `"],[1700000900,"` + val + `"]]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"` + val + `"]}]}}`))
	}
}

func (f *fakeProm) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func newService(t *testing.T, f *fakeProm) *Service {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return NewService(Config{PrometheusURL: srv.URL, CacheTTL: time.Minute, HTTPTimeout: time.Second})
}

func TestBuildScopesTeamLabelledMetrics(t *testing.T) {
	f := &fakeProm{value: "42"}
	svc := newService(t, f)

	svc.Build(context.Background(), Options{Range: "1h", TeamID: "globex"})

	var reqQ, overheadQ string
	for _, q := range f.asked() {
		if strings.Contains(q, "switchyard_requests_total") && !strings.Contains(q, "5..") {
			reqQ = q
		}
		if strings.Contains(q, "gateway_overhead") {
			overheadQ = q
		}
	}
	if !strings.Contains(reqQ, `switchyard_requests_total{team="globex"}`) {
		t.Errorf("request query not scoped to team: %s", reqQ)
	}
	if strings.Contains(overheadQ, "team=") {
		t.Errorf("overhead query must not be team-scoped: %s", overheadQ)
	}
}

func TestBuildAdminIsUnscoped(t *testing.T) {
	f := &fakeProm{value: "1"}
	svc := newService(t, f)
	svc.Build(context.Background(), Options{Range: "24h", TeamID: ""})
	for _, q := range f.asked() {
		if strings.Contains(q, "team=") {
			t.Errorf("unscoped build issued a team-filtered query: %s", q)
		}
	}
}

func TestBuildDegradesOnPrometheusFailure(t *testing.T) {
	f := &fakeProm{status: http.StatusBadGateway}
	svc := newService(t, f)
	got := svc.Build(context.Background(), Options{Range: "1h"})
	if !got.Degraded {
		t.Fatal("expected Degraded on a 502 from Prometheus")
	}
	if got.RequestCount != nil {
		t.Errorf("RequestCount = %v, want nil on failure", *got.RequestCount)
	}
}

func TestBuildDegradesWhenNoURLConfigured(t *testing.T) {
	svc := NewService(Config{PrometheusURL: "", CacheTTL: time.Minute})
	got := svc.Build(context.Background(), Options{Range: "7d"})
	if !got.Degraded {
		t.Fatal("expected Degraded when no Prometheus URL is set")
	}
}

func TestBuildTreatsNaNAsNoData(t *testing.T) {
	f := &fakeProm{value: "NaN"}
	svc := newService(t, f)
	got := svc.Build(context.Background(), Options{Range: "1h"})
	if got.Degraded {
		t.Error("NaN is no-data, not a failure — Degraded should be false")
	}
	if got.OverheadP95 != nil {
		t.Errorf("OverheadP95 = %v, want nil for NaN", *got.OverheadP95)
	}
}

func TestBuildCachesWithinTTL(t *testing.T) {
	f := &fakeProm{value: "1"}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	svc := NewService(Config{PrometheusURL: srv.URL, CacheTTL: time.Minute, HTTPTimeout: time.Second})

	svc.Build(context.Background(), Options{Range: "1h", TeamID: "acme"})
	n := len(f.asked())
	svc.Build(context.Background(), Options{Range: "1h", TeamID: "acme"})
	if len(f.asked()) != n {
		t.Errorf("second Build hit Prometheus again: %d queries then %d", n, len(f.asked()))
	}
	// A different scope is a different cache key and does query.
	svc.Build(context.Background(), Options{Range: "1h", TeamID: "globex"})
	if len(f.asked()) == n {
		t.Error("a different team scope should not be served from acme's cache entry")
	}
}

func TestValidRange(t *testing.T) {
	for _, ok := range []string{"1h", "24h", "7d"} {
		if !ValidRange(ok) {
			t.Errorf("ValidRange(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "2h", "30d", "1H"} {
		if ValidRange(bad) {
			t.Errorf("ValidRange(%q) = true", bad)
		}
	}
}

func TestBuildPopulatesAlignedSeries(t *testing.T) {
	f := &fakeProm{value: "3"}
	svc := newService(t, f)
	got := svc.Build(context.Background(), Options{Range: "1h", TeamID: "acme"})

	if got.Degraded {
		t.Fatal("series queries succeeded; Degraded should be false")
	}
	if got.Series == nil {
		t.Fatal("Series is nil")
	}
	if got.Series.StepSeconds != 60 {
		t.Errorf("StepSeconds = %d, want 60 for 1h", got.Series.StepSeconds)
	}
	if len(got.Series.Traffic) != 2 || got.Series.Traffic[0].Value != 3 {
		t.Errorf("traffic points = %+v", got.Series.Traffic)
	}
	// The three percentile queries share a timestamp grid, so each point
	// carries all three.
	for _, p := range got.Series.Overhead {
		if p.P50 == nil || p.P95 == nil || p.P99 == nil {
			t.Errorf("overhead point %v missing a percentile", p.T)
		}
	}
	if len(got.Series.Overhead) != 2 {
		t.Errorf("overhead points = %d, want 2", len(got.Series.Overhead))
	}
}

func TestBuildSeriesFailureDegradesButKeepsScalars(t *testing.T) {
	// A fake that answers instant queries but fails range queries.
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "query_range") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"9"]}]}}`))
	}))
	t.Cleanup(srv.Close)
	svc := NewService(Config{PrometheusURL: srv.URL, CacheTTL: time.Minute, HTTPTimeout: time.Second})

	got := svc.Build(context.Background(), Options{Range: "24h"})
	if !got.Degraded {
		t.Error("a failed range query should set Degraded")
	}
	if got.RequestCount == nil || *got.RequestCount != 9 {
		t.Errorf("scalars should still be present: RequestCount = %v", got.RequestCount)
	}
	if got.Series != nil {
		t.Error("Series should be nil when a range query failed")
	}
}
