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
	fallback, cache_hit, quality_score, trace_id, fallback_cost_delta_micros,
	routing_tier, routing_reason, routing_savings_micros, quality_sample_reason`

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

// SpendByTeamSince totals logged request cost per team from `since` to now. It
// spans both the live detail rows and any the retention sweep has already
// folded into requests_daily, so a month that straddles the retention window
// is still summed in full — this is the number GET /admin/reconciliation
// checks Redis's budget counter against.
func (w *Writer) SpendByTeamSince(ctx context.Context, since time.Time) (map[string]int64, error) {
	const sql = `
		SELECT team_id, SUM(cost_micros)::bigint FROM (
			SELECT team_id, cost_micros FROM requests       WHERE ts  >= $1
			UNION ALL
			SELECT team_id, cost_micros FROM requests_daily  WHERE day >= $1::date
		) s
		GROUP BY team_id`

	rows, err := w.pool.Query(ctx, sql, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("summing request-log spend by team: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var team string
		var micros int64
		if err := rows.Scan(&team, &micros); err != nil {
			return nil, fmt.Errorf("scanning request-log spend row: %w", err)
		}
		out[team] = micros
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading request-log spend rows: %w", err)
	}
	return out, nil
}

// CostBucket is the time resolution of a cost series.
type CostBucket string

const (
	CostHourly CostBucket = "hour"
	CostDaily  CostBucket = "day"
)

// CostDimension is the column a cost series is split by.
type CostDimension string

const (
	CostByProvider CostDimension = "provider"
	CostByModel    CostDimension = "model"
	CostByTeam     CostDimension = "team"
)

// columns maps a dimension onto its column in each source table. served_model,
// not requested_model: cost is attributed to the model that actually ran.
func (d CostDimension) columns() (live, rolled string, ok bool) {
	switch d {
	case CostByProvider:
		return "COALESCE(provider, '')", "provider", true
	case CostByModel:
		return "COALESCE(served_model, '')", "served_model", true
	case CostByTeam:
		return "team_id", "team_id", true
	}
	return "", "", false
}

// CostQuery aggregates request-log cost into time buckets split by one
// dimension. TeamID == "" spans every team (an admin caller).
type CostQuery struct {
	Since     time.Time
	Bucket    CostBucket
	Dimension CostDimension
	TeamID    string
}

// CostCell is one (bucket, dimension-value) total in micro-dollars.
type CostCell struct {
	Bucket time.Time
	Key    string
	Micros int64
}

// CostSeries returns per-bucket, per-dimension cost totals ordered oldest
// first. Daily buckets union the live rows with anything retention has already
// rolled into requests_daily; hourly buckets read only the live rows, since
// the rollup has no sub-day resolution and an hourly range is always well
// inside the retention window anyway.
func (w *Writer) CostSeries(ctx context.Context, q CostQuery) ([]CostCell, error) {
	liveCol, rolledCol, ok := q.Dimension.columns()
	if !ok {
		return nil, fmt.Errorf("cost series: unknown dimension %q", q.Dimension)
	}
	trunc := "hour"
	if q.Bucket == CostDaily {
		trunc = "day"
	}

	args := []any{q.Since.UTC()}
	teamClause := ""
	if q.TeamID != "" {
		args = append(args, q.TeamID)
		teamClause = " AND team_id = $2"
	}

	live := fmt.Sprintf(`
		SELECT date_trunc('%s', ts AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS bucket,
		       %s AS k, cost_micros AS micros
		FROM requests WHERE ts >= $1%s`, trunc, liveCol, teamClause)

	src := live
	if q.Bucket == CostDaily {
		src += fmt.Sprintf(` UNION ALL
		SELECT (day + time '00:00') AT TIME ZONE 'UTC' AS bucket,
		       %s AS k, cost_micros AS micros
		FROM requests_daily WHERE day >= $1::date%s`, rolledCol, teamClause)
	}

	sql := fmt.Sprintf(
		`SELECT bucket, k, SUM(micros)::bigint FROM (%s) s GROUP BY bucket, k ORDER BY bucket`, src)

	rows, err := w.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("querying cost series: %w", err)
	}
	defer rows.Close()

	var cells []CostCell
	for rows.Next() {
		var c CostCell
		if err := rows.Scan(&c.Bucket, &c.Key, &c.Micros); err != nil {
			return nil, fmt.Errorf("scanning cost cell: %w", err)
		}
		cells = append(cells, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading cost cells: %w", err)
	}
	return cells, nil
}

// FallbackAttribution splits the logged fallback cost deltas by sign: what
// fallbacks added versus what they saved, over some window.
type FallbackAttribution struct {
	ExtraMicros int64 // sum of positive deltas — fallback served something pricier
	SavedMicros int64 // sum of the negative deltas' magnitudes — fallback served something cheaper
}

// NetMicros is the overall effect: positive means fallback cost more than the
// requested models would have.
func (a FallbackAttribution) NetMicros() int64 { return a.ExtraMicros - a.SavedMicros }

// FallbackCostSince totals the per-request fallback cost deltas from `since` to
// now, split by sign. It reads only the live detail rows — every supported
// range sits within the retention window, so the daily rollup (which stores
// only a net sum per group and cannot be resplit by sign) is not consulted.
func (w *Writer) FallbackCostSince(ctx context.Context, since time.Time, teamID string) (FallbackAttribution, error) {
	args := []any{since.UTC()}
	where := "ts >= $1 AND fallback_cost_delta_micros IS NOT NULL"
	if teamID != "" {
		args = append(args, teamID)
		where += " AND team_id = $2"
	}

	sql := `
		SELECT
			COALESCE(SUM(fallback_cost_delta_micros) FILTER (WHERE fallback_cost_delta_micros > 0), 0),
			COALESCE(-SUM(fallback_cost_delta_micros) FILTER (WHERE fallback_cost_delta_micros < 0), 0)
		FROM requests WHERE ` + where

	var a FallbackAttribution
	if err := w.pool.QueryRow(ctx, sql, args...).Scan(&a.ExtraMicros, &a.SavedMicros); err != nil {
		return FallbackAttribution{}, fmt.Errorf("summing fallback cost deltas: %w", err)
	}
	return a, nil
}

// RoutingSavings is what cost-aware routing avoided spending over a range.
//
// Downgraded counts requests routed below the top tier — the ones that could
// save anything. Routed is the denominator: savings without it says nothing
// about how much traffic the classifier actually handled.
type RoutingSavings struct {
	SavedMicros int64
	Routed      int64
	Downgraded  int64
}

// RoutingSavingsSince totals the per-request routing savings from `since` to
// now. It reads only live detail rows, like FallbackCostSince: every supported
// range sits inside the retention window, so the daily rollup is not consulted.
//
// Rows where routing_savings_micros IS NULL are excluded rather than counted
// as zero — those are requests whose caller named a model, and routing cannot
// take credit for traffic it never saw.
func (w *Writer) RoutingSavingsSince(ctx context.Context, since time.Time, teamID string) (RoutingSavings, error) {
	args := []any{since.UTC()}
	where := "ts >= $1 AND routing_savings_micros IS NOT NULL"
	if teamID != "" {
		args = append(args, teamID)
		where += " AND team_id = $2"
	}

	sql := `
		SELECT
			COALESCE(SUM(routing_savings_micros), 0),
			COUNT(*),
			COUNT(*) FILTER (WHERE routing_savings_micros > 0)
		FROM requests WHERE ` + where

	var rs RoutingSavings
	if err := w.pool.QueryRow(ctx, sql, args...).Scan(&rs.SavedMicros, &rs.Routed, &rs.Downgraded); err != nil {
		return RoutingSavings{}, fmt.Errorf("summing routing savings: %w", err)
	}
	return rs, nil
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
	var requested, served, provider, traceID, routingTier, routingReason, qualityReason *string
	if err := s.Scan(
		&rec.ID, &rec.Timestamp, &rec.TeamID, &requested, &served, &provider,
		&rec.StatusCode, &rec.InputTokens, &rec.OutputTokens, &rec.CostMicros,
		&rec.LatencyMS, &rec.OverheadMS, &rec.Fallback, &rec.CacheHit,
		&rec.QualityScore, &traceID, &rec.FallbackCostDeltaMicros,
		&routingTier, &routingReason, &rec.RoutingSavingsMicros, &qualityReason,
	); err != nil {
		return Record{}, fmt.Errorf("scanning request log row: %w", err)
	}
	rec.RequestedModel = deref(requested)
	rec.ServedModel = deref(served)
	rec.Provider = deref(provider)
	rec.TraceID = deref(traceID)
	rec.RoutingTier = deref(routingTier)
	rec.RoutingReason = deref(routingReason)
	rec.QualitySampleReason = deref(qualityReason)
	return rec, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CacheSavings is what the semantic cache avoided spending over a range.
//
// Grouped by served model because a saving is only meaningful at that model's
// price: the caller multiplies each group's real token counts by the same
// pricing table the live path uses, so the figure is computed rather than
// estimated. Hits and Misses come back alongside so the caller can report a
// hit rate from the same query.
type CacheSavings struct {
	Groups []CacheSavingsGroup
	Hits   int64
	Misses int64
}

// CacheSavingsGroup is one served model's cache-hit token totals.
type CacheSavingsGroup struct {
	Model        string
	Hits         int64
	InputTokens  int64
	OutputTokens int64
}

// CacheSavingsSince totals cache-hit tokens by served model from `since` to now.
//
// Only rows where cache_hit is non-null are counted: a null means the cache was
// never consulted, which is neither a hit nor a miss and must not enter the
// denominator.
func (w *Writer) CacheSavingsSince(ctx context.Context, since time.Time, teamID string) (CacheSavings, error) {
	args := []any{since.UTC()}
	where := "ts >= $1 AND cache_hit IS NOT NULL"
	if teamID != "" {
		args = append(args, teamID)
		where += " AND team_id = $2"
	}

	sql := `
		SELECT COALESCE(served_model, ''),
		       COUNT(*) FILTER (WHERE cache_hit),
		       COALESCE(SUM(input_tokens) FILTER (WHERE cache_hit), 0),
		       COALESCE(SUM(output_tokens) FILTER (WHERE cache_hit), 0),
		       COUNT(*) FILTER (WHERE NOT cache_hit)
		FROM requests WHERE ` + where + `
		GROUP BY served_model`

	rows, err := w.pool.Query(ctx, sql, args...)
	if err != nil {
		return CacheSavings{}, fmt.Errorf("summing cache savings: %w", err)
	}
	defer rows.Close()

	var out CacheSavings
	for rows.Next() {
		var g CacheSavingsGroup
		var misses int64
		if err := rows.Scan(&g.Model, &g.Hits, &g.InputTokens, &g.OutputTokens, &misses); err != nil {
			return CacheSavings{}, fmt.Errorf("scanning cache savings: %w", err)
		}
		out.Hits += g.Hits
		out.Misses += misses
		if g.Hits > 0 {
			out.Groups = append(out.Groups, g)
		}
	}
	if err := rows.Err(); err != nil {
		return CacheSavings{}, fmt.Errorf("reading cache savings: %w", err)
	}
	return out, nil
}

// SetQualityScore writes Phase 9's async score onto an already-logged row.
//
// The row is written by the buffered flusher and the score arrives seconds
// later from the judge, so by the time this runs the row is almost always
// present; when it is not (a very fast judge, a dropped row) the UPDATE
// touches nothing and the caller treats that as a benign miss rather than an
// error — an unscored request is exactly what the sampling policy already
// tolerates.
func (w *Writer) SetQualityScore(ctx context.Context, id string, score float64, reason string) (bool, error) {
	tag, err := w.pool.Exec(ctx,
		"UPDATE requests SET quality_score = $1, quality_sample_reason = $2 WHERE id = $3",
		score, nullable(reason), id)
	if err != nil {
		return false, fmt.Errorf("setting quality score for %q: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// QualityReasonStat aggregates the async scores for one sampling reason.
// LowScored counts rows at or below the caller's low-score cutoff — the ones
// Step 9.3's feedback loops are about.
type QualityReasonStat struct {
	Reason    string
	Scored    int64
	AvgScore  float64
	LowScored int64
	MinScore  float64
	MaxScore  float64
}

// QualityExample is one low-scoring downgraded request: a candidate classifier
// mislabel. The prompt is not stored, so the row is identified by ID and trace
// ID for follow-up rather than carrying its own content.
type QualityExample struct {
	ID            string
	Timestamp     time.Time
	ServedModel   string
	RoutingTier   string
	RoutingReason string
	QualityScore  float64
	TraceID       string
}

// QualityFeedback is the whole Step 9.3 picture over a range.
type QualityFeedback struct {
	Reasons  []QualityReasonStat
	Examples []QualityExample
}

// QualityFeedbackSince aggregates async quality scores by sampling reason, and
// pulls a bounded list of the low-scoring downgraded requests. Live detail
// rows only, like the other attribution queries — every supported range sits
// inside the retention window, and the rollup carries no quality at all.
func (w *Writer) QualityFeedbackSince(ctx context.Context, since time.Time, teamID string, lowScore float64, exampleLimit int) (QualityFeedback, error) {
	args := []any{since.UTC(), lowScore}
	where := "ts >= $1 AND quality_score IS NOT NULL"
	if teamID != "" {
		args = append(args, teamID)
		where += " AND team_id = $3"
	}

	statSQL := `
		SELECT COALESCE(quality_sample_reason, 'unspecified'),
		       COUNT(*), AVG(quality_score),
		       COUNT(*) FILTER (WHERE quality_score <= $2),
		       MIN(quality_score), MAX(quality_score)
		FROM requests WHERE ` + where + `
		GROUP BY 1 ORDER BY 1`

	rows, err := w.pool.Query(ctx, statSQL, args...)
	if err != nil {
		return QualityFeedback{}, fmt.Errorf("aggregating quality feedback: %w", err)
	}
	defer rows.Close()

	var fb QualityFeedback
	for rows.Next() {
		var s QualityReasonStat
		if err := rows.Scan(&s.Reason, &s.Scored, &s.AvgScore, &s.LowScored, &s.MinScore, &s.MaxScore); err != nil {
			return QualityFeedback{}, fmt.Errorf("scanning quality feedback row: %w", err)
		}
		fb.Reasons = append(fb.Reasons, s)
	}
	if err := rows.Err(); err != nil {
		return QualityFeedback{}, fmt.Errorf("reading quality feedback rows: %w", err)
	}

	if exampleLimit <= 0 {
		return fb, nil
	}

	exArgs := append(args[:0:0], args...)
	exArgs = append(exArgs, exampleLimit)
	exSQL := `
		SELECT id, ts, COALESCE(served_model, ''), COALESCE(routing_tier, ''),
		       COALESCE(routing_reason, ''), quality_score, COALESCE(trace_id, '')
		FROM requests WHERE ` + where + `
		  AND quality_sample_reason = 'downgraded' AND quality_score <= $2
		ORDER BY ts DESC LIMIT $` + strconv.Itoa(len(exArgs))

	exRows, err := w.pool.Query(ctx, exSQL, exArgs...)
	if err != nil {
		return QualityFeedback{}, fmt.Errorf("listing low-quality downgrades: %w", err)
	}
	defer exRows.Close()

	for exRows.Next() {
		var e QualityExample
		if err := exRows.Scan(&e.ID, &e.Timestamp, &e.ServedModel, &e.RoutingTier, &e.RoutingReason, &e.QualityScore, &e.TraceID); err != nil {
			return QualityFeedback{}, fmt.Errorf("scanning low-quality downgrade: %w", err)
		}
		fb.Examples = append(fb.Examples, e)
	}
	return fb, exRows.Err()
}
