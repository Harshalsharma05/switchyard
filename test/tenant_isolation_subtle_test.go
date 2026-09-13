// Phase 4 Step 4.3 — the subtler vectors that a straightforward "guess an ID"
// attack does not cover: aggregates that could quietly include another org's
// numbers without ever naming it, error bodies that could leak more than the
// attacker already supplied, a JWT whose claim is edited after signing, the
// two ports re-checked from the integration level rather than trusted from
// Phase 1's unit tests, and the audit log's no-tenant entries, which must
// reach the superadmin and nobody else.
//
//go:build integration

package integration

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAggregatesStayWithinCallersOrg sums two aggregate endpoints against the
// Step 4.1 fixture's known seed data. Each org's three seeded rows cost 1000
// micros apiece and carry exactly one "downgraded" quality score — so a
// scoping bug that merges org B's rows into org A's view shows up as a wrong
// number even though no response ever names org B's team or org ID directly,
// which is exactly the failure mode a substring check on Step 4.2 could not
// have caught.
func TestAggregatesStayWithinCallersOrg(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	for _, s := range []struct {
		name         string
		cookie, csrf string
	}{
		{"org A", f.orgACookie, f.orgACSRF},
		{"org B", f.orgBCookie, f.orgBCSRF},
	} {
		t.Run(s.name, func(t *testing.T) {
			status, raw := adminRequest(t, f.gw, http.MethodGet, "/admin/costs?by=team", s.cookie, s.csrf, nil)
			if status != http.StatusOK {
				t.Fatalf("%s reading costs: want 200, got %d: %s", s.name, status, raw)
			}
			var costs struct {
				Series []struct {
					TotalMicros int64            `json:"total_micros"`
					Breakdown   map[string]int64 `json:"breakdown"`
				} `json:"series"`
			}
			if err := json.Unmarshal(raw, &costs); err != nil {
				t.Fatalf("decoding %s's costs: %v", s.name, err)
			}
			var total int64
			for _, point := range costs.Series {
				total += point.TotalMicros
			}
			if total != 3000 {
				t.Errorf("%s's total cost across its 3 seeded rows: want 3000 micros, got %d", s.name, total)
			}

			status, raw = adminRequest(t, f.gw, http.MethodGet, "/admin/quality/feedback", s.cookie, s.csrf, nil)
			if status != http.StatusOK {
				t.Fatalf("%s reading quality feedback: want 200, got %d: %s", s.name, status, raw)
			}
			var fb struct {
				Routing struct {
					Stat *struct {
						Scored int64 `json:"scored"`
					} `json:"stat"`
				} `json:"routing"`
			}
			if err := json.Unmarshal(raw, &fb); err != nil {
				t.Fatalf("decoding %s's quality feedback: %v", s.name, err)
			}
			if fb.Routing.Stat == nil {
				t.Fatalf("%s's quality feedback has no downgraded stat at all; the fixture seeded one", s.name)
			}
			if fb.Routing.Stat.Scored != 1 {
				t.Errorf("%s's downgraded quality count: want 1 (its own seeded row), got %d — the other org's row leaked in", s.name, fb.Routing.Stat.Scored)
			}
		})
	}
}

// TestCrossOrgErrorsDoNotNameTheOtherOrg re-reads two of Step 4.2's cross-org
// 404s, but asks a different question of them: not "is this 404" (already
// proven) but "does the body say anything about org B that org A's session
// had no way to already know". A team ID is something the attacker supplied
// itself; org B's real display name and organisation ID are not, so their
// presence in a 404 meant for org A would be a genuine disclosure.
func TestCrossOrgErrorsDoNotNameTheOtherOrg(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	cases := map[string]struct {
		method, path string
	}{
		"team detail 404":    {http.MethodGet, "/admin/teams/" + f.teamB},
		"reset-budget 404":   {http.MethodPost, "/admin/teams/" + f.teamB + "/reset-budget"},
		"request detail 404": {http.MethodGet, "/admin/requests/does-not-exist"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			status, raw := adminRequest(t, f.gw, tc.method, tc.path, f.orgACookie, f.orgACSRF, nil)
			if status != http.StatusNotFound {
				t.Fatalf("%s: want 404, got %d: %s", name, status, raw)
			}
			body := string(raw)
			if strings.Contains(body, f.orgB) {
				t.Errorf("%s leaked org B's real organisation ID: %s", name, body)
			}
			if strings.Contains(body, "Org B") {
				t.Errorf("%s leaked org B's display name: %s", name, body)
			}
		})
	}
}

// tamperOrgClaim edits the org claim inside an already-signed session cookie
// and reassembles it with the ORIGINAL signature, which now no longer matches
// the new payload. This is precisely what an attacker without the signing
// secret would produce by editing the (unencrypted, merely signed) JWT
// payload — the honest way to construct this attack, rather than re-signing
// with a secret a real attacker would not have.
func tamperOrgClaim(t *testing.T, cookie, newOrg string) string {
	t.Helper()

	sessionPart, rest, _ := strings.Cut(cookie, "; ")
	jwt := strings.TrimPrefix(sessionPart, "sy_session=")

	segs := strings.Split(jwt, ".")
	if len(segs) != 3 {
		t.Fatalf("session cookie is not a three-part JWT: %q", jwt)
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(segs[1])
	if err != nil {
		t.Fatalf("decoding session payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		t.Fatalf("unmarshalling session payload: %v", err)
	}
	claims["org"] = newOrg
	tamperedPayload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshalling tampered payload: %v", err)
	}
	segs[1] = base64.RawURLEncoding.EncodeToString(tamperedPayload)
	// segs[2], the signature, is deliberately left untouched: it was computed
	// over the original payload and now pairs with a different one.
	tampered := "sy_session=" + strings.Join(segs, ".")
	if rest != "" {
		tampered += "; " + rest
	}
	return tampered
}

// TestTamperedJWTOrgClaimIsRejected covers Step 4.3's token-tampering case:
// an org-A session whose org claim is edited to org B after signing must be
// rejected outright by signature verification, not merely denied by org
// scoping — the whole point of signing the token is that scoping never even
// gets a chance to run on a forged claim.
func TestTamperedJWTOrgClaimIsRejected(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	tampered := tamperOrgClaim(t, f.orgACookie, f.orgB)
	status, raw := adminRequest(t, f.gw, http.MethodGet, "/admin/teams", tampered, "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("a session with a tampered org claim: want 401, got %d: %s", status, raw)
	}
}

// TestCrossIdentityRejectedOnBothPorts re-verifies Phase 1's boundary at the
// integration level: a team API key is not a valid admin credential, and a
// dashboard session cookie is not a valid gateway credential, on the actual
// running binary rather than only in internal/proxy and internal/identity's
// unit tests.
func TestCrossIdentityRejectedOnBothPorts(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	t.Run("team key against the admin port", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, f.gw.AdminURL+"/admin/teams", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+f.teamAKey)

		resp, err := f.gw.Client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a team API key against the admin port: want 401, got %d", resp.StatusCode)
		}
	})

	t.Run("session cookie against the gateway port", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, f.gw.BaseURL+"/v1/chat/completions", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Cookie", f.orgACookie)
		req.Header.Set("Content-Type", "application/json")

		resp, err := f.gw.Client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a session cookie against the gateway port: want 401, got %d", resp.StatusCode)
		}
	})
}

// TestNoTenantAuditEntriesAreSuperadminOnly closes the last half of Step
// 4.3's audit check: the fixture's config-reload entry belongs to no
// organisation at all, and must reach the superadmin — with a real actor
// identity, since an unscoped read never redacts — while staying invisible
// to both org A and org B.
func TestNoTenantAuditEntriesAreSuperadminOnly(t *testing.T) {
	f := setupTenantIsolationFixture(t)

	type entry struct {
		Actor          string `json:"actor"`
		OrganizationID string `json:"organization_id"`
	}

	status, raw := adminRequest(t, f.gw, http.MethodGet, "/admin/audit", f.superadminCookie, f.superadminCSRF, nil)
	if status != http.StatusOK {
		t.Fatalf("superadmin listing the audit log: want 200, got %d: %s", status, raw)
	}
	var superView struct {
		Entries []entry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &superView); err != nil {
		t.Fatalf("decoding superadmin's audit page: %v", err)
	}
	var found *entry
	for i, e := range superView.Entries {
		if e.OrganizationID == "" {
			found = &superView.Entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("superadmin's audit log has no no-tenant entry; the fixture's config reload should have written one")
	}
	if found.Actor == "" || found.Actor == "platform-operator" {
		t.Errorf("superadmin's unscoped read should show the real actor, got %q", found.Actor)
	}

	for _, s := range []struct {
		name         string
		cookie, csrf string
	}{
		{"org A", f.orgACookie, f.orgACSRF},
		{"org B", f.orgBCookie, f.orgBCSRF},
	} {
		t.Run(s.name, func(t *testing.T) {
			status, raw := adminRequest(t, f.gw, http.MethodGet, "/admin/audit", s.cookie, s.csrf, nil)
			if status != http.StatusOK {
				t.Fatalf("%s listing its own audit log: want 200, got %d: %s", s.name, status, raw)
			}
			var view struct {
				Entries []entry `json:"entries"`
			}
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatalf("decoding %s's audit page: %v", s.name, err)
			}
			for _, e := range view.Entries {
				if e.OrganizationID == "" {
					t.Errorf("%s saw a no-tenant audit entry (actor %q); those must be superadmin-only", s.name, e.Actor)
				}
			}
		})
	}
}
