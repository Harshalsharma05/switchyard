package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testClientID = "client-123.apps.googleusercontent.com"

// idToken builds an unsigned token with the given claims. The signature is
// junk on purpose: profileFromIDToken must not be reading it.
func idToken(t *testing.T, c claims) string {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".not-a-signature"
}

func validClaims() claims {
	return claims{
		Iss:           "https://accounts.google.com",
		Aud:           testClientID,
		Sub:           "google-sub-1",
		Exp:           time.Now().Add(time.Hour).Unix(),
		Email:         "person@example.com",
		EmailVerified: true,
		Name:          "A Person",
	}
}

func TestProfileFromIDTokenRejects(t *testing.T) {
	cfg := Config{ClientID: testClientID}
	now := time.Now()

	tests := map[string]struct {
		mutate func(*claims)
		token  string
		want   error
	}{
		"wrong audience":     {mutate: func(c *claims) { c.Aud = "someone-else" }},
		"wrong issuer":       {mutate: func(c *claims) { c.Iss = "https://evil.example" }},
		"expired":            {mutate: func(c *claims) { c.Exp = now.Add(-time.Minute).Unix() }},
		"no expiry":          {mutate: func(c *claims) { c.Exp = 0 }},
		"missing sub":        {mutate: func(c *claims) { c.Sub = "" }},
		"missing email":      {mutate: func(c *claims) { c.Email = "" }},
		"mismatched azp":     {mutate: func(c *claims) { c.Azp = "another-client" }},
		"unverified email":   {mutate: func(c *claims) { c.EmailVerified = false }, want: ErrEmailNotVerified},
		"not three segments": {token: "a.b"},
		"payload not base64": {token: "a.!!!.c"},
		"payload not json":   {token: "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tok := tc.token
			if tok == "" {
				c := validClaims()
				tc.mutate(&c)
				tok = idToken(t, c)
			}

			_, err := cfg.profileFromIDToken(tok, now)
			if err == nil {
				t.Fatal("expected rejection, got a profile")
			}
			want := tc.want
			if want == nil {
				want = ErrInvalidIDToken
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

// A matching azp is the common case and must not be rejected alongside the
// mismatched one.
func TestProfileFromIDTokenAcceptsMatchingAzp(t *testing.T) {
	cfg := Config{ClientID: testClientID}
	c := validClaims()
	c.Azp = testClientID

	p, err := cfg.profileFromIDToken(idToken(t, c), time.Now())
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if p.Sub != c.Sub || p.Email != c.Email || !p.EmailVerified {
		t.Fatalf("profile = %+v, want sub/email from claims", p)
	}
}

func TestExchange(t *testing.T) {
	tests := map[string]struct {
		status  int
		body    string
		wantErr bool
	}{
		"ok":             {status: http.StatusOK},
		"google 500":     {status: http.StatusInternalServerError, body: `{"error":"internal"}`, wantErr: true},
		"oauth error":    {status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, wantErr: true},
		"no id_token":    {status: http.StatusOK, body: `{"access_token":"x"}`, wantErr: true},
		"malformed json": {status: http.StatusOK, body: `{`, wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var gotVerifier string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parsing form: %v", err)
				}
				gotVerifier = r.Form.Get("code_verifier")

				body := tc.body
				if body == "" {
					body = `{"id_token":"` + idToken(t, validClaims()) + `"}`
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)

			cfg := Config{ClientID: testClientID, ClientSecret: "secret", tokenEndpoint: srv.URL}
			p, err := cfg.Exchange(context.Background(), "the-code", "the-verifier")

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Sub != "google-sub-1" {
				t.Fatalf("sub = %q", p.Sub)
			}
			// PKCE is only real if the verifier actually reaches Google.
			if gotVerifier != "the-verifier" {
				t.Fatalf("code_verifier = %q, want it forwarded", gotVerifier)
			}
		})
	}
}

// The token request carries the client secret, so a failure must never echo
// the response body back into an error a caller might log.
func TestExchangeErrorOmitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"secret-sauce-do-not-log"}`))
	}))
	t.Cleanup(srv.Close)

	cfg := Config{ClientID: testClientID, ClientSecret: "super-secret", tokenEndpoint: srv.URL}
	_, err := cfg.Exchange(context.Background(), "code", "verifier")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, leak := range []string{"secret-sauce-do-not-log", "super-secret"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error text leaked %q: %s", leak, err)
		}
	}
}
