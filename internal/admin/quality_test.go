package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

func qualityServer(t *testing.T, reader RequestLogReader, auth Auth) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(func() bool { return true }, testTeamStore(t), &fakeSpendReader{},
		fakeProviderLister{}, fakeHealthReader{}, &fakeBreakerController{}, nil, fakeReloader, reader,
		nil, nil, nil, QualityFeedbackConfig{ExampleLimit: 5}, true, nil, nil,
		auth, testMetrics(t), discardLogger()))
	t.Cleanup(srv.Close)
	return srv
}

// Quality scores are tenant data, so the feedback view is org-scoped like the
// request log it reads from. Step 2.4 opened the route to org admins, which is
// what makes the scope load-bearing rather than decorative.
func TestQualityFeedbackScopedToOrg(t *testing.T) {
	cases := map[string]struct {
		superadmin bool
		wantTeams  []string
	}{
		"an org admin sees only their own org's scores": {false, []string{"acme"}},
		"a superadmin spans every org":                  {true, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reader := &fakeRequestLogReader{
				qualityFeedback: logstore.QualityFeedback{
					Reasons: []logstore.QualityReasonStat{{Reason: "downgraded", Scored: 1, AvgScore: 4}},
				},
			}
			srv := qualityServer(t, reader, testAuth(tc.superadmin))

			resp := get(t, srv, "/admin/quality/feedback")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if !slicesEqual(reader.gotTeamIDs, tc.wantTeams) {
				t.Errorf("team scope = %v, want %v", reader.gotTeamIDs, tc.wantTeams)
			}
		})
	}
}

// A client-supplied ?team= is not a way into another org's scores.
func TestQualityFeedbackIgnoresClientTeam(t *testing.T) {
	reader := &fakeRequestLogReader{}
	srv := qualityServer(t, reader, testAuth(false))

	if resp := get(t, srv, "/admin/quality/feedback?team=globex"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if want := []string{"acme"}; !slicesEqual(reader.gotTeamIDs, want) {
		t.Errorf("team scope = %v, want %v", reader.gotTeamIDs, want)
	}
}
