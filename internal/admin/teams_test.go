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
	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/internal/provider"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testMetrics(t *testing.T) *telemetry.Metrics {
	t.Helper()
	m, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	return m
}

// testTeamStore builds a real *auth.Registry rather than a hand-rolled fake:
// Update's validation and its byHash/byID consistency are already covered by
// internal/auth's own tests, and re-deriving that logic in a fake here would
// only risk drifting from the real behavior these handlers actually run
// against.
// registryStore adapts an in-memory auth.Registry to the TeamStore interface,
// whose mutations take a context now that the real store writes to Postgres.
// These are handler tests: the registry's semantics are the right stand-in, and
// the context is simply not something an in-memory registry needs.
type registryStore struct{ reg *auth.Registry }

func (s registryStore) List() []auth.Team                { return s.reg.List() }
func (s registryStore) Get(id string) (auth.Team, error) { return s.reg.Get(id) }
func (s registryStore) Update(_ context.Context, id string, p auth.TeamPatch) (auth.Team, error) {
	return s.reg.Update(id, p)
}
func (s registryStore) RotateKey(_ context.Context, id, h, m string) (auth.Team, error) {
	return s.reg.RotateKey(id, h, m)
}
func (s registryStore) RevokeKey(_ context.Context, id string) (auth.Team, error) {
	return s.reg.RevokeKey(id)
}
func (s registryStore) Create(_ context.Context, t auth.Team) (auth.Team, error) {
	return t, s.reg.Add(t)
}
func (s registryStore) Delete(_ context.Context, id string) (auth.Team, error) {
	return s.reg.Remove(id)
}

// Not part of TeamStore — the tests that check a rotated key works use it
// directly, the same way the gateway's authenticator would.
func (s registryStore) Authenticate(k string) (*auth.Team, error) { return s.reg.Authenticate(k) }

func testTeamStore(t *testing.T) registryStore {
	t.Helper()
	r, err := auth.NewRegistry([]auth.Team{
		{
			ID: "acme", Name: "Acme Corp", KeyHash: auth.HashKey("acme-key"),
			AllowedProviders: []string{"groq"}, AllowedModels: []string{"m"},
			RateLimits: auth.RateLimits{RPM: 60, TPM: 100_000}, MonthlyBudgetMicros: 50_000_000,
			Priority: auth.PriorityRealtime, IsAdmin: true,
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
	return registryStore{reg: r}
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
	srv := httptest.NewServer(NewRouter(func() bool { return true }, teams, spend, providers, fakeHealthReader{}, &fakeBreakerController{}, nil, reload, nil, nil, nil, nil, nil, nil, QualityFeedbackConfig{}, false, nil, nil, nil, testMetrics(t), discardLogger()))
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
	if !v.IsAdmin {
		t.Error("is_admin = false on GET /admin/teams/{id}, want true — the Settings table needs this")
	}

	// Key metadata: a config-seeded team reports source "config", no mask (the
	// gateway never saw the plaintext), and no created-at. The hash appears
	// nowhere — Step 1's rule.
	if v.Key.Source != auth.KeySourceConfig {
		t.Errorf("key.source = %q, want %q", v.Key.Source, auth.KeySourceConfig)
	}
	if v.Key.Masked != "" || v.Key.CreatedAt != nil {
		t.Errorf("key = %+v, want no mask and no created-at for a config-seeded team", v.Key)
	}
	body, _ := json.Marshal(v)
	if strings.Contains(string(body), auth.HashKey("acme-key")) {
		t.Error("GET /admin/teams/{id} response contains the key hash")
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

// --- POST /admin/teams/{id}/key/rotate ------------------------------------

// The Step 1 headline: the plaintext key is returned exactly once, the old key
// dies immediately, the new one works with no restart, and no later read gives
// the plaintext back.
func TestRotateKeyReturnsPlaintextOnceThenMasked(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{spent: map[string]int64{}}, fakeProviderLister{})

	resp, err := http.Post(srv.URL+"/admin/teams/globex/key/rotate", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var rr rotateKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(rr.APIKey, "sk-switchyard-globex-") {
		t.Errorf("api_key = %q, want an sk-switchyard-globex- key", rr.APIKey)
	}
	if rr.Key.Source != auth.KeySourceRotated || rr.Key.CreatedAt == nil {
		t.Errorf("key metadata = %+v, want source rotated with a created-at", rr.Key)
	}
	if !strings.HasSuffix(rr.Key.Masked, rr.APIKey[len(rr.APIKey)-4:]) {
		t.Errorf("masked %q does not end in the key's last four", rr.Key.Masked)
	}

	// Old key rejected, new key accepted — immediately, against the same store
	// a real Auth middleware authenticates against.
	if _, err := store.Authenticate("globex-key"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("old key still authenticates after rotation: %v", err)
	}
	if _, err := store.Authenticate(rr.APIKey); err != nil {
		t.Errorf("rotated key does not authenticate: %v", err)
	}

	// A subsequent GET never carries the plaintext.
	getResp, err := http.Get(srv.URL + "/admin/teams/globex")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()
	body, _ := io.ReadAll(getResp.Body)
	if strings.Contains(string(body), rr.APIKey) {
		t.Error("GET /admin/teams/{id} returned the plaintext key after rotation")
	}
	var v teamView
	json.Unmarshal(body, &v)
	if v.Key.Source != auth.KeySourceRotated || v.Key.Masked != rr.Key.Masked {
		t.Errorf("GET key metadata = %+v, want rotated with mask %q", v.Key, rr.Key.Masked)
	}
}

func TestRotateKeyUnknownTeamIs404(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	resp, err := http.Post(srv.URL+"/admin/teams/nope/key/rotate", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// --- DELETE /admin/teams/{id}/key ----------------------------------------

func TestRevokeKeyRemovesAuthentication(t *testing.T) {
	store := testTeamStore(t)
	srv := newTestAdminServer(t, store, &fakeSpendReader{}, fakeProviderLister{})

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin/teams/globex/key", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var v teamView
	json.NewDecoder(resp.Body).Decode(&v)
	if v.Key.Source != auth.KeySourceRevoked {
		t.Errorf("key.source = %q, want revoked", v.Key.Source)
	}
	if _, err := store.Authenticate("globex-key"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("revoked team still authenticates: %v", err)
	}
}

// Revoking the key the request is authenticated with would lock the operator
// out; it must be refused. Needs a real authenticator, so it runs on the
// auth-wired server (acme = admin).
func TestRevokeOwnKeyIsRefused(t *testing.T) {
	srv := authedServer(t, &fakeSpendReader{})

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin/teams/acme/key", nil)
	req.Header.Set("Authorization", "Bearer acme-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// --- POST /admin/teams, DELETE /admin/teams/{id} (Tier 1, Step 2.5) --------

// failingAudit is an audit log that is down. Every team mutation writes its
// audit entry first, so this is how a test proves nothing happens without one.
type failingAudit struct{}

func (failingAudit) RecordAudit(context.Context, logstore.AuditEntry) error {
	return errors.New("audit log unavailable")
}

func (failingAudit) ListAudit(context.Context, int, string) (logstore.AuditPage, error) {
	return logstore.AuditPage{}, errors.New("audit log unavailable")
}

func newCreateDeleteServer(t *testing.T, teams TeamStore, audit AuditRecorder) *httptest.Server {
	t.Helper()
	providers := fakeProviderLister{configs: []provider.Config{{Name: "groq", Models: []string{"m", "m2"}}}}
	srv := httptest.NewServer(NewRouter(func() bool { return true }, teams, &fakeSpendReader{}, providers, fakeHealthReader{}, &fakeBreakerController{}, nil, fakeReloader, nil, nil, nil, nil, nil, nil, QualityFeedbackConfig{}, false, audit, nil, nil, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

// createBody is a valid create request with overrides applied on top.
func createBody(t *testing.T, overrides map[string]any) string {
	t.Helper()
	body := map[string]any{
		"name": "Initech Labs", "priority": "batch", "rpm": 30, "tpm": 1000,
		"monthly_budget_usd": 2.5, "allowed_providers": []string{"groq"}, "allowed_models": []string{"m"},
	}
	for k, v := range overrides {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func postCreateTeam(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/admin/teams", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /admin/teams: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func deleteTeamRequest(t *testing.T, srv *httptest.Server, id, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin/teams/"+id, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /admin/teams/%s: %v", id, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The headline: a created team's key is returned once and authenticates on the
// very next request, with no restart.
func TestCreateTeamReturnsKeyThatAuthenticates(t *testing.T) {
	store := testTeamStore(t)
	srv := newCreateDeleteServer(t, store, nil)

	resp := postCreateTeam(t, srv, createBody(t, nil))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	var cr createTeamResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cr.Team.ID != "initech-labs" {
		t.Errorf("team id = %q, want initech-labs derived from the name", cr.Team.ID)
	}
	if !strings.HasPrefix(cr.APIKey, "sk-switchyard-initech-labs-") {
		t.Errorf("api_key = %q, want an sk-switchyard-initech-labs- key", cr.APIKey)
	}
	if cr.Key.Source != auth.KeySourceCreated {
		t.Errorf("key.source = %q, want %q", cr.Key.Source, auth.KeySourceCreated)
	}

	team, err := store.Authenticate(cr.APIKey)
	if err != nil {
		t.Fatalf("new key does not authenticate: %v", err)
	}
	if team.MonthlyBudgetMicros != 2_500_000 || team.Priority != auth.PriorityBatch {
		t.Errorf("stored budget/priority = %d/%q, want 2500000/batch", team.MonthlyBudgetMicros, team.Priority)
	}
}

// Every rejection happens before anything is written: the team count is
// unchanged whatever the reason.
func TestCreateTeamRejectsInvalidRequests(t *testing.T) {
	tests := map[string]struct {
		overrides map[string]any
		want      int
	}{
		"name slugs to an existing id": {map[string]any{"name": "Acme"}, http.StatusConflict},
		"unknown model":                {map[string]any{"allowed_models": []string{"gpt-9"}}, http.StatusBadRequest},
		"unknown provider":             {map[string]any{"allowed_providers": []string{"openai"}}, http.StatusBadRequest},
		"name with no ASCII letters":   {map[string]any{"name": "!!!"}, http.StatusBadRequest},
		"zero rpm":                     {map[string]any{"rpm": 0}, http.StatusBadRequest},
		"unknown priority":             {map[string]any{"priority": "urgent"}, http.StatusBadRequest},
		"caller-chosen id":             {map[string]any{"id": "chosen"}, http.StatusBadRequest},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			store := testTeamStore(t)
			srv := newCreateDeleteServer(t, store, nil)

			resp := postCreateTeam(t, srv, createBody(t, tc.overrides))
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if n := len(store.List()); n != 2 {
				t.Errorf("team count = %d after a rejected create, want 2", n)
			}
		})
	}
}

func TestCreateTeamAuditFailureCreatesNothing(t *testing.T) {
	store := testTeamStore(t)
	srv := newCreateDeleteServer(t, store, failingAudit{})

	resp := postCreateTeam(t, srv, createBody(t, nil))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if _, err := store.Get("initech-labs"); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Errorf("team was created despite a failed audit write: %v", err)
	}
}

func TestDeleteTeamRemovesAuthentication(t *testing.T) {
	store := testTeamStore(t)
	srv := newCreateDeleteServer(t, store, nil)

	resp := deleteTeamRequest(t, srv, "globex", "")
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204: %s", resp.StatusCode, body)
	}
	if _, err := store.Authenticate("globex-key"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("deleted team's key still authenticates: %v", err)
	}
	if _, err := store.Get("globex"); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Errorf("deleted team is still listed: %v", err)
	}
}

// Deleting the team you are authenticated as would lock you out, and refusing
// it is what guarantees an admin always remains. Needs a real authenticator, so
// it runs on the auth-wired server (acme = admin).
func TestDeleteOwnTeamIsRefused(t *testing.T) {
	srv := authedServer(t, &fakeSpendReader{})

	resp := deleteTeamRequest(t, srv, "acme", "acme-key")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestDeleteTeamAuditFailureDeletesNothing(t *testing.T) {
	store := testTeamStore(t)
	srv := newCreateDeleteServer(t, store, failingAudit{})

	resp := deleteTeamRequest(t, srv, "globex", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if _, err := store.Authenticate("globex-key"); err != nil {
		t.Errorf("team was deleted despite a failed audit write: %v", err)
	}
}
