package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeSystemReporter struct {
	deps map[string]string
}

func (f fakeSystemReporter) Version() string       { return "test-1.2.3" }
func (f fakeSystemReporter) Uptime() time.Duration { return 90 * time.Second }
func (f fakeSystemReporter) ConfigHash() string    { return "abc123def456" }
func (f fakeSystemReporter) Dependencies(context.Context) map[string]string {
	return f.deps
}

func systemServer(t *testing.T, sys SystemReporter) *httptest.Server {
	t.Helper()
	reg := requestLogRegistry(t) // acme = admin, globex = not
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		registryStore{reg: reg}, &fakeSpendReader{}, configuredProviders(), fakeHealthReader{}, &fakeBreakerController{},
		nil, fakeReloader, nil, reg, nil, nil, nil, nil, QualityFeedbackConfig{}, false, nil, sys,
		nil, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

func TestSystemReturnsProcessInfo(t *testing.T) {
	sys := fakeSystemReporter{deps: map[string]string{
		"redis": "up", "postgres": "up", "prometheus": "down",
	}}
	srv := systemServer(t, sys)

	resp := getWithKey(t, srv, "/admin/system", "acme-key")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var v systemView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Version != "test-1.2.3" || v.UptimeSeconds != 90 || v.ConfigHash != "abc123def456" {
		t.Errorf("view = %+v, want version/uptime/hash filled", v)
	}
	if v.Dependencies["prometheus"] != "down" || v.Dependencies["redis"] != "up" {
		t.Errorf("dependencies = %v, want the reporter's values passed through", v.Dependencies)
	}
	// Provider list comes from the ProviderLister, not the reporter.
	if len(v.Providers) == 0 {
		t.Errorf("providers empty, want the fakeProviderLister's entries")
	}
}

// The Settings screen's real enforcement: a non-admin key never gets past the
// gate, whatever the UI shows.
func TestSystemIsAdminOnly(t *testing.T) {
	srv := systemServer(t, fakeSystemReporter{deps: map[string]string{}})

	if got := getWithKey(t, srv, "/admin/system", "globex-key").StatusCode; got != http.StatusForbidden {
		t.Errorf("non-admin GET /admin/system: status = %d, want 403", got)
	}
	if got := getWithKey(t, srv, "/admin/system", "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("no-key GET /admin/system: status = %d, want 401", got)
	}
}
