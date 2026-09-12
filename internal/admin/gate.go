// The admin port's caller: a signed-in dashboard user (Multi-user, Step 1.5).
//
// Part 2 authenticated this surface with a team API key. It now takes a session
// only, and a team key reaches nothing here -- the session middleware does not
// read the Authorization header at all. The middleware lives in
// internal/identity; this package never imports it, and cmd/ supplies the
// adapter that fills in the Caller below.
package admin

import (
	"context"
	"log/slog"
	"net/http"
)

// Caller is the authenticated user behind an admin request.
type Caller struct {
	UserID       string
	OrgID        string
	IsSuperadmin bool
	SessionID    string
}

type callerCtxKey struct{}

// WithCaller is called by cmd/'s session adapter. Exported because the writer
// lives outside this package; the key stays unexported so nothing else can
// forge one.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

func caller(r *http.Request) (Caller, bool) {
	c, ok := r.Context().Value(callerCtxKey{}).(Caller)
	return c, ok
}

// actorID is the audit "who". It is the user ID now rather than a team ID --
// forced, not chosen: after Step 1.5 there is no team on an admin request.
// The plan puts this in Phase 2, Step 2.5; there is nothing else it could be.
func actorID(r *http.Request) string {
	if c, ok := caller(r); ok {
		return c.UserID
	}
	return "unknown"
}

// requireSuperadmin gates everything that crosses tenants: team management,
// chaos, reload, audit, the system panel.
//
// Every signed-in user is an admin of their own organisation, but nothing here
// is organisation-scoped yet, so the fail-closed reading is the only safe one
// until Phase 2 splits org admin from superadmin.
func requireSuperadmin(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := caller(r)
			if !ok {
				writeError(w, log, http.StatusUnauthorized, "no_session",
					"this endpoint requires a signed-in session")
				return
			}
			if !c.IsSuperadmin {
				writeError(w, log, http.StatusForbidden, "superadmin_required",
					"this endpoint is superadmin-only until organisation scoping ships")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// teamScope resolves the ?team= filter the cross-team read endpoints accept.
//
// A superadmin reads any team or all of them, which is exactly what an admin
// key did before. Everyone else is refused rather than served unscoped: these
// queries have no organisation filter yet, so the alternative would hand a new
// user every other organisation's rows. Phase 2, Step 2.2 replaces this with a
// real org filter.
func teamScope(w http.ResponseWriter, r *http.Request, log *slog.Logger) (string, bool) {
	c, ok := caller(r)
	if !ok {
		writeError(w, log, http.StatusUnauthorized, "no_session",
			"this endpoint requires a signed-in session")
		return "", false
	}
	if !c.IsSuperadmin {
		writeError(w, log, http.StatusForbidden, "org_scope_pending",
			"organisation-scoped views are not available yet; this endpoint is superadmin-only")
		return "", false
	}
	return r.URL.Query().Get("team"), true
}
