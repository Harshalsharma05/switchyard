package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

// fakeRequestLogReader records the filter it was handed, which is how these
// tests assert on scoping: what matters is the TeamID the handler resolved,
// not what a stub database would return for it.
type fakeRequestLogReader struct {
	qualityFeedback    logstore.QualityFeedback
	qualityFeedbackErr error
	gotFilter          logstore.Filter
	gotID              string
	gotTeamID          string
	page               logstore.Page
	record             logstore.Record
	getErr             error
	spendByTeam        map[string]int64
	spendErr           error

	gotCostQuery logstore.CostQuery
	costCells    []logstore.CostCell
	costErr      error

	fallbackAttr logstore.FallbackAttribution
	fallbackErr  error

	cacheSavings      logstore.CacheSavings
	routingSavings    logstore.RoutingSavings
	routingSavingsErr error
	cacheSavingsErr   error
}

func (f *fakeRequestLogReader) Query(_ context.Context, filter logstore.Filter) (logstore.Page, error) {
	f.gotFilter = filter
	return f.page, nil
}

func (f *fakeRequestLogReader) Get(_ context.Context, id, teamID string) (logstore.Record, error) {
	f.gotID, f.gotTeamID = id, teamID
	return f.record, f.getErr
}

func (f *fakeRequestLogReader) SpendByTeamSince(_ context.Context, _ time.Time) (map[string]int64, error) {
	return f.spendByTeam, f.spendErr
}

func (f *fakeRequestLogReader) CostSeries(_ context.Context, q logstore.CostQuery) ([]logstore.CostCell, error) {
	f.gotCostQuery = q
	return f.costCells, f.costErr
}

func (f *fakeRequestLogReader) CacheSavingsSince(_ context.Context, _ time.Time, teamID string) (logstore.CacheSavings, error) {
	f.gotTeamID = teamID
	return f.cacheSavings, f.cacheSavingsErr
}

func (f *fakeRequestLogReader) RoutingSavingsSince(_ context.Context, _ time.Time, teamID string) (logstore.RoutingSavings, error) {
	f.gotTeamID = teamID
	return f.routingSavings, f.routingSavingsErr
}

func (f *fakeRequestLogReader) FallbackCostSince(_ context.Context, _ time.Time, teamID string) (logstore.FallbackAttribution, error) {
	f.gotTeamID = teamID
	return f.fallbackAttr, f.fallbackErr
}

func (f *fakeRequestLogReader) QualityFeedbackSince(_ context.Context, _ time.Time, teamID string, _ float64, _ int) (logstore.QualityFeedback, error) {
	f.gotTeamID = teamID
	return f.qualityFeedback, f.qualityFeedbackErr
}

// requestLogRegistry mirrors testTeamStore but marks acme as admin, which is
// the distinction every scoping test in this file turns on.
func requestLogRegistry(t *testing.T) *auth.Registry {
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
	return r
}

func newRequestLogServer(t *testing.T, reader RequestLogReader) *httptest.Server {
	return newRequestLogServerAs(t, reader, testAuth(true))
}

func newRequestLogServerAs(t *testing.T, reader RequestLogReader, auth Auth) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true },
		testTeamStore(t), &fakeSpendReader{}, fakeProviderLister{}, fakeHealthReader{},
		&fakeBreakerController{}, nil, fakeReloader, reader,
		nil, nil, nil, QualityFeedbackConfig{}, false, nil, nil, auth, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

func getWithKey(t *testing.T, srv *httptest.Server, path, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The fail-closed rule from Step 1.5: these views have no organisation filter
// yet, so anyone who is not a superadmin is refused rather than served
// unscoped. Phase 2, Step 2.2 must deliberately replace this.
func TestRequestLogRefusesNonSuperadmin(t *testing.T) {
	reader := &fakeRequestLogReader{}
	srv := newRequestLogServerAs(t, reader, testAuth(false))

	for _, path := range []string{"/admin/requests", "/admin/requests?team=acme", "/admin/requests/any-id"} {
		if resp := get(t, srv, path); resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s: status = %d, want 403", path, resp.StatusCode)
		}
	}
	if reader.gotFilter.TeamID != "" || reader.gotTeamID != "" {
		t.Error("a refused request still reached the database")
	}
}

// A superadmin may look across teams, and may narrow to one -- unchanged from
// what an admin key did.
func TestListRequestsSuperadminSeesAcrossTeams(t *testing.T) {
	reader := &fakeRequestLogReader{}
	srv := newRequestLogServer(t, reader)

	if resp := get(t, srv, "/admin/requests"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if reader.gotFilter.TeamID != "" {
		t.Errorf("admin filter team = %q, want empty (all teams)", reader.gotFilter.TeamID)
	}

	if resp := get(t, srv, "/admin/requests?team=globex"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if reader.gotFilter.TeamID != "globex" {
		t.Errorf("admin filter team = %q, want %q", reader.gotFilter.TeamID, "globex")
	}
}

// A superadmin reads any row; the scope passed to the store is empty.
func TestGetRequestSuperadminScope(t *testing.T) {
	reader := &fakeRequestLogReader{getErr: logstore.ErrNotFound}
	srv := newRequestLogServer(t, reader)

	if resp := get(t, srv, "/admin/requests/missing-id"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	reader.getErr = nil
	get(t, srv, "/admin/requests/any-id")
	if reader.gotTeamID != "" {
		t.Errorf("superadmin lookup scope = %q, want empty (any team)", reader.gotTeamID)
	}
}

// No session at all -- the state a request arrives in when the cookie is
// missing or the middleware is not wired.
func TestRequestLogRequiresASession(t *testing.T) {
	srv := newRequestLogServerAs(t, &fakeRequestLogReader{}, Auth{})
	if resp := get(t, srv, "/admin/requests"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Filters must reach the query layer as typed values, not be silently dropped.
func TestListRequestsParsesFilters(t *testing.T) {
	reader := &fakeRequestLogReader{}
	srv := newRequestLogServer(t, reader)

	resp := getWithKey(t, srv,
		"/admin/requests?provider=groq&model=gpt-oss&status=4xx&fallback=true&limit=10&since=24h",
		"acme-key")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	f := reader.gotFilter
	if f.Provider != "groq" || f.Model != "gpt-oss" {
		t.Errorf("provider/model = %q/%q", f.Provider, f.Model)
	}
	if f.StatusMin != 400 || f.StatusMax != 499 {
		t.Errorf("status range = %d-%d, want 400-499", f.StatusMin, f.StatusMax)
	}
	if f.Fallback == nil || !*f.Fallback {
		t.Error("fallback filter did not reach the query")
	}
	if f.Limit != 10 {
		t.Errorf("limit = %d, want 10", f.Limit)
	}
	if f.Since.IsZero() {
		t.Error("since=24h did not resolve to a timestamp")
	}
}

// The request log being unconfigured must read as a stated 503, not a crash or
// an empty list that looks like "no traffic yet".
func TestRequestLogDisabledReports503(t *testing.T) {
	srv := newRequestLogServer(t, nil)

	resp := get(t, srv, "/admin/requests")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	if body.Error.Type != "request_log_disabled" {
		t.Errorf("error type = %q, want %q", body.Error.Type, "request_log_disabled")
	}
}

// testAuth wires the router the way cmd/ does, minus identity: a middleware
// that attaches a caller. Superadmin false is the fail-closed path that Phase 2
// replaces with real organisation scoping.
func testAuth(superadmin bool) Auth {
	return Auth{Session: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := Caller{UserID: "usr_test", OrgID: "personal", IsSuperadmin: superadmin, SessionID: "ses_test"}
			next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), c)))
		})
	}}
}

// get issues a signed-in GET. The admin port no longer takes a key at all, so
// there is nothing to pass: the session comes from testAuth.
func get(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
