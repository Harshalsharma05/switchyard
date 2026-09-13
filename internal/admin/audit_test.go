package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// recordingAudit captures what was written and how the listing was scoped,
// which is the wiring Step 2.5 changed: a handler could record the right entry
// and still read the log back unscoped.
type recordingAudit struct {
	written []logstore.AuditEntry
	query   logstore.AuditQuery
	page    logstore.AuditPage
}

func (a *recordingAudit) RecordAudit(_ context.Context, e logstore.AuditEntry) error {
	a.written = append(a.written, e)
	return nil
}

func (a *recordingAudit) ListAudit(_ context.Context, q logstore.AuditQuery) (logstore.AuditPage, error) {
	a.query = q
	return a.page, nil
}

func auditServer(t *testing.T, teams TeamStore, audit AuditRecorder, auth Auth) *httptest.Server {
	t.Helper()
	providers := fakeProviderLister{configs: []provider.Config{{Name: "groq", Models: []string{"m", "m2"}}}}
	srv := httptest.NewServer(NewRouter(func() bool { return true }, teams, &fakeSpendReader{}, providers,
		fakeHealthReader{}, &fakeBreakerController{}, nil, fakeReloader, nil, nil, nil, nil,
		QualityFeedbackConfig{}, false, audit, nil, auth, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

// An entry attributes the human who acted, files itself under the organisation
// of the team that changed, and records whether an operator did it.
func TestAuditEntryAttributesUserAndOrg(t *testing.T) {
	cases := map[string]struct {
		superadmin     bool
		team           string
		wantOrg        string
		wantSuperadmin bool
	}{
		// acme is in "personal", the org testAuth's caller belongs to.
		"an org admin changing its own team": {false, "acme", "personal", false},
		// globex is in "other-org": the entry is filed under the organisation
		// that was affected, not the superadmin's own.
		"a superadmin reaching another org": {true, "globex", "other-org", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			audit := &recordingAudit{}
			srv := auditServer(t, testTeamStore(t), audit, testAuth(tc.superadmin))

			resp := doRequest(t, srv, http.MethodPatch, "/admin/teams/"+tc.team, `{"rpm":42}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if len(audit.written) != 1 {
				t.Fatalf("wrote %d audit entries, want 1", len(audit.written))
			}

			e := audit.written[0]
			if e.ActorUserID != "usr_test" {
				t.Errorf("actor = %q, want usr_test (a user, not a team)", e.ActorUserID)
			}
			if e.OrganizationID != tc.wantOrg {
				t.Errorf("organization = %q, want %q", e.OrganizationID, tc.wantOrg)
			}
			if e.Superadmin != tc.wantSuperadmin {
				t.Errorf("superadmin = %v, want %v", e.Superadmin, tc.wantSuperadmin)
			}
			if e.TargetTeamID != tc.team {
				t.Errorf("target = %q, want %q", e.TargetTeamID, tc.team)
			}
		})
	}
}

// A config reload targets no team, so it must be filed against no organisation
// at all. That NULL is the only thing keeping it out of every tenant's view now
// that the superadmin flag no longer gates visibility.
func TestReloadAuditBelongsToNoTenant(t *testing.T) {
	audit := &recordingAudit{}
	srv := auditServer(t, testTeamStore(t), audit, testAuth(true))

	resp := doRequest(t, srv, http.MethodPost, "/admin/reload", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(audit.written) != 1 {
		t.Fatalf("wrote %d entries, want 1", len(audit.written))
	}
	e := audit.written[0]
	if e.OrganizationID != "" {
		t.Errorf("organization = %q, want none: a reload belongs to no tenant", e.OrganizationID)
	}
	if !e.Superadmin {
		t.Error("a reload must be recorded as a superadmin action")
	}
}

// The listing's scope comes from the session: an org admin's read is restricted
// to their organisation, a superadmin's is not.
func TestListAuditScopeComesFromSession(t *testing.T) {
	cases := map[string]struct {
		superadmin   bool
		wantOrg      string
		wantUnscoped bool
	}{
		"an org admin is scoped to their org": {false, "personal", false},
		"a superadmin reads every org":        {true, "personal", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			audit := &recordingAudit{}
			srv := auditServer(t, testTeamStore(t), audit, testAuth(tc.superadmin))

			resp := doRequest(t, srv, http.MethodGet, "/admin/audit", "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if audit.query.Unscoped != tc.wantUnscoped {
				t.Errorf("Unscoped = %v, want %v", audit.query.Unscoped, tc.wantUnscoped)
			}
			if audit.query.OrgID != tc.wantOrg {
				t.Errorf("OrgID = %q, want %q", audit.query.OrgID, tc.wantOrg)
			}
		})
	}
}

// The audit log is readable by a normal user now, but still only with a
// session: Step 1.5's boundary, re-checked on the endpoint that just moved out
// of the superadmin gate.
func TestListAuditRequiresASession(t *testing.T) {
	srv := auditServer(t, testTeamStore(t), &recordingAudit{}, Auth{})
	if resp := doRequest(t, srv, http.MethodGet, "/admin/audit", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// The actor reaches the wire as "actor", unchanged, so the console's audit
// table keeps working across the team-ID-to-user-ID switch.
func TestAuditViewRendersActorAndOrg(t *testing.T) {
	audit := &recordingAudit{page: logstore.AuditPage{Entries: []logstore.AuditEntry{{
		ID: "a1", Action: "team.patch", ActorUserID: "usr_9",
		OrganizationID: "personal", Superadmin: true, TargetTeamID: "acme",
	}}}}
	srv := auditServer(t, testTeamStore(t), audit, testAuth(true))

	resp := doRequest(t, srv, http.MethodGet, "/admin/audit", "")
	var got auditPageView
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(got.Entries))
	}
	e := got.Entries[0]
	if e.Actor != "usr_9" || e.Organization != "personal" || !e.Superadmin {
		t.Errorf("entry = %+v, want actor usr_9 / org personal / superadmin true", e)
	}
}
