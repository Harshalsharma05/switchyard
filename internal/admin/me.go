// Identity endpoint (Part 2, Step 2.1). The frontend calls GET /admin/me once
// on load to learn who its key belongs to and whether to render admin routes.
package admin

import (
	"log/slog"
	"net/http"
)

// meView is GET /admin/me's response: the caller's own team, plus the admin
// flag the request-log and gated routes branch on. It embeds teamView so the
// identity payload and the /admin/teams payload stay one shape.
type meView struct {
	teamView
	IsAdmin bool `json:"is_admin"`
}

// handleMe resolves the bearer key to its team and returns that team only —
// never another's. Any valid key works here; the admin flag is data in the
// response, not a gate on reaching it.
func handleMe(authr KeyAuthenticator, spend SpendReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authr == nil {
			writeError(w, log, http.StatusServiceUnavailable, "auth_disabled",
				"this gateway has no team registry wired; /admin/me is unavailable")
			return
		}

		team, ok := authenticate(w, r, authr, log)
		if !ok {
			return
		}

		view := meView{
			teamView: newTeamView(*team, readSpent(r.Context(), spend, log, team.ID)),
			IsAdmin:  team.IsAdmin,
		}
		writeJSON(w, log, http.StatusOK, view)
	}
}
