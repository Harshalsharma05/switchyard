package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
)

// fakeHealthReader is the fake behind HealthReader. Its zero value reports no
// providers at all, which is enough for every test that is not specifically
// about the health endpoint — the same role fakeProviderLister's zero value
// plays for ProviderLister.
type fakeHealthReader struct {
	snapshots []health.ProviderHealth
}

func (f fakeHealthReader) Snapshots() []health.ProviderHealth {
	return f.snapshots
}

// fakeBreakerController is the fake behind BreakerController. Its zero value
// reports nothing reset and no error, which is all the tests that are not
// about the breaker endpoint need.
type fakeBreakerController struct {
	count     int
	err       error
	snapshots map[resilience.Labels]resilience.BreakerSnapshot

	mu       sync.Mutex
	resetFor []string
}

func (f *fakeBreakerController) Reset(_ context.Context, providerName string) (int, error) {
	f.mu.Lock()
	f.resetFor = append(f.resetFor, providerName)
	f.mu.Unlock()
	return f.count, f.err
}

func (f *fakeBreakerController) Snapshots() map[resilience.Labels]resilience.BreakerSnapshot {
	return f.snapshots
}

func (f *fakeBreakerController) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resetFor...)
}

// newTestAdminServerWithHealth builds a router with a caller-controlled
// HealthReader and permissive defaults for everything else this file's tests
// don't care about — the same shape newTestAdminServerWithReload gives
// reload_test.go for the piece it does care about.
func newTestAdminServerWithHealth(t *testing.T, reader HealthReader) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, reader, &fakeBreakerController{}, nil, fakeReloader, nil, nil, nil, nil, nil, nil, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

func TestListProviderHealthReportsStatusAndSignal(t *testing.T) {
	lastCheck := time.Now().Add(-5 * time.Second)
	reader := fakeHealthReader{snapshots: []health.ProviderHealth{
		{
			Provider:    "groq",
			Status:      health.StatusDegraded,
			ErrorRate:   0.25,
			P99Latency:  120 * time.Millisecond,
			LastCheckAt: lastCheck,
			LastTransition: &health.Transition{
				At: lastCheck, From: health.StatusHealthy, To: health.StatusDegraded, Reason: "error_rate_threshold",
			},
			History: []health.Transition{
				{At: lastCheck, From: health.StatusHealthy, To: health.StatusDegraded, Reason: "error_rate_threshold"},
			},
		},
	}}
	srv := newTestAdminServerWithHealth(t, reader)

	resp, err := http.Get(srv.URL + "/admin/providers/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var views []providerHealthView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d entries, want 1", len(views))
	}

	v := views[0]
	if v.Provider != "groq" {
		t.Errorf("Provider = %q, want groq", v.Provider)
	}
	if v.Status != "degraded" {
		t.Errorf("Status = %q, want degraded", v.Status)
	}
	if v.ErrorRate != 0.25 {
		t.Errorf("ErrorRate = %v, want 0.25", v.ErrorRate)
	}
	if v.P99LatencyMillis != 120 {
		t.Errorf("P99LatencyMillis = %v, want 120", v.P99LatencyMillis)
	}
	if v.LastCheckAt == nil {
		t.Fatalf("LastCheckAt = nil, want non-nil")
	}
	if v.LastTransition == nil || v.LastTransition.Reason != "error_rate_threshold" {
		t.Errorf("LastTransition = %+v, want reason error_rate_threshold", v.LastTransition)
	}
	if len(v.History) != 1 {
		t.Errorf("History = %+v, want one entry", v.History)
	}
}

// TestListProviderHealthOmitsZeroLastCheckAt proves a provider that has never
// been actively checked yet (LastCheckAt is the zero time) reports that
// absence as an omitted field, not a serialized "0001-01-01" that would read
// as a very stale timestamp instead of "no check yet."
func TestListProviderHealthOmitsZeroLastCheckAt(t *testing.T) {
	reader := fakeHealthReader{snapshots: []health.ProviderHealth{
		{Provider: "ollama", Status: health.StatusHealthy},
	}}
	srv := newTestAdminServerWithHealth(t, reader)

	resp, err := http.Get(srv.URL + "/admin/providers/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if strings.Contains(string(raw), "last_check_at") {
		t.Errorf("response included last_check_at for a provider never checked: %s", raw)
	}
	if strings.Contains(string(raw), "last_transition") {
		t.Errorf("response included last_transition for a provider with no history: %s", raw)
	}
}

// TestListProviderHealthEmptyIsAnEmptyArray proves the response is `[]`, not
// `null`, when nothing is tracked — a client decoding straight into a slice
// should never have to special-case null.
func TestListProviderHealthEmptyIsAnEmptyArray(t *testing.T) {
	srv := newTestAdminServerWithHealth(t, fakeHealthReader{})

	resp, err := http.Get(srv.URL + "/admin/providers/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("body = %q, want []", raw)
	}
}

// --- POST /admin/providers/{name}/breaker/reset (Step 7.4) -------------------

// newTestAdminServerWithBreakers builds a router whose provider list and
// breaker resetter are both caller-controlled, since the endpoint consults
// one to validate the name it passes to the other.
func newTestAdminServerWithBreakers(t *testing.T, providers ProviderLister, resetter BreakerController) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		testTeamStore(t), &fakeSpendReader{}, providers, fakeHealthReader{}, resetter, nil, fakeReloader, nil, nil, nil, nil, nil, nil, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

func configuredProviders() fakeProviderLister {
	return fakeProviderLister{configs: []provider.Config{{Name: "groq"}, {Name: "ollama"}}}
}

func TestResetBreakerClosesAProvidersBreakers(t *testing.T) {
	resetter := &fakeBreakerController{count: 2}
	srv := newTestAdminServerWithBreakers(t, configuredProviders(), resetter)

	resp, err := http.Post(srv.URL+"/admin/providers/groq/breaker/reset", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var view breakerResetView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if view.Provider != "groq" || view.Reset != 2 {
		t.Errorf("view = %+v, want groq with 2 reset", view)
	}

	if got := resetter.calls(); len(got) != 1 || got[0] != "groq" {
		t.Errorf("resetter called with %v, want exactly [groq]", got)
	}
}

// TestResetBreakerRejectsAnUnknownProvider proves a typo at incident time
// reads as an error rather than a no-op success.
func TestResetBreakerRejectsAnUnknownProvider(t *testing.T) {
	resetter := &fakeBreakerController{}
	srv := newTestAdminServerWithBreakers(t, configuredProviders(), resetter)

	resp, err := http.Post(srv.URL+"/admin/providers/grok/breaker/reset", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resetter.calls(); len(got) != 0 {
		t.Errorf("resetter was called with %v for an unknown provider, want no calls", got)
	}
}

// TestResetBreakerReportsASharedStateFailure proves a Redis failure during
// reset is surfaced rather than reported as a clean success — the local
// breakers were reset, but the rest of the fleet may still hold the episode.
func TestResetBreakerReportsASharedStateFailure(t *testing.T) {
	resetter := &fakeBreakerController{count: 1, err: errors.New("redis is down")}
	srv := newTestAdminServerWithBreakers(t, configuredProviders(), resetter)

	resp, err := http.Post(srv.URL+"/admin/providers/groq/breaker/reset", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// TestBreakerResetRouteDoesNotShadowTheHealthRoute guards the one routing
// hazard in mounting a wildcard under /admin/providers: "health" must keep
// reaching the health endpoint rather than being read as a provider name.
func TestBreakerResetRouteDoesNotShadowTheHealthRoute(t *testing.T) {
	srv := newTestAdminServerWithBreakers(t, configuredProviders(), &fakeBreakerController{})

	resp, err := http.Get(srv.URL + "/admin/providers/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the health endpoint", resp.StatusCode)
	}
}

// Step 2.4: /admin/providers/health also carries per-model breaker state,
// grouped by provider and sorted by model, so Overview reads health and
// breakers from one call.
func TestListProviderHealthIncludesBreakerStates(t *testing.T) {
	reader := fakeHealthReader{snapshots: []health.ProviderHealth{
		{Provider: "groq", Status: health.StatusHealthy},
		{Provider: "gemini", Status: health.StatusHealthy},
	}}
	breakers := &fakeBreakerController{snapshots: map[resilience.Labels]resilience.BreakerSnapshot{
		{Provider: "groq", Model: "openai/gpt-oss-20b"}:  {State: resilience.StateOpen, Failures: 5, FailureThreshold: 5, Cooldown: 30 * time.Second, CooldownRemaining: 12 * time.Second},
		{Provider: "groq", Model: "openai/gpt-oss-120b"}: {State: resilience.StateClosed, FailureThreshold: 5},
	}}
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, reader, breakers,
		nil, fakeReloader, nil, nil, nil, nil, nil, nil, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin/providers/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var views []providerHealthView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]providerHealthView{}
	for _, v := range views {
		byName[v.Provider] = v
	}

	groq := byName["groq"].Breakers
	if len(groq) != 2 {
		t.Fatalf("groq breakers = %+v, want 2", groq)
	}
	if groq[0].Model != "openai/gpt-oss-120b" || groq[0].State != "closed" {
		t.Errorf("breakers not sorted by model or wrong state: %+v", groq)
	}
	if groq[1].State != "open" {
		t.Errorf("second breaker state = %q, want open", groq[1].State)
	}
	if groq[1].Failures != 5 || groq[1].FailureThreshold != 5 {
		t.Errorf("open breaker failures = %d/%d, want 5/5", groq[1].Failures, groq[1].FailureThreshold)
	}
	if groq[1].CooldownRemaining != 12000 {
		t.Errorf("open breaker cooldown_remaining_ms = %v, want 12000", groq[1].CooldownRemaining)
	}
	if byName["gemini"].Breakers == nil {
		t.Error("a provider with no breakers should serialize [], not null")
	}
}
