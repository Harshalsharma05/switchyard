// The access token: a compact HS256 JWT, minted and verified here rather than
// by a library.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrTokenInvalid = errors.New("access token failed verification")
	ErrTokenExpired = errors.New("access token expired")
)

// headerSegment is emitted so the token is a well-formed JWT for anything that
// inspects one. It is never read back -- see verify.
var headerSegment = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// Claims is the whole payload. Nothing here is sensitive: a JWT is signed, not
// encrypted, so anyone holding the cookie can read every field. Email, name,
// and avatar are deliberately absent and come from GET /auth/me instead.
type Claims struct {
	UserID     string `json:"sub"`
	OrgID      string `json:"org"`
	Superadmin bool   `json:"sa"`
	SessionID  string `json:"sid"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

type signer struct{ secret []byte }

func (s signer) mint(c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshalling claims: %w", err)
	}
	body := headerSegment + "." + base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.sign(body)), nil
}

// verify checks the signature and expiry and returns the claims.
//
// THE HEADER IS NEVER PARSED. Not read, not decoded, not checked -- the
// algorithm is a property of this code, not of the token. Every JWT
// vulnerability of note comes from a verifier that branches on a field the
// token itself supplies: "alg":"none" accepted as valid, or an RS256 token
// verified as HS256 with the public key used as the HMAC secret. A verifier
// that always computes HMAC-SHA256 with this server's key cannot be steered,
// because there is nothing to steer.
//
// If this function ever grows a branch on anything the token carries outside
// the verified payload, that immunity is gone.
func (s signer) verify(token string, now time.Time) (Claims, error) {
	i := strings.LastIndex(token, ".")
	if i < 0 {
		return Claims{}, fmt.Errorf("%w: no signature segment", ErrTokenInvalid)
	}
	body, sig := token[:i], token[i+1:]

	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(body)) {
		return Claims{}, fmt.Errorf("%w: signature mismatch", ErrTokenInvalid)
	}

	_, encoded, ok := strings.Cut(body, ".")
	if !ok {
		return Claims{}, fmt.Errorf("%w: no payload segment", ErrTokenInvalid)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: payload is not base64url", ErrTokenInvalid)
	}

	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, fmt.Errorf("%w: payload is not json", ErrTokenInvalid)
	}
	if c.UserID == "" || c.OrgID == "" {
		return Claims{}, fmt.Errorf("%w: missing subject or organization", ErrTokenInvalid)
	}
	if c.ExpiresAt <= 0 || now.After(time.Unix(c.ExpiresAt, 0)) {
		return Claims{}, ErrTokenExpired
	}
	return c, nil
}

func (s signer) sign(body string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(body))
	return m.Sum(nil)
}
