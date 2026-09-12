// Signed double-submit CSRF protection for mutating requests.
package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// CSRFHeader is what the console echoes the sy_csrf cookie back in. The cookie
// is readable by JavaScript on purpose -- the defence is that a cross-origin
// script can send our cookie but cannot read it, so it cannot produce this
// header.
const CSRFHeader = "X-CSRF-Token"

// newCSRFToken returns "nonce.signature". Signing it is what stops someone who
// can write a cookie on a sibling host from injecting a matching pair of their
// own: they can choose the value, but not a signature for it.
func (h *Handlers) newCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating csrf token: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(b)
	return nonce + "." + base64.RawURLEncoding.EncodeToString(h.sign(nonce)), nil
}

func (h *Handlers) validCSRFToken(v string) bool {
	nonce, sig, ok := strings.Cut(v, ".")
	if !ok || nonce == "" {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	return err == nil && hmac.Equal(got, h.sign(nonce))
}

// RequireCSRF rejects a mutating request whose X-CSRF-Token header does not
// match its sy_csrf cookie. Safe methods are exempt: they do not change state,
// and requiring a header on them would break plain navigation.
//
// Applied to whole route groups rather than to individual handlers -- a
// partially protected mutation surface is not protected.
func (h *Handlers) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(csrfCookie)
		header := r.Header.Get(CSRFHeader)
		switch {
		case err != nil || cookie.Value == "":
			h.rejectCSRF(w, r, "no_csrf_cookie")
		case header == "":
			h.rejectCSRF(w, r, "no_csrf_header")
		case !h.validCSRFToken(cookie.Value):
			h.rejectCSRF(w, r, "unsigned_csrf_cookie")
		case subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1:
			h.rejectCSRF(w, r, "csrf_mismatch")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (h *Handlers) rejectCSRF(w http.ResponseWriter, r *http.Request, reason string) {
	h.log.Warn("rejected request failing the csrf check",
		slog.String("reason", reason),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
	writeJSONError(w, h.log, http.StatusForbidden, "csrf_failed",
		"this request did not carry a matching CSRF token")
}
