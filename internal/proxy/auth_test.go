package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

func TestBearerToken(t *testing.T) {
	tests := map[string]struct {
		header string
		want   string
		ok     bool
	}{
		"well formed":        {"Bearer sk-abc123", "sk-abc123", true},
		"missing header":     {"", "", false},
		"wrong scheme":       {"Basic dXNlcjpwYXNz", "", false},
		"bearer with no key": {"Bearer ", "", false},
		"bearer trims space": {"Bearer   sk-abc123  ", "sk-abc123", true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			got, ok := bearerToken(req)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

// TeamFrom returning nil when Auth never ran is what authorizeModel's
// wiring-bug branch in handler.go depends on to tell "not authenticated"
// apart from "authenticated as a team with no allowlist entries."
func TestTeamFromWithoutAuthIsNil(t *testing.T) {
	if got := TeamFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != nil {
		t.Errorf("TeamFrom on a bare context = %+v, want nil", got)
	}
}

func TestAuthAttachesTeamToContext(t *testing.T) {
	team := &auth.Team{ID: "acme"}
	var seen *auth.Team

	h := Auth(stubAuthenticator{team: team}, discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TeamFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-abc123")

	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != team {
		t.Fatalf("handler saw team %+v, want the same *Team instance %+v", seen, team)
	}
}

func TestAuthRejectsMissingHeader(t *testing.T) {
	called := false
	h := Auth(stubAuthenticator{team: &auth.Team{ID: "acme"}}, discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("handler ran despite a missing Authorization header")
	}
}

func TestAuthRejectsUnknownKey(t *testing.T) {
	called := false
	h := Auth(stubAuthenticator{err: auth.ErrUnknownKey}, discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-nope")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("handler ran despite an unknown key")
	}
}

// A failure that is not ErrUnknownKey (a Redis-backed registry down the line,
// say) must not be reported to the caller as their own bad credential.
func TestAuthSurfacesUnexpectedErrorAs500(t *testing.T) {
	h := Auth(stubAuthenticator{err: errors.New("registry backend unavailable")}, discardLogger())(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler must not run when Authenticate fails unexpectedly")
		}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-abc123")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
