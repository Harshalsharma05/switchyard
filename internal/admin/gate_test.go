package admin

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestOrgScopeTeamFilter exercises orgScope directly rather than through a
// full handler — this is the one place the security property lives (Multi-
// user Step 3.3: a non-superadmin can now narrow to one of their own teams,
// same as a superadmin always could), so it gets its own focused test rather
// than relying on handleSummary/handleCosts to exercise it incidentally.
func TestOrgScopeTeamFilter(t *testing.T) {
	teams := testTeamStore(t) // acme in "personal", globex in "other-org"

	cases := map[string]struct {
		superadmin bool
		queryTeam  string
		want       []string // nil means "unfiltered"
	}{
		"non-superadmin with no filter sees their own org": {
			superadmin: false, queryTeam: "", want: []string{"acme"},
		},
		"non-superadmin can narrow to their own team": {
			superadmin: false, queryTeam: "acme", want: []string{"acme"},
		},
		"non-superadmin filtering to another org's team falls back to their own org": {
			superadmin: false, queryTeam: "globex", want: []string{"acme"},
		},
		"non-superadmin filtering to an unknown id falls back to their own org": {
			superadmin: false, queryTeam: "does-not-exist", want: []string{"acme"},
		},
		"superadmin with no filter is unrestricted": {
			superadmin: true, queryTeam: "", want: nil,
		},
		"superadmin can narrow to any team": {
			superadmin: true, queryTeam: "globex", want: []string{"globex"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/admin/summary", nil)
			if tc.queryTeam != "" {
				q := r.URL.Query()
				q.Set("team", tc.queryTeam)
				r.URL.RawQuery = q.Encode()
			}
			c := Caller{UserID: "usr_test", OrgID: "personal", IsSuperadmin: tc.superadmin}
			r = r.WithContext(WithCaller(r.Context(), c))
			w := httptest.NewRecorder()

			got, ok := orgScope(w, r, teams, discardLogger())
			if !ok {
				t.Fatalf("orgScope returned ok=false, status %d", w.Code)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("orgScope = %v, want %v", got, tc.want)
			}
		})
	}
}
