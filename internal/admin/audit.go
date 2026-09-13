// Audit log: GET /admin/audit, and the recorder every mutating handler writes
// through (Part 2, Step 6.4 backend).
package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

// AuditRecorder is the audit store this package needs: append one entry, read
// one page. Declared here by the consumer, like RequestLogReader — and returning
// logstore's own types, because internal/logstore must never import this
// package. Nil when Postgres is unconfigured, which is not an error: the
// gateway falls back to the slog-only audit trail it has always had.
type AuditRecorder interface {
	RecordAudit(ctx context.Context, e logstore.AuditEntry) error
	ListAudit(ctx context.Context, q logstore.AuditQuery) (logstore.AuditPage, error)
}

type auditEntryView struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`

	// Actor is a user ID now rather than a team ID. The wire name stays "actor"
	// so the console's audit table needs no change for the switch.
	Actor     string `json:"actor"`
	ActorAddr string `json:"actor_addr"`
	Action    string `json:"action"`

	// Organization and Superadmin matter only to a superadmin reading across
	// tenants: everyone else sees their own organisation's non-superadmin
	// entries and nothing else, so both fields would be constant.
	Organization string `json:"organization_id,omitempty"`
	Superadmin   bool   `json:"superadmin"`

	TargetTeam string         `json:"target_team,omitempty"`
	Before     map[string]any `json:"before"`
	After      map[string]any `json:"after"`
}

type auditPageView struct {
	Entries    []auditEntryView `json:"entries"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func toAuditView(e logstore.AuditEntry) auditEntryView {
	return auditEntryView{
		ID:           e.ID,
		Timestamp:    e.Timestamp.UTC().Format(time.RFC3339Nano),
		Actor:        e.ActorUserID,
		ActorAddr:    e.ActorAddr,
		Action:       e.Action,
		Organization: e.OrganizationID,
		Superadmin:   e.Superadmin,
		TargetTeam:   e.TargetTeamID,
		Before:       e.Before,
		After:        e.After,
	}
}

// listAudit serves GET /admin/audit, scoped to the caller's own organisation.
//
// Step 2.5 opened this to org admins: an audit log exists so the people whose
// resources changed can see what happened to them, which is no use if only an
// operator can read it. The scope comes from the session's org claim, and
// superadmin actions stay superadmin-only.
func listAudit(rec AuditRecorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rec == nil {
			writeError(w, log, http.StatusServiceUnavailable, "audit_log_disabled",
				"the audit log is not configured; set POSTGRES_PASSWORD to enable it")
			return
		}

		c, ok := caller(r)
		if !ok {
			writeError(w, log, http.StatusUnauthorized, "no_session",
				"this endpoint requires a signed-in session")
			return
		}

		limit := 0
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeError(w, log, http.StatusBadRequest, "invalid_request_error",
					"limit must be a positive integer")
				return
			}
			limit = n
		}

		page, err := rec.ListAudit(r.Context(), logstore.AuditQuery{
			OrgID:    c.OrgID,
			Unscoped: c.IsSuperadmin,
			Limit:    limit,
			Cursor:   r.URL.Query().Get("cursor"),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "reading audit log", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the audit log")
			return
		}

		views := make([]auditEntryView, 0, len(page.Entries))
		for _, e := range page.Entries {
			views = append(views, toAuditView(e))
		}
		writeJSON(w, log, http.StatusOK, auditPageView{Entries: views, NextCursor: page.NextCursor})
	}
}

// recordAudit writes one entry, or does nothing when no recorder is wired.
//
// The mutation handlers call this BEFORE applying their change and abort with a
// 503 on error: a privileged mutation must not land without a record of it. A
// nil recorder is the exception — Postgres is simply not configured, the
// pre-Part-2 world, and the slog line every handler still emits is the trail.
func recordAudit(ctx context.Context, rec AuditRecorder, e logstore.AuditEntry) error {
	if rec == nil {
		return nil
	}
	return rec.RecordAudit(ctx, e)
}
