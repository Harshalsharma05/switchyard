// Request-log query endpoints (Part 2, Step 1.3).
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
)

// RequestLogReader is everything the admin API reads from the request log: the
// paginated query and single-row lookup behind /admin/requests, the per-team
// cost total /admin/reconciliation weighs against Redis, and the bucketed cost
// series behind /admin/costs.
type RequestLogReader interface {
	Query(ctx context.Context, f logstore.Filter) (logstore.Page, error)
	Get(ctx context.Context, id, teamID string) (logstore.Record, error)
	SpendByTeamSince(ctx context.Context, since time.Time) (map[string]int64, error)
	CostSeries(ctx context.Context, q logstore.CostQuery) ([]logstore.CostCell, error)
	FallbackCostSince(ctx context.Context, since time.Time, teamID string) (logstore.FallbackAttribution, error)
}

// KeyAuthenticator resolves a bearer token to a team. The admin listener has no
// auth of its own — it is an operator port — so only the request-log routes take
// a key, which is what lets them scope rows to the calling team.
type KeyAuthenticator interface {
	Authenticate(key string) (*auth.Team, error)
}

// --- wire shape ---------------------------------------------------------

type requestView struct {
	ID             string   `json:"id"`
	Timestamp      string   `json:"timestamp"`
	TeamID         string   `json:"team_id"`
	RequestedModel string   `json:"requested_model,omitempty"`
	ServedModel    string   `json:"served_model,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	StatusCode     int      `json:"status_code"`
	InputTokens    int      `json:"input_tokens"`
	OutputTokens   int      `json:"output_tokens"`
	CostMicros     int64    `json:"cost_micros"`
	CostUSD        float64  `json:"cost_usd"`
	LatencyMS      float64  `json:"latency_ms"`
	OverheadMS     float64  `json:"overhead_ms"`
	Fallback       bool     `json:"fallback"`
	CacheHit       *bool    `json:"cache_hit"`
	QualityScore   *float64 `json:"quality_score"`
	TraceID        string   `json:"trace_id,omitempty"`
}

type requestsPageView struct {
	Requests   []requestView `json:"requests"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func toRequestView(r logstore.Record) requestView {
	return requestView{
		ID:             r.ID,
		Timestamp:      r.Timestamp.UTC().Format(time.RFC3339Nano),
		TeamID:         r.TeamID,
		RequestedModel: r.RequestedModel,
		ServedModel:    r.ServedModel,
		Provider:       r.Provider,
		StatusCode:     r.StatusCode,
		InputTokens:    r.InputTokens,
		OutputTokens:   r.OutputTokens,
		CostMicros:     r.CostMicros,
		CostUSD:        float64(r.CostMicros) / microsPerUSD,
		LatencyMS:      r.LatencyMS,
		OverheadMS:     r.OverheadMS,
		Fallback:       r.Fallback,
		CacheHit:       r.CacheHit,
		QualityScore:   r.QualityScore,
		TraceID:        r.TraceID,
	}
}

// --- handlers -----------------------------------------------------------

func listRequests(reader RequestLogReader, authr KeyAuthenticator, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			writeRequestLogDisabled(w, log)
			return
		}

		team, ok := authenticate(w, r, authr, log)
		if !ok {
			return
		}

		filter, err := parseFilter(r, team)
		if err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		page, err := reader.Query(r.Context(), filter)
		if err != nil {
			log.ErrorContext(r.Context(), "querying request log", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the request log")
			return
		}

		views := make([]requestView, 0, len(page.Records))
		for _, rec := range page.Records {
			views = append(views, toRequestView(rec))
		}
		writeJSON(w, log, http.StatusOK, requestsPageView{Requests: views, NextCursor: page.NextCursor})
	}
}

func getRequest(reader RequestLogReader, authr KeyAuthenticator, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			writeRequestLogDisabled(w, log)
			return
		}

		team, ok := authenticate(w, r, authr, log)
		if !ok {
			return
		}

		// An admin passes an empty scope and can read any row; everyone else is
		// pinned to their own team, so a guessed id answers 404 rather than
		// handing back another team's data.
		scope := team.ID
		if team.IsAdmin {
			scope = ""
		}

		rec, err := reader.Get(r.Context(), chi.URLParam(r, "id"), scope)
		if err != nil {
			if errors.Is(err, logstore.ErrNotFound) {
				writeError(w, log, http.StatusNotFound, "not_found", "no such request")
				return
			}
			log.ErrorContext(r.Context(), "reading request log row", slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not read the request log")
			return
		}
		writeJSON(w, log, http.StatusOK, toRequestView(rec))
	}
}

func writeRequestLogDisabled(w http.ResponseWriter, log *slog.Logger) {
	writeError(w, log, http.StatusServiceUnavailable, "request_log_disabled",
		"the request log is not configured; set POSTGRES_PASSWORD to enable it")
}

// authenticate resolves the bearer token, writing the 401 itself on failure.
func authenticate(w http.ResponseWriter, r *http.Request, authr KeyAuthenticator, log *slog.Logger) (*auth.Team, bool) {
	const prefix = "Bearer "

	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		writeError(w, log, http.StatusUnauthorized, "invalid_api_key",
			"missing or malformed Authorization header; expected a Bearer key")
		return nil, false
	}

	team, err := authr.Authenticate(strings.TrimSpace(strings.TrimPrefix(h, prefix)))
	if err != nil {
		if errors.Is(err, auth.ErrUnknownKey) {
			writeError(w, log, http.StatusUnauthorized, "invalid_api_key",
				"the provided API key was not recognized")
			return nil, false
		}
		log.ErrorContext(r.Context(), "authenticating admin request", slog.Any("error", err))
		writeError(w, log, http.StatusInternalServerError, "internal_error",
			"the gateway could not authenticate this request")
		return nil, false
	}
	return team, true
}

// parseFilter builds the query filter from the URL. A non-admin caller's
// TeamID always comes from its key, never from the query string, so no
// parameter a client can set widens what it sees.
func parseFilter(r *http.Request, team *auth.Team) (logstore.Filter, error) {
	q := r.URL.Query()
	f := logstore.Filter{
		Provider: q.Get("provider"),
		Model:    q.Get("model"),
	}

	if team.IsAdmin {
		f.TeamID = q.Get("team")
	} else {
		if want := q.Get("team"); want != "" && want != team.ID {
			return f, fmt.Errorf("team %q may only read its own requests", team.ID)
		}
		f.TeamID = team.ID
	}

	if v := q.Get("status"); v != "" {
		// A class like 4xx reads as a range; a bare number is an exact match.
		if len(v) == 3 && (v[1] == 'x' || v[1] == 'X') {
			d, err := strconv.Atoi(v[:1])
			if err != nil || d < 1 || d > 5 {
				return f, errors.New("status class must be one of 1xx through 5xx")
			}
			f.StatusMin, f.StatusMax = d*100, d*100+99
		} else {
			code, err := strconv.Atoi(v)
			if err != nil {
				return f, errors.New("status must be an HTTP status code or a class like 4xx")
			}
			f.StatusCode = code
		}
	}

	switch v := q.Get("cache"); v {
	case "":
	case "hit":
		f.CacheHit = boolPtr(true)
	case "miss":
		f.CacheHit = boolPtr(false)
	default:
		return f, errors.New("cache must be hit or miss")
	}

	if v := q.Get("fallback"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return f, errors.New("fallback must be true or false")
		}
		f.Fallback = &b
	}

	var err error
	if f.Since, err = parseTime(q.Get("since")); err != nil {
		return f, errors.New("since must be an RFC3339 timestamp or a duration like 24h")
	}
	if f.Until, err = parseTime(q.Get("until")); err != nil {
		return f, errors.New("until must be an RFC3339 timestamp or a duration like 24h")
	}

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return f, errors.New("limit must be a positive integer")
		}
		f.Limit = n
	}

	if f.Cursor, err = logstore.DecodeCursor(q.Get("cursor")); err != nil {
		return f, err
	}
	return f, nil
}

// parseTime accepts an absolute RFC3339 timestamp or a relative duration like
// 24h, which is what the Phase 2 time-range selector actually sends.
func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, err
	}
	return time.Now().UTC().Add(-d), nil
}

func boolPtr(b bool) *bool { return &b }
