// admintoken prints a session cookie for scripts that drive the admin API.
//
// The admin port takes sessions only, and a script cannot complete a Google
// sign-in. This is not a second authentication path: it mints an ordinary
// session and it needs JWT_SECRET, so anyone who can run it could already
// forge any session by hand. A team API key still reaches nothing on :9090.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	var (
		user = flag.String("user", "usr_script", "user id to put in the token")
		org  = flag.String("org", "personal", "organization id to put in the token")
		ttl  = flag.Duration("ttl", time.Hour, "how long the token is valid")
		cook = flag.Bool("cookie", true, "print a Cookie header value rather than the bare token")
	)
	flag.Parse()

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "JWT_SECRET is unset; it is the signing key for dashboard sessions")
		os.Exit(1)
	}

	now := time.Now()
	payload, err := json.Marshal(map[string]any{
		"sub": *user,
		"org": *org,
		"sa":  true,
		"sid": "ses_script",
		"iat": now.Unix(),
		"exp": now.Add(*ttl).Unix(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshalling claims: %v\n", err)
		os.Exit(1)
	}

	enc := base64.RawURLEncoding.EncodeToString
	body := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	token := body + "." + enc(mac.Sum(nil))

	if !*cook {
		fmt.Println(token)
		return
	}

	// The CSRF pair a mutating request needs: the cookie value and the header
	// are the same string, and the signature is what makes it unforgeable.
	nonce := enc([]byte(fmt.Sprintf("script-%d", now.UnixNano())))
	cmac := hmac.New(sha256.New, []byte(secret))
	cmac.Write([]byte(nonce))
	csrf := nonce + "." + enc(cmac.Sum(nil))

	fmt.Printf("sy_session=%s; sy_csrf=%s\n", token, csrf)
	fmt.Fprintf(os.Stderr, "X-CSRF-Token: %s\n", csrf)
}
