package identity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testSigner() signer { return signer{secret: []byte("test-signing-secret")} }

func validToken(t *testing.T, s signer, exp time.Time) string {
	t.Helper()
	tok, err := s.mint(Claims{
		UserID: "usr_1", OrgID: "org_1", SessionID: "ses_1",
		IssuedAt: time.Now().Unix(), ExpiresAt: exp.Unix(),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

// The verifier must not be steerable by anything inside the token. alg:none and
// algorithm confusion are the two classic JWT breaks and both rely on a
// verifier that reads the header.
func TestVerifyRejects(t *testing.T) {
	s := testSigner()
	now := time.Now()
	good := validToken(t, s, now.Add(time.Hour))
	parts := strings.Split(good, ".")

	enc := base64.RawURLEncoding.EncodeToString
	noneHeader := enc([]byte(`{"alg":"none","typ":"JWT"}`))

	tests := map[string]struct {
		token string
		want  error
	}{
		"alg none with no signature": {token: noneHeader + "." + parts[1] + "."},
		"alg none with a signature":  {token: noneHeader + "." + parts[1] + "." + parts[2]},
		"tampered payload":           {token: parts[0] + "." + enc([]byte(`{"sub":"usr_evil","org":"org_evil","exp":99999999999}`)) + "." + parts[2]},
		"truncated signature":        {token: parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-4]},
		"empty signature":            {token: parts[0] + "." + parts[1] + "."},
		"no signature segment":       {token: parts[0] + parts[1]},
		"payload is not base64":      {token: parts[0] + ".!!!." + parts[2]},
		"signed by a different key":  {token: validToken(t, signer{secret: []byte("another-secret")}, now.Add(time.Hour))},
		"missing subject":            {token: mintRaw(t, s, `{"org":"org_1","exp":99999999999}`)},
		"expired": {
			token: validToken(t, s, now.Add(-time.Minute)),
			want:  ErrTokenExpired,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := s.verify(tc.token, now)
			if err == nil {
				t.Fatal("expected rejection, got claims")
			}
			want := tc.want
			if want == nil {
				want = ErrTokenInvalid
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

// mintRaw signs an arbitrary payload so a claim can be omitted entirely.
func mintRaw(t *testing.T, s signer, payload string) string {
	t.Helper()
	body := headerSegment + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	return body + "." + base64.RawURLEncoding.EncodeToString(s.sign(body))
}

func TestMintVerifyRoundTrip(t *testing.T) {
	s := testSigner()
	in := Claims{
		UserID: "usr_1", OrgID: "org_9", Superadmin: true, SessionID: "ses_3",
		IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
	}
	tok, err := s.mint(in)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	out, err := s.verify(tok, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if out != in {
		t.Fatalf("claims = %+v, want %+v", out, in)
	}
}

// A JWT is signed, not encrypted. Anything in the payload is readable by
// whoever holds the cookie, so PII must not be in there.
func TestTokenPayloadCarriesNoPII(t *testing.T) {
	s := testSigner()
	tok, err := s.mint(Claims{
		UserID: "usr_1", OrgID: "org_1", SessionID: "ses_1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("payload is not json: %v", err)
	}
	for _, banned := range []string{"email", "name", "avatar_url", "picture"} {
		if _, ok := fields[banned]; ok {
			t.Errorf("claims carry %q; the payload is world-readable", banned)
		}
	}
}
