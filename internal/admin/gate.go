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
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
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

// actorID is the audit and log "who". It is the user ID now rather than a team
// ID -- forced, not chosen: after Step 1.5 there is no team on an admin request.
func actorID(r *http.Request) string {
	if c, ok := caller(r); ok && c.UserID != "" {
		return c.UserID
	}
	return "unknown"
}

// stampActor fills in an audit entry's identity fields from the session: who
// acted, which tenant the change belongs to, and whether it was a superadmin
// action.
//
// targetOrg is the organisation of the resource being changed, and it is what
// the entry is filed under — never the actor's own organisation. That is the
// whole mechanism behind tenant-visible operator actions: a superadmin rotating
// another organisation's key files the entry against that organisation, so the
// organisation can see it happened.
//
// An empty targetOrg is left empty, stored as NULL, which belongs to no tenant
// and is therefore superadmin-only. Actions with nothing to target — a config
// reload — are exactly the ones no single tenant should see.
//
// Every mutating handler records through here, so no call site decides these
// three fields for itself.
func stampActor(r *http.Request, targetOrg string, e logstore.AuditEntry) logstore.AuditEntry {
	c, _ := caller(r)

	e.ActorUserID = actorID(r)
	e.ActorAddr = r.RemoteAddr
	e.Superadmin = c.IsSuperadmin
	e.OrganizationID = targetOrg
	return e
}

// requireSuperadmin gates what genuinely crosses tenants or changes the whole
// deployment: config reload, chaos injection, breaker resets, the system panel,
// the cross-tenant cache sweep, and the audit log until Step 2.5 scopes it.
//
// Step 2.4's split is the other half of this. Everything that is "admin of my
// own organisation" — managing teams, their keys and budgets, reconciliation,
// quality feedback — left this gate and is scoped by the caller's org claim
// instead. The question asked of every endpoint was which of the two it is, and
// the answer was "org admin" far more often than not.
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

// orgScope resolves which team IDs a caller may see on the cross-team read
// endpoints.
//
// A superadmin reads every team, or narrows with the same ?team= filter an
// admin key used before this existed — nil comes back, which every caller
// below this point reads as "no filter." Everyone else is scoped to every
// team in their own organisation: queries.List() is an in-memory snapshot
// (internal/teamstore does no I/O for a read), so filtering it in Go costs
// nothing worth measuring and needs no new index. The returned slice is
// always non-nil for a non-superadmin, even when their org has no teams yet
// — an empty, non-nil slice means "match nothing," which is the correct
// answer for a brand-new org, not "match everything."
//
// Multi-user Step 3.3's project switcher needs a non-superadmin to narrow to
// one team too, so ?team= is honoured for any caller — but only when the
// requested team is one of their own. A foreign or unknown id falls back to
// the full org scope rather than an error: this is a display filter, not a
// resource lookup, so there is nothing to 404 and no reason to hint at
// whether the id exists (same instinct as teamInScope's 404-not-403 below,
// applied to a filter instead of a path parameter).
func orgScope(w http.ResponseWriter, r *http.Request, teams TeamStore, log *slog.Logger) ([]string, bool) {
	c, ok := caller(r)
	if !ok {
		writeError(w, log, http.StatusUnauthorized, "no_session",
			"this endpoint requires a signed-in session")
		return nil, false
	}

	if requested := r.URL.Query().Get("team"); requested != "" {
		if c.IsSuperadmin {
			return []string{requested}, true
		}
		for _, t := range teams.List() {
			if t.ID == requested && t.OrganizationID == c.OrgID {
				return []string{requested}, true
			}
		}
		// Not one of the caller's own teams — fall through to the unfiltered
		// org scope below.
	} else if c.IsSuperadmin {
		return nil, true
	}

	ids := []string{}
	for _, t := range teams.List() {
		if t.OrganizationID == c.OrgID {
			ids = append(ids, t.ID)
		}
	}
	sort.Strings(ids)
	return ids, true
}

// teamInScope resolves a {id} path parameter to a team the caller is allowed to
// act on, which is every team in their own organisation — or any team at all
// for a superadmin.
//
// A team belonging to another organisation is reported as 404, identical to a
// team that does not exist. Not 403: a 403 would confirm the ID is real, which
// tells the caller something about another tenant even when the request is
// refused. Every handler taking an {id} resolves it through here, so the rule
// lives in one place rather than being re-derived eight times.
func teamInScope(w http.ResponseWriter, r *http.Request, store TeamStore, log *slog.Logger) (auth.Team, bool) {
	id := chi.URLParam(r, "id")

	c, ok := caller(r)
	if !ok {
		writeError(w, log, http.StatusUnauthorized, "no_session",
			"this endpoint requires a signed-in session")
		return auth.Team{}, false
	}

	team, err := store.Get(id)
	if err != nil {
		writeTeamLookupError(w, log, id, err)
		return auth.Team{}, false
	}

	if !c.IsSuperadmin && team.OrganizationID != c.OrgID {
		writeError(w, log, http.StatusNotFound, "team_not_found", "no such team "+id)
		return auth.Team{}, false
	}
	return team, true
}
