package identity

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/oauth"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testHandlers() *Handlers {
	return NewHandlers(
		oauth.Config{ClientID: "client-1", ClientSecret: "secret", RedirectURL: "http://localhost:9090/auth/google/callback"},
		nil, []byte("test-signing-secret"), "/", "/login", false, discardLogger(),
	)
}

// The state check is the CSRF defence for the whole flow, so every way of
// getting it wrong must be rejected outright -- 400, no redirect, no retry.
func TestCallbackRejectsBadState(t *testing.T) {
	h := testHandlers()
	good := h.seal("state-abc", "verifier-xyz")

	tests := map[string]struct {
		cookie string
		state  string
	}{
		"no cookie at all":      {cookie: "", state: "state-abc"},
		"no state in the query": {cookie: good, state: ""},
		"state does not match":  {cookie: good, state: "state-different"},
		"tampered signature":    {cookie: good[:len(good)-2] + "xx", state: "state-abc"},
		"tampered payload":      {cookie: "state-evil." + strings.SplitN(good, ".", 2)[1], state: "state-evil"},
		"no signature segment":  {cookie: "state-abc", state: "state-abc"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/auth/google/callback?code=c&state="+tc.state, nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: stateCookie, Value: tc.cookie})
			}
			w := httptest.NewRecorder()

			h.callback(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", w.Code, w.Body.String())
			}
		})
	}
}

// A valid state must survive the round trip, or the rejection test above would
// pass for the wrong reason.
func TestSealOpenRoundTrip(t *testing.T) {
	h := testHandlers()
	state, verifier, ok := h.open(h.seal("state-abc", "verifier-xyz"))
	if !ok || state != "state-abc" || verifier != "verifier-xyz" {
		t.Fatalf("open() = %q, %q, %v", state, verifier, ok)
	}

	// A cookie sealed with a different secret is not ours.
	other := NewHandlers(oauth.Config{}, nil, []byte("a-different-secret"), "/", "/login", false, discardLogger())
	if _, _, ok := h.open(other.seal("state-abc", "verifier-xyz")); ok {
		t.Fatal("accepted a cookie signed with another secret")
	}
}

// start must park the state in an httpOnly cookie and send the browser to
// Google with a PKCE challenge -- never the verifier itself.
func TestStartSetsStateAndRedirects(t *testing.T) {
	h := testHandlers()
	w := httptest.NewRecorder()
	h.start(w, httptest.NewRequest(http.MethodGet, "/auth/google", nil))

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}

	var c *http.Cookie
	for _, got := range w.Result().Cookies() {
		if got.Name == stateCookie {
			c = got
		}
	}
	if c == nil {
		t.Fatal("no state cookie was set")
	}
	if !c.HttpOnly {
		t.Error("state cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax: Strict would withhold the cookie on Google's callback", c.SameSite)
	}

	state, verifier, ok := h.open(c.Value)
	if !ok {
		t.Fatal("state cookie does not verify")
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "state="+state) {
		t.Error("redirect does not carry the state from the cookie")
	}
	if !strings.Contains(loc, "code_challenge_method=S256") {
		t.Error("redirect does not request S256 PKCE")
	}
	if strings.Contains(loc, verifier) {
		t.Error("redirect leaked the PKCE verifier; only its challenge may travel to Google")
	}
}
