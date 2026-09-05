package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// authedServer wires a real registry (acme = admin, globex = not) as the
// authenticator, so the /admin/me identity payload and the requireAdmin gate
// run against the same logic cmd/gateway uses.
func authedServer(t *testing.T, spend SpendReader) *httptest.Server {
	t.Helper()
	reg := requestLogRegistry(t)
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		reg, spend, fakeProviderLister{}, fakeHealthReader{}, &fakeBreakerController{},
		nil, fakeReloader, nil, reg, nil, nil, nil, nil, QualityFeedbackConfig{}, false, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

func TestMeReturnsCallersOwnIdentity(t *testing.T) {
	spend := &fakeSpendReader{spent: map[string]int64{"globex": 1_000_000}}
	srv := authedServer(t, spend)

	resp := getWithKey(t, srv, "/admin/me", "globex-key")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var v meView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.ID != "globex" || v.IsAdmin {
		t.Errorf("got id=%q is_admin=%v, want globex/false", v.ID, v.IsAdmin)
	}
	if v.SpentUSD == nil || *v.SpentUSD != 1.0 {
		t.Errorf("SpentUSD = %v, want 1.0", v.SpentUSD)
	}
	if v.RateLimits.RPM != 10 {
		t.Errorf("RateLimits.RPM = %d, want 10", v.RateLimits.RPM)
	}
}

func TestMeAdminFlag(t *testing.T) {
	srv := authedServer(t, &fakeSpendReader{})
	resp := getWithKey(t, srv, "/admin/me", "acme-key")
	var v meView
	_ = json.NewDecoder(resp.Body).Decode(&v)
	if !v.IsAdmin {
		t.Errorf("acme is_admin = false, want true")
	}
}

func TestMeRejectsBadKeys(t *testing.T) {
	srv := authedServer(t, &fakeSpendReader{})
	for name, key := range map[string]string{"missing": "", "unknown": "nope"} {
		t.Run(name, func(t *testing.T) {
			if got := getWithKey(t, srv, "/admin/me", key).StatusCode; got != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", got)
			}
		})
	}
}

// The Step 2.1 headline: a valid non-admin key is 403 on a gated route, an
// admin key passes, and no key is 401 — verified outside the UI.
func TestRequireAdminGate(t *testing.T) {
	srv := authedServer(t, &fakeSpendReader{})
	cases := map[string]struct {
		key  string
		want int
	}{
		"non-admin key": {"globex-key", http.StatusForbidden},
		"admin key":     {"acme-key", http.StatusOK},
		"no key":        {"", http.StatusUnauthorized},
		"unknown key":   {"nope", http.StatusUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := getWithKey(t, srv, "/admin/teams", tc.key).StatusCode; got != tc.want {
				t.Errorf("GET /admin/teams: status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRequireAdminGateCoversChaos(t *testing.T) {
	srv := authedServer(t, &fakeSpendReader{})
	if got := getWithKey(t, srv, "/admin/chaos", "globex-key").StatusCode; got != http.StatusForbidden {
		t.Errorf("GET /admin/chaos as non-admin: status = %d, want 403", got)
	}
}
