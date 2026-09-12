// The /auth endpoints on the admin port: start a Google sign-in, and handle
// the callback. Mounted by cmd/ so internal/admin never imports this package.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/oauth"
)

const (
	// stateCookie carries the OAuth state and the PKCE verifier between the
	// two legs of the flow. Short-lived, signed, and deleted on callback.
	stateCookie = "sy_oauth"
	stateTTL    = 10 * time.Minute

	// signinFailed is the only failure the browser is ever told about. Every
	// distinguishable reason -- a taken email, a database outage, a rejected
	// token -- collapses into it, so sign-in cannot be used to discover which
	// addresses are registered.
	signinFailed = "signin_failed"
)

// Handlers serves the OAuth flow.
type Handlers struct {
	oauth  oauth.Config
	store  *Store
	secret []byte
	log    *slog.Logger

	// successURL and failureURL are where the browser lands after the
	// callback. Both are same-origin paths, never taken from the request.
	successURL string
	failureURL string

	// secure sets the Secure flag on cookies. Off for plain-HTTP local dev.
	secure bool
}

func NewHandlers(cfg oauth.Config, store *Store, secret []byte, successURL, failureURL string, secure bool, log *slog.Logger) *Handlers {
	return &Handlers{
		oauth:      cfg,
		store:      store,
		secret:     secret,
		log:        log,
		successURL: successURL,
		failureURL: failureURL,
		secure:     secure,
	}
}

func (h *Handlers) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/google", h.start)
	r.Get("/google/callback", h.callback)
	return r
}

// start mints a state and a PKCE verifier, parks them in a signed cookie, and
// redirects to Google.
func (h *Handlers) start(w http.ResponseWriter, r *http.Request) {
	state, err := oauth.NewState()
	if err != nil {
		h.log.Error("generating oauth state", slog.Any("error", err))
		h.fail(w, r)
		return
	}
	verifier, err := oauth.NewVerifier()
	if err != nil {
		h.log.Error("generating pkce verifier", slog.Any("error", err))
		h.fail(w, r)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:  stateCookie,
		Value: h.seal(state, verifier),
		Path:  "/auth",
		// Lax, not Strict: Google's callback is a cross-site top-level
		// navigation, and Strict would withhold the cookie on exactly the
		// request that needs it.
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   h.secure,
		MaxAge:   int(stateTTL.Seconds()),
	})

	http.Redirect(w, r, h.oauth.AuthURL(state, verifier), http.StatusFound)
}

// callback verifies state, exchanges the code, and resolves the user.
func (h *Handlers) callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(stateCookie)
	h.clearState(w)
	if err != nil {
		h.reject(w, "missing_state", "no sign-in is in progress for this browser")
		return
	}

	state, verifier, ok := h.open(cookie.Value)
	if !ok {
		h.reject(w, "invalid_state", "the sign-in state could not be verified")
		return
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(r.URL.Query().Get("state"))) != 1 {
		h.reject(w, "state_mismatch", "the sign-in state did not match")
		return
	}

	// The user declined consent, or Google refused. Not an error on our side.
	if e := r.URL.Query().Get("error"); e != "" {
		h.log.Info("google sign-in not completed", slog.String("reason", e))
		h.fail(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		h.reject(w, "missing_code", "the sign-in response carried no authorization code")
		return
	}

	profile, err := h.oauth.Exchange(r.Context(), code, verifier)
	if err != nil {
		if errors.Is(err, oauth.ErrEmailNotVerified) {
			h.log.Info("rejected google account with an unverified email")
		} else {
			h.log.Error("exchanging google authorization code", slog.Any("error", err))
		}
		h.fail(w, r)
		return
	}

	user, created, err := h.store.FindOrCreateGoogleUser(r.Context(), profile)
	if err != nil {
		// ErrEmailTaken is logged and never reflected: the browser gets the
		// same generic failure a database outage produces.
		h.log.Error("resolving google user", slog.Any("error", err))
		h.fail(w, r)
		return
	}

	// User and org IDs, never the email: the address is PII and the ID is what
	// an operator would actually grep for.
	h.log.Info("google sign-in",
		slog.String("user", user.ID),
		slog.String("organization", user.OrganizationID),
		slog.Bool("created", created),
		slog.Bool("superadmin", user.IsSuperadmin),
	)

	// Step 1.4 issues the session cookie here. Until then the callback proves
	// the flow and the user row, and lands the browser on the dashboard.
	http.Redirect(w, r, h.successURL, http.StatusFound)
}

// reject answers a state failure outright. No redirect and no retry: a missing
// or mismatched state is a forged callback, not a user who mistyped something.
func (h *Handlers) reject(w http.ResponseWriter, errType, message string) {
	h.log.Warn("rejected oauth callback", slog.String("reason", errType))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	body := map[string]any{"error": map[string]string{"message": message, "type": errType}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.Warn("writing oauth error response", slog.Any("error", err))
	}
}

// fail sends the browser back to the login page with one opaque reason.
func (h *Handlers) fail(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, h.failureURL+"?error="+signinFailed, http.StatusFound)
}

func (h *Handlers) clearState(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    "",
		Path:     "/auth",
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   h.secure,
		MaxAge:   -1,
	})
}

// seal encodes state and verifier with an HMAC over both. The cookie is
// client-held, so without the signature it is attacker-controlled input.
func (h *Handlers) seal(state, verifier string) string {
	payload := state + "." + verifier
	return payload + "." + base64.RawURLEncoding.EncodeToString(h.sign(payload))
}

func (h *Handlers) open(value string) (state, verifier string, ok bool) {
	i := strings.LastIndex(value, ".")
	if i < 0 {
		return "", "", false
	}
	payload, sig := value[:i], value[i+1:]
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(want, h.sign(payload)) {
		return "", "", false
	}
	state, verifier, found := strings.Cut(payload, ".")
	if !found || state == "" || verifier == "" {
		return "", "", false
	}
	return state, verifier, true
}

func (h *Handlers) sign(payload string) []byte {
	m := hmac.New(sha256.New, h.secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
