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
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditEntry is one row to record. ID and Timestamp are filled in by RecordAudit
// — the caller supplies only what happened.
type AuditEntry struct {
	ID        string
	Timestamp time.Time

	// ActorUserID is the dashboard user who made the change. Since Step 1.5 the
	// admin port takes a session and nothing else, so there is always a real
	// person here rather than a machine credential — which is the whole point of
	// an audit log.
	ActorUserID string
	ActorAddr   string
	Action      string

	// OrganizationID is the tenant the change belongs to, and what scopes the
	// read. "" is stored as NULL, which no organisation filter matches.
	OrganizationID string

	// Superadmin records that an operator took this action. Those entries are
	// visible to superadmins only.
	Superadmin bool

	TargetTeamID string // "" is stored as NULL
	Before       map[string]any
	After        map[string]any
}

// ActorPlatformOperator stands in for the real actor when a tenant reads an
// entry for an action an operator took on its resources.
//
// The tenant needs to know its key was rotated and that it was not one of its
// own people who did it; it does not need the operator's identity. The sentinel
// cannot collide with a real actor, which is always a "usr_"-prefixed ID.
const ActorPlatformOperator = "platform-operator"

// AuditQuery scopes one page of audit entries.
type AuditQuery struct {
	// OrgID restricts entries to the organisation they were filed against —
	// the tenant whose resources changed, whoever changed them. Ignored when
	// Unscoped is set.
	OrgID string

	// Unscoped reads every organisation's entries and every actor's real
	// identity. Only a superadmin caller may set it.
	Unscoped bool

	Limit  int
	Cursor string
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
INSERT INTO audit_log (id, ts, actor_user_id, actor_addr, action, organization_id,
                       superadmin, target_team_id, before, after)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

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
		id, time.Now().UTC(), e.ActorUserID, e.ActorAddr, e.Action,
		nullable(e.OrganizationID), e.Superadmin, nullable(e.TargetTeamID), before, after,
	); err != nil {
		return fmt.Errorf("writing audit entry: %w", err)
	}
	return nil
}

// ListAudit returns one page of entries, newest first, scoped by q.
//
// A tenant read returns every entry filed against its organisation, including
// actions an operator took on its resources — an audit log exists so the party
// whose resources changed can see that they changed, and one that hid operator
// actions from them would fail at exactly the case it is for.
//
// What a tenant does not get is who the operator was: the actor and its address
// are replaced with ActorPlatformOperator for superadmin rows. That redaction
// happens here, in the query, rather than in the response mapper, so the real
// identity never enters the process on a tenant read at all.
//
// An entry with a NULL organization_id belongs to no tenant — a config reload,
// a bootstrap grant, or a row written before Step 2.5 that cannot be attributed
// — and matches no organisation filter, so it stays superadmin-only.
func (a *AuditLog) ListAudit(ctx context.Context, q AuditQuery) (AuditPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	cur, err := DecodeCursor(q.Cursor)
	if err != nil {
		return AuditPage{}, err
	}

	var where []string
	var args []any

	actorExpr, addrExpr := "actor_user_id", "actor_addr"
	if !q.Unscoped {
		args = append(args, ActorPlatformOperator)
		actorExpr = fmt.Sprintf("CASE WHEN superadmin THEN $%d ELSE actor_user_id END", len(args))
		addrExpr = "CASE WHEN superadmin THEN '' ELSE actor_addr END"
	}

	sql := fmt.Sprintf(`SELECT id, ts, %s, %s, action,
	               COALESCE(organization_id, ''), superadmin,
	               COALESCE(target_team_id, ''), before, after
	        FROM audit_log`, actorExpr, addrExpr)

	if !q.Unscoped {
		args = append(args, q.OrgID)
		where = append(where, fmt.Sprintf("organization_id = $%d", len(args)))
	}
	if !cur.IsZero() {
		args = append(args, cur.TS, cur.ID)
		where = append(where, fmt.Sprintf("(ts, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
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
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.ActorUserID, &e.ActorAddr,
			&e.Action, &e.OrganizationID, &e.Superadmin, &e.TargetTeamID,
			&before, &after); err != nil {
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
