package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/summary"
)

// fakeSummary records the Options it was built with, which is how the scoping
// tests assert on what the handler resolved from the key and query string.
type fakeSummary struct {
	got    summary.Options
	result summary.Result
}

func (f *fakeSummary) Build(_ context.Context, opts summary.Options) summary.Result {
	f.got = opts
	return f.result
}

func summaryServer(t *testing.T, svc SummaryService, health HealthReader) *httptest.Server {
	t.Helper()
	reg := requestLogRegistry(t) // acme = admin, globex = not
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		reg, &fakeSpendReader{}, fakeProviderLister{}, health, &fakeBreakerController{},
		nil, fakeReloader, nil, reg, svc, nil, nil, nil, QualityFeedbackConfig{}, false, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

func TestSummaryScoping(t *testing.T) {
	cases := map[string]struct {
		key      string
		query    string
		wantTeam string
		wantCode int
	}{
		"non-admin is pinned to its own team": {"globex-key", "", "globex", 200},
		"non-admin cannot name another team":  {"globex-key", "?team=acme", "", 400},
		"admin defaults to all teams":         {"acme-key", "", "", 200},
		"admin may filter to one team":        {"acme-key", "?team=globex", "globex", 200},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeSummary{}
			srv := summaryServer(t, f, fakeHealthReader{})
			resp := getWithKey(t, srv, "/admin/summary"+tc.query, tc.key)
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if tc.wantCode == 200 && f.got.TeamID != tc.wantTeam {
				t.Errorf("Options.TeamID = %q, want %q", f.got.TeamID, tc.wantTeam)
			}
		})
	}
}

func TestSummaryRangeValidation(t *testing.T) {
	f := &fakeSummary{}
	srv := summaryServer(t, f, fakeHealthReader{})

	if got := getWithKey(t, srv, "/admin/summary", "acme-key"); got.StatusCode != 200 || f.got.Range != "24h" {
		t.Errorf("default: status=%d range=%q, want 200/24h", got.StatusCode, f.got.Range)
	}
	if got := getWithKey(t, srv, "/admin/summary?range=1h", "acme-key"); got.StatusCode != 200 || f.got.Range != "1h" {
		t.Errorf("explicit: status=%d range=%q, want 200/1h", got.StatusCode, f.got.Range)
	}
	if got := getWithKey(t, srv, "/admin/summary?range=90m", "acme-key"); got.StatusCode != http.StatusBadRequest {
		t.Errorf("bad range: status = %d, want 400", got.StatusCode)
	}
}

func TestSummaryDisabledWhenNil(t *testing.T) {
	srv := summaryServer(t, nil, fakeHealthReader{})
	if got := getWithKey(t, srv, "/admin/summary", "acme-key").StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", got)
	}
}

func TestSummaryMergesProviderHealthAndCachePlaceholder(t *testing.T) {
	f := &fakeSummary{result: summary.Result{Range: "24h", GeneratedAt: time.Now(), Degraded: true}}
	health := fakeHealthReader{snapshots: []health.ProviderHealth{
		{Provider: "groq", Status: health.StatusHealthy, ErrorRate: 0, P99Latency: 200 * time.Millisecond},
	}}
	srv := summaryServer(t, f, health)

	resp := getWithKey(t, srv, "/admin/summary", "acme-key")
	var v summaryView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !v.Degraded {
		t.Error("degraded flag not propagated")
	}
	if len(v.Providers) != 1 || v.Providers[0].Provider != "groq" || v.Providers[0].P99LatencyMillis != 200 {
		t.Errorf("provider health not merged: %+v", v.Providers)
	}
	if v.Cache.Enabled || v.Cache.HitRate != nil {
		t.Errorf("cache placeholder should be disabled/null, got %+v", v.Cache)
	}
}
