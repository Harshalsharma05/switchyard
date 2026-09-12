// Package oauth is the Google OAuth 2.0 client: authorize URL, PKCE, code
// exchange, and the verified profile that comes back. It knows nothing about
// users, sessions, cookies, or this gateway's database.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"

	// openid and email are what identity needs; profile is only for the
	// display name and avatar.
	scopes = "openid email profile"

	exchangeTimeout = 10 * time.Second
	maxTokenBody    = 1 << 20
)

var (
	// ErrEmailNotVerified rejects a Google account whose address Google itself
	// has not verified. Under Google-only sign-in that verification is the
	// only proof of mailbox ownership anywhere in the system.
	ErrEmailNotVerified = errors.New("google account has no verified email")

	ErrInvalidIDToken = errors.New("google id token failed validation")

	validIssuers = map[string]bool{
		"accounts.google.com":         true,
		"https://accounts.google.com": true,
	}
)

// Config is one Google OAuth client. A nil HTTPClient gets a bounded default;
// http.DefaultClient is deliberately not used because it has no timeout.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTPClient   *http.Client

	// endpoints, when set, replace Google's. Only tests set them.
	authEndpoint  string
	tokenEndpoint string
}

// Profile is everything this gateway learns about a person from Google.
type Profile struct {
	Sub           string
	Email         string
	EmailVerified bool
	Name          string
	Picture       string
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: exchangeTimeout}
}

func (c Config) authURL() string {
	if c.authEndpoint != "" {
		return c.authEndpoint
	}
	return authEndpoint
}

func (c Config) tokenURL() string {
	if c.tokenEndpoint != "" {
		return c.tokenEndpoint
	}
	return tokenEndpoint
}

// AuthURL is where the browser is redirected to consent.
//
// access_type is deliberately omitted, so Google never issues a refresh token:
// after login this gateway mints its own session and never calls Google again,
// and a credential nobody needs is a credential nobody has to protect.
func (c Config) AuthURL(state, verifier string) string {
	q := url.Values{
		"client_id":             {c.ClientID},
		"redirect_uri":          {c.RedirectURL},
		"response_type":         {"code"},
		"scope":                 {scopes},
		"state":                 {state},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	return c.authURL() + "?" + q.Encode()
}

// Exchange trades an authorization code for the caller's verified profile.
func (c Config) Exchange(ctx context.Context, code, verifier string) (Profile, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"redirect_uri":  {c.RedirectURL},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return Profile{}, fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client().Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("calling google token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	if err != nil {
		return Profile{}, fmt.Errorf("reading token response: %w", err)
	}

	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return Profile{}, fmt.Errorf("decoding token response: status %d", resp.StatusCode)
	}
	// Only the error code is surfaced, never the body: this is the one request
	// that carries the client secret, and an echoed body is how it would come
	// back out into a log line.
	if resp.StatusCode != http.StatusOK || tok.Error != "" {
		return Profile{}, fmt.Errorf("google token endpoint: status %d, error %q", resp.StatusCode, tok.Error)
	}
	if tok.IDToken == "" {
		return Profile{}, fmt.Errorf("google token response carried no id_token: status %d", resp.StatusCode)
	}

	return c.profileFromIDToken(tok.IDToken, time.Now())
}

// claims is the subset of the ID token this gateway reads.
type claims struct {
	Iss           string `json:"iss"`
	Aud           string `json:"aud"`
	Azp           string `json:"azp"`
	Sub           string `json:"sub"`
	Exp           int64  `json:"exp"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// profileFromIDToken decodes the ID token's claims WITHOUT VERIFYING ITS
// SIGNATURE, then validates them.
//
// ---------------------------------------------------------------------------
// THIS IS SAFE ONLY BECAUSE OF HOW THE TOKEN ARRIVED.
//
// Exchange received it directly from Google's token endpoint, over TLS, in
// exchange for this client's secret. That channel already authenticates the
// issuer and no untrusted party ever touched the token, so verifying the
// signature would prove something TLS has already proven -- at the cost of
// fetching, caching, and rotating Google's JWKS.
//
// IF AN ID TOKEN EVER REACHES THIS CODE FROM ANYWHERE ELSE -- a browser, a
// client SDK, a webhook, a mobile app -- THAT REASONING COLLAPSES AND
// SIGNATURE VERIFICATION AGAINST GOOGLE'S JWKS BECOMES MANDATORY. Do not call
// this function on such a token.
// ---------------------------------------------------------------------------
func (c Config) profileFromIDToken(idToken string, now time.Time) (Profile, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Profile{}, fmt.Errorf("%w: not three segments", ErrInvalidIDToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Profile{}, fmt.Errorf("%w: payload is not base64url", ErrInvalidIDToken)
	}

	var cl claims
	if err := json.Unmarshal(payload, &cl); err != nil {
		return Profile{}, fmt.Errorf("%w: payload is not json", ErrInvalidIDToken)
	}

	if !validIssuers[cl.Iss] {
		return Profile{}, fmt.Errorf("%w: issuer %q", ErrInvalidIDToken, cl.Iss)
	}
	if cl.Aud != c.ClientID {
		return Profile{}, fmt.Errorf("%w: audience is not this client", ErrInvalidIDToken)
	}
	// azp names the party the token was issued to. Google omits it in the
	// common case and sets it where it can differ from aud; when present it
	// must still be this client.
	if cl.Azp != "" && cl.Azp != c.ClientID {
		return Profile{}, fmt.Errorf("%w: authorized party is not this client", ErrInvalidIDToken)
	}
	if cl.Exp <= 0 || now.After(time.Unix(cl.Exp, 0)) {
		return Profile{}, fmt.Errorf("%w: expired", ErrInvalidIDToken)
	}
	if cl.Sub == "" || cl.Email == "" {
		return Profile{}, fmt.Errorf("%w: missing sub or email", ErrInvalidIDToken)
	}
	if !cl.EmailVerified {
		return Profile{}, ErrEmailNotVerified
	}

	return Profile{
		Sub:           cl.Sub,
		Email:         cl.Email,
		EmailVerified: true,
		Name:          cl.Name,
		Picture:       cl.Picture,
	}, nil
}

// NewState returns an opaque CSRF value for one login attempt.
func NewState() (string, error) { return randomToken() }

// NewVerifier returns a PKCE code verifier (RFC 7636).
func NewVerifier() (string, error) { return randomToken() }

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
