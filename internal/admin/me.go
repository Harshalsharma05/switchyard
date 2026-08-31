// Identity endpoint (Part 2, Step 2.1). The frontend calls GET /admin/me once
// on load to learn who its key belongs to and whether to render admin routes.
package admin

import (
	"log/slog"
	"net/http"
)

// RoutingInfo is the slice of internal/router this package needs: what a
// caller may name in `model` to opt into Step 8.2's routing. Nil when the
// gateway has no router wired.
type RoutingInfo interface {
	Options() []string
}

// meView is GET /admin/me's response: the caller's own team, plus the admin
// flag the request-log and gated routes branch on. It embeds teamView so the
// identity payload and the /admin/teams payload stay one shape.
type meView struct {
	teamView
	IsAdmin bool `json:"is_admin"`

	// RoutingOptions lists what this gateway accepts in `model` beyond real
	// model names — the auto keyword and each routable tier. Empty when
	// routing is off, so the UI omits the controls rather than offering ones
	// that would 404, and never hardcodes a tier name of its own.
	RoutingOptions []string `json:"routing_options,omitempty"`
}

// handleMe resolves the bearer key to its team and returns that team only —
// never another's. Any valid key works here; the admin flag is data in the
// response, not a gate on reaching it.
func handleMe(authr KeyAuthenticator, spend SpendReader, routing RoutingInfo, log *slog.Logger) http.HandlerFunc {
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

		var options []string
		if routing != nil {
			options = routing.Options()
		}

		view := meView{
			teamView:       newTeamView(*team, readSpent(r.Context(), spend, log, team.ID)),
			IsAdmin:        team.IsAdmin,
			RoutingOptions: options,
		}
		writeJSON(w, log, http.StatusOK, view)
	}
}
