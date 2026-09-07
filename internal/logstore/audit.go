// Operator audit log: synchronous, append-only writes and one paginated read
// (Part 2, Step 6.4 backend).
//
// Separate from Writer on purpose. The request log is high-volume and buffered
// — a dropped row there is acceptable. An audit write is rare, must not be
// dropped, and its caller waits on the result to decide whether to proceed with
// the mutation, so it is a plain synchronous Exec against the shared pool.
package logstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditEntry is one row to record. ID and Timestamp are filled in by RecordAudit
// — the caller supplies only what happened.
type AuditEntry struct {
	ID           string
	Timestamp    time.Time
	ActorTeamID  string
	ActorAddr    string
	Action       string
	TargetTeamID string // "" is stored as NULL
	Before       map[string]any
	After        map[string]any
}

// AuditPage is one result window plus the cursor that continues it. It reuses
// Cursor from the request-log query layer: the ordering is identical,
// (ts DESC, id DESC), so the keyset pagination is too.
type AuditPage struct {
	Entries    []AuditEntry
	NextCursor string
}

// AuditLog is the read/write handle. It shares the request log's pool.
type AuditLog struct {
	pool *pgxpool.Pool
}

func NewAuditLog(pool *pgxpool.Pool) *AuditLog {
	return &AuditLog{pool: pool}
}

const auditInsertSQL = `
INSERT INTO audit_log (id, ts, actor_team_id, actor_addr, action, target_team_id, before, after)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// RecordAudit writes one entry synchronously and returns the write's outcome.
// The mutation handlers treat any error here as a reason to abort — the row
// must land before the change does.
func (a *AuditLog) RecordAudit(ctx context.Context, e AuditEntry) error {
	id, err := randomHexID()
	if err != nil {
		return err
	}
	before, err := marshalDelta(e.Before)
	if err != nil {
		return fmt.Errorf("marshaling audit before: %w", err)
	}
	after, err := marshalDelta(e.After)
	if err != nil {
		return fmt.Errorf("marshaling audit after: %w", err)
	}

	if _, err := a.pool.Exec(ctx, auditInsertSQL,
		id, time.Now().UTC(), e.ActorTeamID, e.ActorAddr, e.Action,
		nullable(e.TargetTeamID), before, after,
	); err != nil {
		return fmt.Errorf("writing audit entry: %w", err)
	}
	return nil
}

// ListAudit returns one page of entries, newest first.
func (a *AuditLog) ListAudit(ctx context.Context, limit int, cursor string) (AuditPage, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	cur, err := DecodeCursor(cursor)
	if err != nil {
		return AuditPage{}, err
	}

	sql := `SELECT id, ts, actor_team_id, actor_addr, action,
	               COALESCE(target_team_id, ''), before, after
	        FROM audit_log`
	var args []any
	if !cur.IsZero() {
		args = append(args, cur.TS, cur.ID)
		sql += " WHERE (ts, id) < ($1, $2)"
	}
	args = append(args, limit+1)
	sql += fmt.Sprintf(" ORDER BY ts DESC, id DESC LIMIT $%d", len(args))

	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return AuditPage{}, fmt.Errorf("querying audit log: %w", err)
	}
	defer rows.Close()

	entries := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var e AuditEntry
		var before, after []byte
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.ActorTeamID, &e.ActorAddr,
			&e.Action, &e.TargetTeamID, &before, &after); err != nil {
			return AuditPage{}, fmt.Errorf("scanning audit row: %w", err)
		}
		e.Before = unmarshalDelta(before)
		e.After = unmarshalDelta(after)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return AuditPage{}, fmt.Errorf("reading audit rows: %w", err)
	}

	var next string
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[len(entries)-1]
		next = Cursor{TS: last.Timestamp, ID: last.ID}.Encode()
	}
	return AuditPage{Entries: entries, NextCursor: next}, nil
}

func randomHexID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating audit id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func marshalDelta(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalDelta never fails the read over a malformed stored value — an audit
// row is still worth showing without its delta.
func unmarshalDelta(b []byte) map[string]any {
	if len(b) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}
