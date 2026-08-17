package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/health"
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

// newTestAdminServerWithHealth builds a router with a caller-controlled
// HealthReader and permissive defaults for everything else this file's tests
// don't care about — the same shape newTestAdminServerWithReload gives
// reload_test.go for the piece it does care about.
func newTestAdminServerWithHealth(t *testing.T, reader HealthReader) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, reader, fakeReloader, discardLogger()))
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
