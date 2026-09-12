// The /auth endpoints on the admin port: Google sign-in, refresh, and logout.
// Mounted by cmd/ so internal/admin never imports this package.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/oauth"
)

const (
	// stateCookie carries the OAuth state and the PKCE verifier between the
	// two legs of the flow. Short-lived, signed, deleted on callback.
	stateCookie = "sy_oauth"
	stateTTL    = 10 * time.Minute

	sessionCookie = "sy_session"
	refreshCookie = "sy_refresh"
	csrfCookie    = "sy_csrf"

	// refreshPath keeps the long-lived credential off every ordinary dashboard
	// request. The browser sends it only to /auth/refresh and /auth/logout.
	refreshPath = "/auth"

	// signinFailed is the only failure the browser is ever told about. Every
	// distinguishable reason -- a taken email, a database outage, a rejected
	// token -- collapses into it, so sign-in cannot be used to discover which
	// addresses are registered.
	signinFailed = "signin_failed"
)

// Config is everything the auth endpoints need. A struct rather than a
// parameter list because this grew past the point where positional arguments
// read clearly.
type Config struct {
	OAuth      oauth.Config
	Secret     []byte
	SuccessURL string
	FailureURL string
	Secure     bool
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	// RoutingOptions is what GET /auth/me reports the gateway accepts in
	// `model` beyond real model names. Supplied by cmd/; nil when routing is
	// off.
	RoutingOptions func() []string
}

type Handlers struct {
	cfg    Config
	store  *Store
	signer signer
	log    *slog.Logger
}

func NewHandlers(cfg Config, store *Store, log *slog.Logger) *Handlers {
	return &Handlers{cfg: cfg, store: store, signer: signer{secret: cfg.Secret}, log: log}
}

func (h *Handlers) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/google", h.start)
	r.Get("/google/callback", h.callback)
	r.Get("/me", h.me)

	// Both mutate server state, so both sit behind the CSRF check. The /admin
	// group joins them in Step 1.5, when it moves from team keys to sessions.
	r.Group(func(r chi.Router) {
		r.Use(h.RequireCSRF)
		r.Post("/refresh", h.refresh)
		r.Post("/logout", h.logout)
	})
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
		Path:  refreshPath,
		// Lax, not Strict: Google's callback is a cross-site top-level
		// navigation, and Strict would withhold the cookie on exactly the
		// request that needs it.
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   h.cfg.Secure,
		MaxAge:   int(stateTTL.Seconds()),
	})

	http.Redirect(w, r, h.cfg.OAuth.AuthURL(state, verifier), http.StatusFound)
}

// callback verifies state, exchanges the code, resolves the user, and issues
// the session.
func (h *Handlers) callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(stateCookie)
	h.clearCookie(w, stateCookie, refreshPath)
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

	profile, err := h.cfg.OAuth.Exchange(r.Context(), code, verifier)
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

	if err := h.issue(w, r, user); err != nil {
		h.log.Error("issuing session", slog.Any("error", err))
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
	http.Redirect(w, r, h.cfg.SuccessURL, http.StatusFound)
}

// refresh rotates the refresh token and mints a new access token.
func (h *Handlers) refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookie)
	if err != nil || cookie.Value == "" {
		h.clearSession(w)
		writeJSONError(w, h.log, http.StatusUnauthorized, "no_session", "no session to refresh")
		return
	}

	user, sess, err := h.store.RotateSession(r.Context(), cookie.Value,
		r.UserAgent(), clientIP(r), h.cfg.RefreshTTL)
	if err != nil {
		h.clearSession(w)
		if errors.Is(err, ErrTokenReused) {
			// Every session for this user has just been revoked. Loud, because
			// it is either a client bug or a stolen token and both matter.
			h.log.Warn("refresh token replayed; revoked every session for the user")
		} else if !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrSessionExpired) {
			h.log.Error("rotating session", slog.Any("error", err))
		}
		writeJSONError(w, h.log, http.StatusUnauthorized, "session_invalid", "sign in again")
		return
	}

	if err := h.setSessionCookies(w, user, sess, false); err != nil {
		h.log.Error("minting access token on refresh", slog.Any("error", err))
		writeJSONError(w, h.log, http.StatusInternalServerError, "refresh_failed", "could not refresh the session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logout revokes the session server-side and clears the cookies.
//
// The cookies are cleared even when the revoke fails, so the browser is always
// logged out; the 503 then says truthfully that the access token may still be
// accepted for the rest of its lifetime.
func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookie)
	h.clearSession(w)
	if err != nil || cookie.Value == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := h.store.RevokeSession(r.Context(), cookie.Value); err != nil {
		h.log.Error("revoking session on logout", slog.Any("error", err))
		writeJSONError(w, h.log, http.StatusServiceUnavailable, "revoke_failed",
			"signed out in this browser, but the session could not be revoked on the server")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// issue creates a session row and sets all three cookies.
func (h *Handlers) issue(w http.ResponseWriter, r *http.Request, u User) error {
	sess, err := h.store.CreateSession(r.Context(), u.ID, r.UserAgent(), clientIP(r), h.cfg.RefreshTTL)
	if err != nil {
		return err
	}
	return h.setSessionCookies(w, u, sess, true)
}

// setSessionCookies writes the access, refresh, and CSRF cookies. The CSRF
// token is only regenerated at login: rotating it on every refresh would
// invalidate the value another open tab is holding.
func (h *Handlers) setSessionCookies(w http.ResponseWriter, u User, sess Session, newCSRF bool) error {
	now := time.Now()
	token, err := h.signer.mint(Claims{
		UserID:     u.ID,
		OrgID:      u.OrganizationID,
		Superadmin: u.IsSuperadmin,
		SessionID:  sess.ID,
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(h.cfg.AccessTTL).Unix(),
	})
	if err != nil {
		return err
	}

	h.setCookie(w, sessionCookie, token, "/", int(h.cfg.AccessTTL.Seconds()), true)
	h.setCookie(w, refreshCookie, sess.Raw, refreshPath, int(h.cfg.RefreshTTL.Seconds()), true)

	if newCSRF {
		csrf, err := h.newCSRFToken()
		if err != nil {
			return err
		}
		// Readable by the console on purpose: it has to echo this value back
		// in a header, which a cross-origin script cannot do.
		h.setCookie(w, csrfCookie, csrf, "/", int(h.cfg.RefreshTTL.Seconds()), false)
	}
	return nil
}

func (h *Handlers) setCookie(w http.ResponseWriter, name, value, path string, maxAge int, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		SameSite: http.SameSiteLaxMode,
		HttpOnly: httpOnly,
		Secure:   h.cfg.Secure,
		MaxAge:   maxAge,
	})
}

func (h *Handlers) clearSession(w http.ResponseWriter) {
	h.clearCookie(w, sessionCookie, "/")
	h.clearCookie(w, refreshCookie, refreshPath)
	h.clearCookie(w, csrfCookie, "/")
}

func (h *Handlers) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		SameSite: http.SameSiteLaxMode,
		HttpOnly: name != csrfCookie,
		Secure:   h.cfg.Secure,
		MaxAge:   -1,
	})
}

// reject answers a state failure outright. No redirect and no retry: a missing
// or mismatched state is a forged callback, not a user who mistyped something.
func (h *Handlers) reject(w http.ResponseWriter, errType, message string) {
	h.log.Warn("rejected oauth callback", slog.String("reason", errType))
	writeJSONError(w, h.log, http.StatusBadRequest, errType, message)
}

// fail sends the browser back to the login page with one opaque reason.
func (h *Handlers) fail(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, h.cfg.FailureURL+"?error="+signinFailed, http.StatusFound)
}

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Warn("writing auth response", slog.Any("error", err))
	}
}

func writeJSONError(w http.ResponseWriter, log *slog.Logger, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"error": map[string]string{"message": message, "type": errType}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Warn("writing auth error response", slog.Any("error", err))
	}
}

// clientIP records who signed in. Deliberately RemoteAddr and not
// X-Forwarded-For: that header is caller-supplied and trusting it would let
// anyone write whatever they liked into the session row.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return truncate(r.RemoteAddr, 64)
	}
	return truncate(host, 64)
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
	m := hmac.New(sha256.New, h.cfg.Secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
