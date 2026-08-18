package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// testTeamStore builds a real *auth.Registry rather than a hand-rolled fake:
// Update's validation and its byHash/byID consistency are already covered by
// internal/auth's own tests, and re-deriving that logic in a fake here would
// only risk drifting from the real behavior these handlers actually run
// against.
func testTeamStore(t *testing.T) *auth.Registry {
	t.Helper()
	r, err := auth.NewRegistry([]auth.Team{
		{
			ID: "acme", Name: "Acme Corp", KeyHash: auth.HashKey("acme-key"),
			AllowedProviders: []string{"groq"}, AllowedModels: []string{"m"},
			RateLimits: auth.RateLimits{RPM: 60, TPM: 100_000}, MonthlyBudgetMicros: 50_000_000,
			Priority: auth.PriorityRealtime,
		},
		{
			ID: "globex", Name: "Globex Inc", KeyHash: auth.HashKey("globex-key"),
			AllowedProviders: []string{"groq"}, AllowedModels: []string{"m"},
			RateLimits: auth.RateLimits{RPM: 10, TPM: 20_000}, MonthlyBudgetMicros: 5_000_000,
			Priority: auth.PriorityBatch,
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// fakeSpendReader is the fake behind SpendReader — budget.Tracker needs real
// Redis, so a fake is what lets these handler tests run without it and
// simulate a Redis failure on demand.
type fakeSpendReader struct {
	spent      map[string]int64
	spentErr   error
	resetErr   error
	resetCalls []string
}

func (f *fakeSpendReader) Spent(_ context.Context, teamID string) (int64, error) {
	if f.spentErr != nil {
		return 0, f.spentErr
	}
	return f.spent[teamID], nil
}

func (f *fakeSpendReader) Reset(_ context.Context, teamID string) error {
	if f.resetErr != nil {
		return f.resetErr
	}
	f.resetCalls = append(f.resetCalls, teamID)
	if f.spent != nil {
		delete(f.spent, teamID)
	}
	return nil
}

// fakeReloader is the permissive default behind Reloader for every test that
// is not specifically about reload — it succeeds without touching anything,
// mirroring stubRateLimiter's role in the proxy package's own tests.
func fakeReloader(context.Context) (ReloadSummary, error) {
	return ReloadSummary{}, nil
}

func newTestAdminServer(t *testing.T, teams TeamStore, spend SpendReader, providers ProviderLister) *httptest.Server {
	t.Helper()
	return newTestAdminServerWithReload(t, teams, spend, providers, fakeReloader)
}

func newTestAdminServerWithReload(t *testing.T, teams TeamStore, spend SpendReader, providers ProviderLister, reload Reloader) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true }, teams, spend, providers, fakeHealthReader{}, &fakeBreakerResetter{}, nil, reload, discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

// --- GET /admin/teams -----------------------------------------------------

func TestListTeamsIncludesSpend(t *testing.T) {
	store := testTeamStore(t)
	spend := &fakeSpendReader{spent: map[string]int64{"acme": 12_340_000, "globex": 1_000_000}}
	srv := newTestAdminServer(t, store, spend, fakeProviderLister{})

	resp, err := http.Get(srv.URL + "/admin/teams")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var views []teamView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d teams, want 2", len(views))
	}

	byID := make(map[string]teamView, len(views))
	for _, v := range views {
		byID[v.ID] = v
	}

	acme := byID["acme"]
	if acme.SpentUSD == nil || *acme.SpentUSD != 12.34 {
		t.Errorf("acme SpentUSD = %v, want 12.34", acme.SpentUSD)
	}
	if acme.MonthlyBudgetUSD != 50.00 {
		t.Errorf("acme MonthlyBudgetUSD = %v, want 50.00", acme.MonthlyBudgetUSD)
	}
	if acme.BudgetUtilization == nil || *acme.BudgetUtilization < 0.246 || *acme.BudgetUtilization > 0.247 {
		t.Errorf("acme BudgetUtilization = %v, want ~0.2468", acme.BudgetUtilization)
	}
	if acme.RateLimits.RPM != 60 || acme.RateLimits.TPM != 100_000 {
		t.Errorf("acme RateLimits = %+v, want {60 100000}", acme.RateLimits)
	}
}

// A Redis failure reading one team's spend must not take down the whole
// listing, and must report null rather than a misleading 0.
func TestListTeamsSpendErrorYieldsNullNotZero(t *testing.T) {
	store := testTeamStore(t)
	spend := &fakeSpendReader{spentErr: errors.New("redis unreachable")}
	srv := newTestAdminServer(t, store, spend, fakeProviderLister{})

	resp, err := http.Get(srv.URL + "/admin/teams")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a spend read failure must not fail the listing", resp.StatusCode)
	}

	var views []teamView
	json.NewDecoder(resp.Body).Decode(&views)
	for _, v := range views {
		if v.SpentUSD != nil {
			t.Errorf("team %q SpentUSD = %v, want nil (unknown, not a real 0)", v.ID, *v.SpentUSD)
		}
		if v.BudgetUtilization != nil {
			t.Errorf("team %q BudgetUtilization = %v, want nil", v.ID, *v.BudgetUtilization)
		}
	}
}

// --- GET /admin/teams/{id} -------------------------------------------------

func TestGetTeamSuccess(t *testing.T) {
	store := testTeamStore(t)
	spend := &fakeSpendReader{spent: map[string]int64{"acme": 0}}
	srv := newTestAdminServer(t, store, spend, fakeProviderLister{})

	resp, err := http.Get(srv.URL + "/admin/teams/acme")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var v teamView
	json.NewDecoder(resp.Body).Decode(&v)
	if v.ID != "acme" || v.Name != "Acme Corp" {
		t.Errorf("v = %+v, want acme/Acme Corp", v)
	}
}

func TestGetTeamUnknownIs404(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp, err := http.Get(srv.URL + "/admin/teams/nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	var body errorBody
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Type != "team_not_found" {
		t.Errorf("error.type = %q, want team_not_found", body.Error.Type)
	}
}

// --- PATCH /admin/teams/{id} -----------------------------------------------

func patch(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	return resp
}

func TestPatchTeamAppliesOnlyGivenFields(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp := patch(t, srv, "/admin/teams/acme", `{"rpm": 120}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var v teamView
	json.NewDecoder(resp.Body).Decode(&v)
	if v.RateLimits.RPM != 120 {
		t.Errorf("RPM = %d, want 120", v.RateLimits.RPM)
	}
	if v.RateLimits.TPM != 100_000 {
		t.Errorf("TPM = %d, want 100000 (unset field must not change)", v.RateLimits.TPM)
	}

	// The change must be live for the very next request — no restart, per
	// Step 4.3's checklist — proven by reading straight from the same store
	// a real Auth middleware would authenticate against.
	got, err := store.Get("acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RateLimits.RPM != 120 {
		t.Errorf("store.Get(acme).RPM = %d after PATCH, want 120", got.RateLimits.RPM)
	}
}

func TestPatchTeamBudgetConvertsUSDToMicros(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp := patch(t, srv, "/admin/teams/acme", `{"monthly_budget_usd": 75.50}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	got, err := store.Get("acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.MonthlyBudgetMicros != 75_500_000 {
		t.Errorf("MonthlyBudgetMicros = %d, want 75500000", got.MonthlyBudgetMicros)
	}
}

func TestPatchTeamUnknownTeamIs404(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp := patch(t, srv, "/admin/teams/nope", `{"rpm": 10}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPatchTeamInvalidValueIs400(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp := patch(t, srv, "/admin/teams/acme", `{"rpm": 0}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	// Must not have partially applied.
	got, err := store.Get("acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RateLimits.RPM != 60 {
		t.Errorf("RPM = %d after a rejected PATCH, want unchanged 60", got.RateLimits.RPM)
	}
}

func TestPatchTeamMalformedJSONIs400(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp := patch(t, srv, "/admin/teams/acme", `{nope`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPatchTeamRejectsUnknownField(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp := patch(t, srv, "/admin/teams/acme", `{"rmp": 10}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a typo'd field must be rejected, not silently ignored", resp.StatusCode)
	}
}

// --- POST /admin/teams/{id}/reset-budget -----------------------------------

func TestResetBudgetClearsSpend(t *testing.T) {
	store := testTeamStore(t)
	spend := &fakeSpendReader{spent: map[string]int64{"acme": 40_000_000}}
	srv := newTestAdminServer(t, store, spend, fakeProviderLister{})

	resp, err := http.Post(srv.URL+"/admin/teams/acme/reset-budget", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var v teamView
	json.NewDecoder(resp.Body).Decode(&v)
	if v.SpentUSD == nil || *v.SpentUSD != 0 {
		t.Errorf("SpentUSD after reset = %v, want 0", v.SpentUSD)
	}

	if len(spend.resetCalls) != 1 || spend.resetCalls[0] != "acme" {
		t.Errorf("resetCalls = %v, want [acme]", spend.resetCalls)
	}
}

func TestResetBudgetUnknownTeamIs404(t *testing.T) {
	store := testTeamStore(t)
	spend := &fakeSpendReader{}
	srv := newTestAdminServer(t, store, spend, fakeProviderLister{})

	resp, err := http.Post(srv.URL+"/admin/teams/nope/reset-budget", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if len(spend.resetCalls) != 0 {
		t.Errorf("Reset was called for an unknown team: %v", spend.resetCalls)
	}
}

func TestResetBudgetFailureIs503(t *testing.T) {
	store := testTeamStore(t)
	spend := &fakeSpendReader{resetErr: errors.New("redis unreachable")}
	srv := newTestAdminServer(t, store, spend, fakeProviderLister{})

	resp, err := http.Post(srv.URL+"/admin/teams/acme/reset-budget", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
