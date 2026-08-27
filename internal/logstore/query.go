// Read side of the request log: filtered, cursor-paginated queries.
package logstore

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPageSize is what a caller gets without asking; MaxPageSize bounds
	// what it may ask for, so one request cannot pull the whole table.
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// Filter narrows a request-log query. A zero value matches everything.
//
// TeamID is set by the handler from the caller's identity, never straight from
// a query parameter — that is what makes team scoping tamper-proof.
type Filter struct {
	TeamID     string
	Provider   string
	Model      string
	StatusCode int
	StatusMin  int // inclusive, for class filters like 4xx
	StatusMax  int // inclusive
	CacheHit   *bool
	Fallback   *bool
	Since      time.Time
	Until      time.Time

	Limit  int
	Cursor Cursor
}

// Cursor is a position in the (ts DESC, id DESC) ordering. Keyset pagination,
// not OFFSET: at 30 days of retention OFFSET has to walk every skipped row,
// and rows arriving mid-pagination shift the window under the reader.
type Cursor struct {
	TS time.Time
	ID string
}

func (c Cursor) IsZero() bool { return c.ID == "" && c.TS.IsZero() }

// Encode renders a cursor as an opaque token. It is opaque by intent: the
// client must not construct one, so the format stays free to change.
func (c Cursor) Encode() string {
	if c.IsZero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(c.TS.UTC().UnixNano(), 10) + "|" + c.ID))
}

func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor is not valid base64: %w", err)
	}
	nanos, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return Cursor{}, fmt.Errorf("cursor is malformed")
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor timestamp is malformed: %w", err)
	}
	return Cursor{TS: time.Unix(0, n).UTC(), ID: id}, nil
}

// Page is one result window plus the cursor that continues it.
type Page struct {
	Records    []Record
	NextCursor string
}

const selectColumns = `
	id, ts, team_id, requested_model, served_model, provider, status_code,
	input_tokens, output_tokens, cost_micros, latency_ms, overhead_ms,
	fallback, cache_hit, quality_score, trace_id`

// Query returns one page of rows, newest first.
func (w *Writer) Query(ctx context.Context, f Filter) (Page, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	var where []string
	var args []any
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if f.TeamID != "" {
		add("team_id = $%d", f.TeamID)
	}
	if f.Provider != "" {
		add("provider = $%d", f.Provider)
	}
	if f.Model != "" {
		// Matches whichever model the caller cares about: a request that fell
		// back is findable by what was asked for and by what actually served.
		args = append(args, f.Model)
		where = append(where, fmt.Sprintf("(requested_model = $%d OR served_model = $%d)", len(args), len(args)))
	}
	if f.StatusCode != 0 {
		add("status_code = $%d", f.StatusCode)
	}
	if f.StatusMin != 0 {
		add("status_code >= $%d", f.StatusMin)
	}
	if f.StatusMax != 0 {
		add("status_code <= $%d", f.StatusMax)
	}
	if f.CacheHit != nil {
		add("cache_hit = $%d", *f.CacheHit)
	}
	if f.Fallback != nil {
		add("fallback = $%d", *f.Fallback)
	}
	if !f.Since.IsZero() {
		add("ts >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("ts < $%d", f.Until)
	}
	if !f.Cursor.IsZero() {
		// Row-value comparison, which matches the (ts DESC, id DESC) index
		// directly rather than making Postgres re-derive the same predicate
		// from an OR chain.
		args = append(args, f.Cursor.TS, f.Cursor.ID)
		where = append(where, fmt.Sprintf("(ts, id) < ($%d, $%d)", len(args)-1, len(args)))
	}

	sql := "SELECT" + selectColumns + " FROM requests"
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	// One extra row tells us whether another page exists without a count(*).
	args = append(args, limit+1)
	sql += fmt.Sprintf(" ORDER BY ts DESC, id DESC LIMIT $%d", len(args))

	rows, err := w.pool.Query(ctx, sql, args...)
	if err != nil {
		return Page{}, fmt.Errorf("querying request log: %w", err)
	}
	defer rows.Close()

	records := make([]Record, 0, limit)
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return Page{}, err
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("reading request log rows: %w", err)
	}

	var next string
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		next = Cursor{TS: last.Timestamp, ID: last.ID}.Encode()
	}
	return Page{Records: records, NextCursor: next}, nil
}

// ErrNotFound is returned by Get when no row has that id.
var ErrNotFound = fmt.Errorf("request not found")

// Get returns one row by id. teamID, when non-empty, scopes the lookup so a
// non-admin caller cannot read another team's row by guessing its id.
func (w *Writer) Get(ctx context.Context, id, teamID string) (Record, error) {
	sql := "SELECT" + selectColumns + " FROM requests WHERE id = $1"
	args := []any{id}
	if teamID != "" {
		sql += " AND team_id = $2"
		args = append(args, teamID)
	}

	rows, err := w.pool.Query(ctx, sql, args...)
	if err != nil {
		return Record{}, fmt.Errorf("querying request %q: %w", id, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Record{}, fmt.Errorf("reading request %q: %w", id, err)
		}
		return Record{}, ErrNotFound
	}
	return scanRecord(rows)
}

// scanner is the slice of pgx.Rows scanRecord needs.
type scanner interface{ Scan(dest ...any) error }

func scanRecord(s scanner) (Record, error) {
	var rec Record
	var requested, served, provider, traceID *string
	if err := s.Scan(
		&rec.ID, &rec.Timestamp, &rec.TeamID, &requested, &served, &provider,
		&rec.StatusCode, &rec.InputTokens, &rec.OutputTokens, &rec.CostMicros,
		&rec.LatencyMS, &rec.OverheadMS, &rec.Fallback, &rec.CacheHit,
		&rec.QualityScore, &traceID,
	); err != nil {
		return Record{}, fmt.Errorf("scanning request log row: %w", err)
	}
	rec.RequestedModel = deref(requested)
	rec.ServedModel = deref(served)
	rec.Provider = deref(provider)
	rec.TraceID = deref(traceID)
	return rec, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
