// Admin-only route gate (Part 2, Step 2.1).
//
// Part 1 left the admin listener as unauthenticated operator surface. Part 2's
// frontend reaches it with a team key, so the mutating and cross-team routes
// now demand one: 401 without a valid key, 403 with a non-admin one. The read
// paths that a non-admin legitimately needs — /admin/me, its own request-log
// rows — do their own scoping and are not gated here.
package admin

import (
	"log/slog"
	"net/http"
)

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
			next.ServeHTTP(w, r)
		})
	}
}
