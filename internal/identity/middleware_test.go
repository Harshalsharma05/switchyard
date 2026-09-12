package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The admin port must take a session and nothing else. A team API key is the
// credential that must not work here, and the guarantee is structural: the
// middleware never reads the Authorization header, so there is no rule to relax.
func TestRequireSessionIgnoresTeamKeys(t *testing.T) {
	h := testHandlers()
	valid := validToken(t, h.signer, time.Now().Add(time.Hour))

	tests := map[string]struct {
		cookie string
		header string
		want   int
	}{
		"valid session":                 {cookie: valid, want: http.StatusOK},
		"no credential at all":          {want: http.StatusUnauthorized},
		"admin team key as bearer":      {header: "Bearer sk-switchyard-dev-acme-9f2b1c", want: http.StatusUnauthorized},
		"team key and no cookie":        {header: "Bearer sk-anything-at-all", want: http.StatusUnauthorized},
		"expired session":               {cookie: validToken(t, h.signer, time.Now().Add(-time.Minute)), want: http.StatusUnauthorized},
		"session signed by another key": {cookie: validToken(t, signer{secret: []byte("other")}, time.Now().Add(time.Hour)), want: http.StatusUnauthorized},
		"team key alongside a good one": {cookie: valid, header: "Bearer sk-irrelevant", want: http.StatusOK},
		"empty cookie":                  {cookie: "", want: http.StatusUnauthorized},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			r := httptest.NewRequest(http.MethodGet, "/admin/summary", nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
			}
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()

			h.RequireSession(nil)(next).ServeHTTP(w, r)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// The claims the handler sees must come from the verified token, never from
// anything the caller could set -- this is what Phase 2's org scoping rests on.
func TestRequireSessionAttachesVerifiedClaims(t *testing.T) {
	h := testHandlers()
	tok, err := h.signer.mint(Claims{
		UserID: "usr_real", OrgID: "org_real", Superadmin: true, SessionID: "ses_1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	var got Claims
	attach := func(ctx context.Context, c Claims) context.Context {
		got = c
		return ctx
	}

	r := httptest.NewRequest(http.MethodGet, "/admin/summary?org=org_attacker&team=whatever", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	r.Header.Set("X-Org-Id", "org_attacker")

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h.RequireSession(attach)(next).ServeHTTP(httptest.NewRecorder(), r)

	if got.UserID != "usr_real" || got.OrgID != "org_real" || !got.Superadmin {
		t.Fatalf("claims = %+v, want them from the token", got)
	}
}
