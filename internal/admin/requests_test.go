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
// tests assert on scoping: what matters is the TeamIDs the handler resolved,
// not what a stub database would return for it.
type fakeRequestLogReader struct {
	qualityFeedback    logstore.QualityFeedback
	qualityFeedbackErr error
	gotFilter          logstore.Filter
	gotID              string
	gotTeamIDs         []string
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

func (f *fakeRequestLogReader) Get(_ context.Context, id string, teamIDs []string) (logstore.Record, error) {
	f.gotID, f.gotTeamIDs = id, teamIDs
	return f.record, f.getErr
}

func (f *fakeRequestLogReader) SpendByTeamSince(_ context.Context, _ time.Time) (map[string]int64, error) {
	return f.spendByTeam, f.spendErr
}

func (f *fakeRequestLogReader) CostSeries(_ context.Context, q logstore.CostQuery) ([]logstore.CostCell, error) {
	f.gotCostQuery = q
	return f.costCells, f.costErr
}

func (f *fakeRequestLogReader) CacheSavingsSince(_ context.Context, _ time.Time, teamIDs []string) (logstore.CacheSavings, error) {
	f.gotTeamIDs = teamIDs
	return f.cacheSavings, f.cacheSavingsErr
}

func (f *fakeRequestLogReader) RoutingSavingsSince(_ context.Context, _ time.Time, teamIDs []string) (logstore.RoutingSavings, error) {
	f.gotTeamIDs = teamIDs
	return f.routingSavings, f.routingSavingsErr
}

func (f *fakeRequestLogReader) FallbackCostSince(_ context.Context, _ time.Time, teamIDs []string) (logstore.FallbackAttribution, error) {
	f.gotTeamIDs = teamIDs
	return f.fallbackAttr, f.fallbackErr
}

func (f *fakeRequestLogReader) QualityFeedbackSince(_ context.Context, _ time.Time, teamIDs []string, _ float64, _ int) (logstore.QualityFeedback, error) {
	f.gotTeamIDs = teamIDs
	return f.qualityFeedback, f.qualityFeedbackErr
}

// requestLogRegistry mirrors testTeamStore but marks acme as admin, which is
// the distinction every scoping test in this file turns on. Like
// testTeamStore, acme sits in "personal" (testAuth's non-superadmin org) and
// globex sits elsewhere, so org-scoping tests have something real to assert.
func requestLogRegistry(t *testing.T) *auth.Registry {
	t.Helper()
	r, err := auth.NewRegistry([]auth.Team{
		{
			ID: "acme", Name: "Acme Corp", OrganizationID: "personal", KeyHash: auth.HashKey("acme-key"),
			AllowedProviders: []string{"groq"}, AllowedModels: []string{"m"},
			RateLimits: auth.RateLimits{RPM: 60, TPM: 100_000}, MonthlyBudgetMicros: 50_000_000,
			Priority: auth.PriorityRealtime, IsAdmin: true,
		},
		{
			ID: "globex", Name: "Globex Inc", OrganizationID: "other-org", KeyHash: auth.HashKey("globex-key"),
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

// Step 2.2: a non-superadmin is no longer flatly refused. They are scoped to
// every team in their own organisation -- here, exactly "acme" -- never to
// globex, which sits in a different org, and never to every team the way a
// superadmin is.
func TestRequestLogScopesNonSuperadminToOwnOrg(t *testing.T) {
	reader := &fakeRequestLogReader{}
	srv := newRequestLogServerAs(t, reader, testAuth(false))

	if resp := get(t, srv, "/admin/requests"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if want := []string{"acme"}; !slicesEqual(reader.gotFilter.TeamIDs, want) {
		t.Errorf("filter team ids = %v, want %v", reader.gotFilter.TeamIDs, want)
	}

	// A client-supplied ?team= is not an escape hatch: the org's own team list
	// is what decides scope, not the query string.
	if resp := get(t, srv, "/admin/requests?team=globex"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if want := []string{"acme"}; !slicesEqual(reader.gotFilter.TeamIDs, want) {
		t.Errorf("filter team ids = %v, want %v (ignoring the client-supplied ?team=)", reader.gotFilter.TeamIDs, want)
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
	if reader.gotFilter.TeamIDs != nil {
		t.Errorf("admin filter team ids = %v, want nil (all teams)", reader.gotFilter.TeamIDs)
	}

	if resp := get(t, srv, "/admin/requests?team=globex"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if want := []string{"globex"}; !slicesEqual(reader.gotFilter.TeamIDs, want) {
		t.Errorf("admin filter team ids = %v, want %v", reader.gotFilter.TeamIDs, want)
	}
}

// A superadmin reads any row; the scope passed to the store is nil (no filter).
func TestGetRequestSuperadminScope(t *testing.T) {
	reader := &fakeRequestLogReader{getErr: logstore.ErrNotFound}
	srv := newRequestLogServer(t, reader)

	if resp := get(t, srv, "/admin/requests/missing-id"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	reader.getErr = nil
	get(t, srv, "/admin/requests/any-id")
	if reader.gotTeamIDs != nil {
		t.Errorf("superadmin lookup scope = %v, want nil (any team)", reader.gotTeamIDs)
	}
}

// A non-superadmin reading another org's row by id gets 404, the same answer
// as a genuinely missing row -- never 403, which would confirm the row exists.
func TestGetRequestOutsideOrgIsNotFound(t *testing.T) {
	reader := &fakeRequestLogReader{getErr: logstore.ErrNotFound}
	srv := newRequestLogServerAs(t, reader, testAuth(false))

	resp := get(t, srv, "/admin/requests/globex-row")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if want := []string{"acme"}; !slicesEqual(reader.gotTeamIDs, want) {
		t.Errorf("lookup scope = %v, want %v", reader.gotTeamIDs, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
