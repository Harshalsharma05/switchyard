package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testAccessTTL  = 15 * time.Minute
	testRefreshTTL = 720 * time.Hour
)

// Rotation must invalidate what it replaces, or the 30-day token never
// actually expires in practice.
func TestRotateInvalidatesTheOldToken(t *testing.T) {
	s, ctx := newStore(t, "")
	u, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "person@example.com", "A Person"))
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}

	first, err := s.CreateSession(ctx, u.ID, "test-agent", "127.0.0.1", testRefreshTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, second, err := s.RotateSession(ctx, first.Raw, "test-agent", "127.0.0.1", testRefreshTTL)
	if err != nil {
		t.Fatalf("RotateSession: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("rotated into the wrong user: %q", got.ID)
	}
	if second.Raw == first.Raw {
		t.Fatal("rotation reissued the same token")
	}

	// The new one works.
	if _, _, err := s.RotateSession(ctx, second.Raw, "test-agent", "127.0.0.1", testRefreshTTL); err != nil {
		t.Fatalf("rotating the new token: %v", err)
	}
}

// Replaying a spent token means it either leaked or the client is broken, and
// the server cannot tell which -- so every session for that user dies.
func TestReplayedTokenRevokesEverySession(t *testing.T) {
	s, ctx := newStore(t, "")
	u, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "person@example.com", "A Person"))
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}

	// Two independent logins, as two browsers would produce.
	stolen, err := s.CreateSession(ctx, u.ID, "browser-a", "127.0.0.1", testRefreshTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	other, err := s.CreateSession(ctx, u.ID, "browser-b", "127.0.0.1", testRefreshTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, _, err := s.RotateSession(ctx, stolen.Raw, "browser-a", "127.0.0.1", testRefreshTTL); err != nil {
		t.Fatalf("first rotation: %v", err)
	}

	// The same token again: this is the replay.
	_, _, err = s.RotateSession(ctx, stolen.Raw, "attacker", "10.0.0.1", testRefreshTTL)
	if !errors.Is(err, ErrTokenReused) {
		t.Fatalf("error = %v, want ErrTokenReused", err)
	}

	// The untouched second browser must also be logged out.
	if _, _, err := s.RotateSession(ctx, other.Raw, "browser-b", "127.0.0.1", testRefreshTTL); !errors.Is(err, ErrTokenReused) {
		t.Fatalf("second session error = %v, want it revoked too", err)
	}

	var live int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE revoked_at IS NULL").Scan(&live); err != nil {
		t.Fatalf("counting live sessions: %v", err)
	}
	if live != 0 {
		t.Fatalf("live sessions = %d, want every one revoked", live)
	}
}

func TestRotateRejectsUnknownExpiredAndRevoked(t *testing.T) {
	s, ctx := newStore(t, "")
	u, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "person@example.com", "A Person"))
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}

	if _, _, err := s.RotateSession(ctx, "never-issued", "a", "127.0.0.1", testRefreshTTL); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown token error = %v, want ErrNoSession", err)
	}

	expired, err := s.CreateSession(ctx, u.ID, "a", "127.0.0.1", -time.Minute)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, _, err := s.RotateSession(ctx, expired.Raw, "a", "127.0.0.1", testRefreshTTL); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("expired token error = %v, want ErrSessionExpired", err)
	}

	loggedOut, err := s.CreateSession(ctx, u.ID, "a", "127.0.0.1", testRefreshTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.RevokeSession(ctx, loggedOut.Raw); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	// A revoked token is treated as a replay, which also kills the chain.
	if _, _, err := s.RotateSession(ctx, loggedOut.Raw, "a", "127.0.0.1", testRefreshTTL); !errors.Is(err, ErrTokenReused) {
		t.Errorf("revoked token error = %v, want ErrTokenReused", err)
	}
}

// The raw token is what the browser holds; only its hash may reach the table.
func TestRefreshTokenIsStoredHashedOnly(t *testing.T) {
	s, ctx := newStore(t, "")
	u, _, err := s.FindOrCreateGoogleUser(ctx, profile("sub-1", "person@example.com", "A Person"))
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	sess, err := s.CreateSession(ctx, u.ID, "agent", "127.0.0.1", testRefreshTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var stored string
	if err := s.pool.QueryRow(ctx, "SELECT refresh_token_hash FROM sessions WHERE id = $1", sess.ID).Scan(&stored); err != nil {
		t.Fatalf("reading hash: %v", err)
	}
	if stored == sess.Raw {
		t.Fatal("the raw refresh token was stored")
	}
	if stored != hashToken(sess.Raw) {
		t.Fatal("stored value is not the token hash")
	}
}

func TestRequireCSRF(t *testing.T) {
	h := testHandlers()
	good, err := h.newCSRFToken()
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}
	forged := "attacker-chosen-value.AAAA"

	tests := map[string]struct {
		method string
		cookie string
		header string
		want   int
	}{
		"get is exempt":            {method: http.MethodGet, want: http.StatusOK},
		"head is exempt":           {method: http.MethodHead, want: http.StatusOK},
		"matching pair":            {method: http.MethodPost, cookie: good, header: good, want: http.StatusOK},
		"no cookie":                {method: http.MethodPost, header: good, want: http.StatusForbidden},
		"no header":                {method: http.MethodPost, cookie: good, want: http.StatusForbidden},
		"header does not match":    {method: http.MethodPost, cookie: good, header: "something-else", want: http.StatusForbidden},
		"forged unsigned pair":     {method: http.MethodPost, cookie: forged, header: forged, want: http.StatusForbidden},
		"delete is also protected": {method: http.MethodDelete, cookie: good, want: http.StatusForbidden},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			r := httptest.NewRequest(tc.method, "/auth/logout", nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: csrfCookie, Value: tc.cookie})
			}
			if tc.header != "" {
				r.Header.Set(CSRFHeader, tc.header)
			}
			w := httptest.NewRecorder()

			h.RequireCSRF(next).ServeHTTP(w, r)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

// The cookie flags are the whole point of putting the token in a cookie.
func TestSessionCookieFlags(t *testing.T) {
	h := testHandlers()
	h.cfg.Secure = true

	w := httptest.NewRecorder()
	err := h.setSessionCookies(w, User{ID: "usr_1", OrganizationID: "org_1"}, Session{ID: "ses_1", Raw: "raw-token"}, true)
	if err != nil {
		t.Fatalf("setSessionCookies: %v", err)
	}

	got := map[string]*http.Cookie{}
	for _, c := range w.Result().Cookies() {
		got[c.Name] = c
	}

	for _, name := range []string{sessionCookie, refreshCookie, csrfCookie} {
		c, ok := got[name]
		if !ok {
			t.Fatalf("%s was not set", name)
		}
		if !c.Secure {
			t.Errorf("%s is not Secure", name)
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("%s SameSite = %v, want Lax", name, c.SameSite)
		}
	}

	if !got[sessionCookie].HttpOnly {
		t.Error("the access token cookie is readable from JavaScript")
	}
	if !got[refreshCookie].HttpOnly {
		t.Error("the refresh token cookie is readable from JavaScript")
	}
	if got[csrfCookie].HttpOnly {
		t.Error("the csrf cookie must be readable: the console has to echo it in a header")
	}

	// The long-lived credential must not travel on ordinary dashboard requests.
	if got[refreshCookie].Path != refreshPath {
		t.Errorf("refresh cookie path = %q, want %q", got[refreshCookie].Path, refreshPath)
	}
	if got[sessionCookie].Path != "/" {
		t.Errorf("session cookie path = %q, want /", got[sessionCookie].Path)
	}
	if got[refreshCookie].Value != "raw-token" {
		t.Error("refresh cookie does not carry the raw token")
	}
}
