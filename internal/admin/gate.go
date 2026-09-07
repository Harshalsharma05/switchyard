// Admin-only route gate (Part 2, Step 2.1).
//
// Part 1 left the admin listener as unauthenticated operator surface. Part 2's
// frontend reaches it with a team key, so the mutating and cross-team routes
// now demand one: 401 without a valid key, 403 with a non-admin one. The read
// paths that a non-admin legitimately needs — /admin/me, its own request-log
// rows — do their own scoping and are not gated here.
package admin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

// adminTeamCtxKey carries the authenticated admin team from requireAdmin down to
// the handler, so a mutating handler can attribute an audit entry to a real
// identity instead of just a remote address. Unexported key type — nothing
// outside this package sets or reads it.
type adminTeamCtxKey struct{}

// adminTeam returns the team requireAdmin authenticated, or nil when the gate
// was inert (a registry-less test build).
func adminTeam(r *http.Request) *auth.Team {
	t, _ := r.Context().Value(adminTeamCtxKey{}).(*auth.Team)
	return t
}

// actorID is the audit "who": the authenticated team's ID, or "unknown" when
// the gate is inert.
func actorID(r *http.Request) string {
	if t := adminTeam(r); t != nil {
		return t.ID
	}
	return "unknown"
}

// requireAdmin rejects any request whose bearer key is missing, unknown, or
// belongs to a non-admin team. When authr is nil the gate is inert — the port
// falls back to Part 1's open operator surface, the same way a nil request-log
// reader disables those routes. cmd/gateway always wires a registry, so a
// deployed gateway is always gated.
func requireAdmin(authr KeyAuthenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if authr == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			team, ok := authenticate(w, r, authr, log)
			if !ok {
				return
			}
			if !team.IsAdmin {
				writeError(w, log, http.StatusForbidden, "admin_required",
					"this endpoint requires an admin team key")
				return
			}
			ctx := context.WithValue(r.Context(), adminTeamCtxKey{}, team)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
