// GET /auth/me -- who the session belongs to.
package identity

import (
	"net/http"
	"time"
)

// meView is what the console loads on startup. The access token is httpOnly, so
// the frontend cannot decode it; this call is how session state reaches the UI.
type meView struct {
	User struct {
		ID             string `json:"id"`
		Email          string `json:"email"`
		Name           string `json:"name"`
		AvatarURL      string `json:"avatar_url"`
		OrganizationID string `json:"organization_id"`
		IsSuperadmin   bool   `json:"is_superadmin"`
	} `json:"user"`

	// SessionExpiresAt lets the console schedule a refresh before the token
	// dies rather than discovering it through a failed request. Not sensitive:
	// it is a timestamp, and the holder of the cookie already has the session.
	SessionExpiresAt time.Time `json:"session_expires_at"`

	RoutingOptions []string `json:"routing_options,omitempty"`
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeJSONError(w, h.log, http.StatusUnauthorized, "no_session", "not signed in")
		return
	}
	claims, err := h.signer.verify(cookie.Value, time.Now())
	if err != nil {
		writeJSONError(w, h.log, http.StatusUnauthorized, "session_invalid", "sign in again")
		return
	}

	u, err := h.store.UserByID(r.Context(), claims.UserID)
	if err != nil {
		// The token verified but its subject is gone. Treat as signed out
		// rather than as a server error.
		h.clearSession(w)
		writeJSONError(w, h.log, http.StatusUnauthorized, "session_invalid", "sign in again")
		return
	}

	var v meView
	v.User.ID = u.ID
	v.User.Email = u.Email
	v.User.Name = u.Name
	v.User.AvatarURL = u.AvatarURL
	v.User.OrganizationID = u.OrganizationID
	v.User.IsSuperadmin = u.IsSuperadmin
	v.SessionExpiresAt = time.Unix(claims.ExpiresAt, 0).UTC()
	if h.cfg.RoutingOptions != nil {
		v.RoutingOptions = h.cfg.RoutingOptions()
	}

	writeJSON(w, h.log, http.StatusOK, v)
}
