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
	ListAudit(ctx context.Context, limit int, cursor string) (logstore.AuditPage, error)
}

type auditEntryView struct {
	ID         string         `json:"id"`
	Timestamp  string         `json:"timestamp"`
	Actor      string         `json:"actor"`
	ActorAddr  string         `json:"actor_addr"`
	Action     string         `json:"action"`
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
		ID:         e.ID,
		Timestamp:  e.Timestamp.UTC().Format(time.RFC3339Nano),
		Actor:      e.ActorTeamID,
		ActorAddr:  e.ActorAddr,
		Action:     e.Action,
		TargetTeam: e.TargetTeamID,
		Before:     e.Before,
		After:      e.After,
	}
}

// listAudit serves GET /admin/audit. Admin-gated by the route group.
func listAudit(rec AuditRecorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rec == nil {
			writeError(w, log, http.StatusServiceUnavailable, "audit_log_disabled",
				"the audit log is not configured; set POSTGRES_PASSWORD to enable it")
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

		page, err := rec.ListAudit(r.Context(), limit, r.URL.Query().Get("cursor"))
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
