// Phase 4 Step 4.2 — attack every admin endpoint from inside the Step 4.1
// fixture: signed in as org A, try to reach org B's data through a path
// parameter, a query parameter, a request body, or plain pagination. Every
// case here expects 404, never 403 — a 403 would confirm the resource exists,
// which is itself a leak — except the one case (naming another organisation
// on team creation) where the plan itself calls for a 400.
//
//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// adminRequest issues one authenticated call to the admin port and returns
// its status and raw body, so every attack below only has to state which
// session, which endpoint, and what it expects.
func adminRequest(t *testing.T, gw *gatewayInstance, method, path, cookie, csrf string, body []byte) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, gw.AdminURL+path, reader)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)

	resp, err := gw.Client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body for %s %s: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

// TestCrossOrgAccessReturns404 is Step 4.2's core case: signed in as org A,
// reach for org B's team and request by ID, and try every mutation the plan
// names (patch, delete, budget reset, key rotate, key revoke) against org
// B's team. Every one must answer 404, matched against a same-shaped call
// org A makes against its own resources to prove the 404 is about ownership
// and not a broken route.
func TestCrossOrgAccessReturns404(t *testing.T) {
	f := setupTenantIsolationFixture(t)
	ctx := context.Background()

	var orgARequestID, orgBRequestID string
	if err := f.db.QueryRow(ctx, `SELECT id FROM requests WHERE team_id = $1 LIMIT 1`, f.teamA).Scan(&orgARequestID); err != nil {
		t.Fatalf("finding an org-A request row: %v", err)
	}
	if err := f.db.QueryRow(ctx, `SELECT id FROM requests WHERE team_id = $1 LIMIT 1`, f.teamB).Scan(&orgBRequestID); err != nil {
		t.Fatalf("finding an org-B request row: %v", err)
	}

	t.Run("reads", func(t *testing.T) {
		cases := map[string]string{
			"team detail":    "/admin/teams/" + f.teamB,
			"request detail": "/admin/requests/" + orgBRequestID,
		}
		for name, path := range cases {
			t.Run(name, func(t *testing.T) {
				status, _ := adminRequest(t, f.gw, http.MethodGet, path, f.orgACookie, f.orgACSRF, nil)
				if status != http.StatusNotFound {
					t.Errorf("org A reading org B's resource at %s: want 404, got %d", path, status)
				}
			})
		}
	})

	t.Run("mutations", func(t *testing.T) {
		patchBody, err := json.Marshal(map[string]any{"rpm": 500})
		if err != nil {
			t.Fatalf("marshalling patch body: %v", err)
		}

		cases := map[string]struct {
			method, path string
			body         []byte
		}{
			"patch team":   {http.MethodPatch, "/admin/teams/" + f.teamB, patchBody},
			"delete team":  {http.MethodDelete, "/admin/teams/" + f.teamB, nil},
			"reset budget": {http.MethodPost, "/admin/teams/" + f.teamB + "/reset-budget", nil},
			"rotate key":   {http.MethodPost, "/admin/teams/" + f.teamB + "/key/rotate", nil},
			"revoke key":   {http.MethodDelete, "/admin/teams/" + f.teamB + "/key", nil},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				status, _ := adminRequest(t, f.gw, tc.method, tc.path, f.orgACookie, f.orgACSRF, tc.body)
				if status != http.StatusNotFound {
					t.Errorf("org A mutating org B's team via %s %s: want 404, got %d", tc.method, tc.path, status)
				}
			})
		}
	})

	t.Run("controls: org A still reaches its own resources", func(t *testing.T) {
		status, _ := adminRequest(t, f.gw, http.MethodGet, "/admin/teams/"+f.teamA, f.orgACookie, f.orgACSRF, nil)
		if status != http.StatusOK {
			t.Errorf("org A reading its own team: want 200, got %d", status)
		}
		status, _ = adminRequest(t, f.gw, http.MethodGet, "/admin/requests/"+orgARequestID, f.orgACookie, f.orgACSRF, nil)
		if status != http.StatusOK {
			t.Errorf("org A reading its own request: want 200, got %d", status)
		}
		status, _ = adminRequest(t, f.gw, http.MethodPost, "/admin/teams/"+f.teamA2+"/key/rotate", f.orgACookie, f.orgACSRF, nil)
		if status != http.StatusOK {
			t.Errorf("org A rotating its own team's key: want 200, got %d", status)
		}
	})
}

// TestCreateTeamInAnotherOrgIsRejected covers the one case in Step 4.2 that
// is not a 404: a normal user naming another organisation on team creation
// gets a 400, per createOrgFor's documented contract — the request never
// specifies an existing team to look up, so there is nothing to 404 for.
func TestCreateTeamInAnotherOrgIsRejected(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	body, err := json.Marshal(map[string]any{
		"name":               "sneaky-team",
		"organization_id":    f.orgB,
		"priority":           "realtime",
		"rpm":                1000,
		"tpm":                1_000_000,
		"monthly_budget_usd": 100.0,
		"allowed_providers":  []string{},
		"allowed_models":     []string{},
	})
	if err != nil {
		t.Fatalf("marshalling create-team body: %v", err)
	}

	status, raw := adminRequest(t, f.gw, http.MethodPost, "/admin/teams", f.orgACookie, f.orgACSRF, body)
	if status != http.StatusBadRequest {
		t.Fatalf("org A creating a team in org B: want 400, got %d: %s", status, raw)
	}

	// Confirm nothing was actually created: org B's own team list is
	// unaffected by the rejected attempt.
	status, raw = adminRequest(t, f.gw, http.MethodGet, "/admin/teams", f.orgBCookie, f.orgBCSRF, nil)
	if status != http.StatusOK {
		t.Fatalf("org B listing its own teams: want 200, got %d", status)
	}
	if strings.Contains(string(raw), "sneaky-team") {
		t.Error("a team rejected for naming another organisation still appeared in that organisation's team list")
	}
}

// TestReadEndpointsStayWithinCallersOrg attacks the cross-team read
// endpoints two ways: naming org B's team in the ?team= filter every one of
// them accepts, and paging org A's own request history all the way past its
// end. orgScope's contract is that a foreign ?team= silently falls back to
// the caller's full org scope rather than erroring or matching — so the
// assertion is not just "no error", it is "org B's team never appears".
func TestReadEndpointsStayWithinCallersOrg(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	t.Run("team filter falls back instead of leaking", func(t *testing.T) {
		endpoints := []string{
			"/admin/requests",
			"/admin/costs",
			"/admin/attribution",
			"/admin/reconciliation",
			"/admin/quality/feedback",
			"/admin/summary",
		}
		for _, ep := range endpoints {
			t.Run(ep, func(t *testing.T) {
				path := ep + "?team=" + url.QueryEscape(f.teamB)
				status, raw := adminRequest(t, f.gw, http.MethodGet, path, f.orgACookie, f.orgACSRF, nil)
				if status != http.StatusOK {
					t.Fatalf("org A querying %s with org B's team: want 200, got %d: %s", ep, status, raw)
				}
				if strings.Contains(string(raw), f.teamB) {
					t.Errorf("%s leaked org B's team id into org A's response: %s", ep, raw)
				}
			})
		}
	})

	t.Run("pagination reaches the end without crossing orgs", func(t *testing.T) {
		type page struct {
			Requests []struct {
				TeamID string `json:"team_id"`
			} `json:"requests"`
			NextCursor string `json:"next_cursor"`
		}
		fetch := func(cursor string) page {
			t.Helper()
			path := "/admin/requests?limit=1"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			status, raw := adminRequest(t, f.gw, http.MethodGet, path, f.orgACookie, f.orgACSRF, nil)
			if status != http.StatusOK {
				t.Fatalf("paging org A's requests (cursor %q): want 200, got %d: %s", cursor, status, raw)
			}
			var p page
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("decoding page (cursor %q): %v", cursor, err)
			}
			return p
		}

		total := 0
		cursor := ""
		for i := 0; i < 10; i++ {
			p := fetch(cursor)
			for _, req := range p.Requests {
				if req.TeamID != f.teamA && req.TeamID != f.teamA2 {
					t.Fatalf("page %d returned a row for team %q, outside org A", i, req.TeamID)
				}
			}
			total += len(p.Requests)
			if p.NextCursor == "" {
				// Reached the end of org A's own data — every row already
				// checked above belongs to org A, and the empty NextCursor
				// means the query found nothing more to page into rather
				// than continuing past org A's rows into org B's.
				if total != 3 {
					t.Errorf("org A's paginated request history: want 3 rows total, got %d", total)
				}
				return
			}
			cursor = p.NextCursor
		}
		t.Fatal("paged 10 times without reaching the end of org A's 3 seeded rows — possible infinite loop or leak")
	})
}

// TestAuditListNeverLeaksAnotherOrgOrOperatorIdentity re-checks the audit log
// from the read side of Step 4.2's inventory: there is no per-entry GET-by-ID
// route to attack directly, so the equivalent attack is confirming the list
// itself, which takes its scope only from the session's org claim, never
// exposes an org-B row or the superadmin's real identity to org A.
func TestAuditListNeverLeaksAnotherOrgOrOperatorIdentity(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	for _, s := range []struct {
		name          string
		cookie, csrf  string
		ownOrg        string
		foreignTeamID string
	}{
		{"org A", f.orgACookie, f.orgACSRF, f.orgA, f.teamB},
		{"org B", f.orgBCookie, f.orgBCSRF, f.orgB, f.teamA},
	} {
		t.Run(s.name, func(t *testing.T) {
			status, raw := adminRequest(t, f.gw, http.MethodGet, "/admin/audit", s.cookie, s.csrf, nil)
			if status != http.StatusOK {
				t.Fatalf("%s listing its own audit log: want 200, got %d: %s", s.name, status, raw)
			}

			var body struct {
				Entries []struct {
					Actor          string `json:"actor"`
					ActorAddr      string `json:"actor_addr"`
					OrganizationID string `json:"organization_id"`
					TargetTeam     string `json:"target_team"`
					Superadmin     bool   `json:"superadmin"`
				} `json:"entries"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decoding %s's audit page: %v", s.name, err)
			}
			if len(body.Entries) == 0 {
				t.Fatalf("%s's audit log is empty; the fixture should have seeded at least one entry", s.name)
			}
			for _, e := range body.Entries {
				if e.OrganizationID != s.ownOrg {
					t.Errorf("%s saw an audit entry filed against organisation %q", s.name, e.OrganizationID)
				}
				if e.TargetTeam == s.foreignTeamID {
					t.Errorf("%s saw an audit entry targeting the other organisation's team %q", s.name, e.TargetTeam)
				}
				if e.Superadmin && e.Actor != "platform-operator" {
					t.Errorf("%s saw the operator's real identity (%q) on a superadmin entry instead of the redacted sentinel", s.name, e.Actor)
				}
				if e.Superadmin && e.ActorAddr != "" {
					t.Errorf("%s saw the operator's real address (%q) on a superadmin entry instead of a redacted blank", s.name, e.ActorAddr)
				}
			}
		})
	}
}
