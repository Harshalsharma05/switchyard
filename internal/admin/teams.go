package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/auth"
)

// microsPerUSD mirrors internal/config's own constant of the same name.
// Duplicated rather than imported: admin's JSON DTOs convert micro-dollars to
// a human-readable number at the API boundary, which is a presentation
// concern belonging to this package, not to config's YAML-loading one.
const microsPerUSD = 1_000_000

// TeamStore is the slice of auth.Registry this package needs.
//
// Declared here, by the consumer, for the same reason every other package's
// dependencies are: admin depends on listing, reading, and patching teams,
// not on auth.Registry's own locking strategy.
type TeamStore interface {
	List() []auth.Team
	Get(id string) (auth.Team, error)
	Update(id string, patch auth.TeamPatch) (auth.Team, error)
}

// SpendReader is the slice of budget.Tracker this package needs.
type SpendReader interface {
	Spent(ctx context.Context, teamID string) (int64, error)
	Reset(ctx context.Context, teamID string) error
}

// --- wire shapes -------------------------------------------------------

type rateLimitsView struct {
	RPM int `json:"rpm"`
	TPM int `json:"tpm"`
}

// teamView is every team-returning endpoint's response shape.
//
// SpentUSD and BudgetUtilization are pointers, not plain float64s: nil means
// "not read for this response" — either a Redis failure while reading spend,
// or a caller that deliberately didn't ask — rather than a genuine zero. A
// team at 95% of its cap reported as "$0.00 spent" would be a worse answer
// than admitting the number isn't known here; nil serializes as JSON null,
// which is distinguishable from a real 0 in a way an omitted field would not
// be.
type teamView struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	AllowedProviders  []string       `json:"allowed_providers"`
	AllowedModels     []string       `json:"allowed_models"`
	RateLimits        rateLimitsView `json:"rate_limits"`
	MonthlyBudgetUSD  float64        `json:"monthly_budget_usd"`
	SpentUSD          *float64       `json:"spent_usd"`
	BudgetUtilization *float64       `json:"budget_utilization"`
	Priority          string         `json:"priority"`
}

// teamPatchRequest is PATCH /admin/teams/{id}'s body. Every field is
// optional — a nil field in the decoded struct means "leave this alone,"
// which is what a partial PATCH means and is also exactly auth.TeamPatch's
// own convention, so decoding this and building a TeamPatch is a direct
// field-by-field copy with one unit conversion.
type teamPatchRequest struct {
	RPM              *int     `json:"rpm"`
	TPM              *int     `json:"tpm"`
	MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
}

func usdToMicros(usd float64) int64 {
	return int64(math.Round(usd * microsPerUSD))
}

func microsToUSD(micros int64) float64 {
	return float64(micros) / microsPerUSD
}

// newTeamView builds the response shape for one team. spentMicros is nil
// when spend was not read (a Redis failure, logged by the caller) — see the
// teamView doc comment for why that stays null rather than becoming 0.
func newTeamView(t auth.Team, spentMicros *int64) teamView {
	v := teamView{
		ID:               t.ID,
		Name:             t.Name,
		AllowedProviders: t.AllowedProviders,
		AllowedModels:    t.AllowedModels,
		RateLimits:       rateLimitsView{RPM: t.RateLimits.RPM, TPM: t.RateLimits.TPM},
		MonthlyBudgetUSD: microsToUSD(t.MonthlyBudgetMicros),
		Priority:         string(t.Priority),
	}
	if spentMicros != nil {
		spentUSD := microsToUSD(*spentMicros)
		v.SpentUSD = &spentUSD
		if t.MonthlyBudgetMicros > 0 {
			util := float64(*spentMicros) / float64(t.MonthlyBudgetMicros)
			v.BudgetUtilization = &util
		}
	}
	return v
}

// readSpent reads a team's spend for a response, logging and returning nil
// on failure rather than propagating the error — every call site here is
// building a response that has other useful information in it regardless of
// whether spend could be read, so a Redis hiccup on this one field must not
// take down the rest of the response.
func readSpent(ctx context.Context, spend SpendReader, log *slog.Logger, teamID string) *int64 {
	v, err := spend.Spent(ctx, teamID)
	if err != nil {
		log.ErrorContext(ctx, "reading team spend", slog.String("team", teamID), slog.Any("error", err))
		return nil
	}
	return &v
}

// --- handlers ------------------------------------------------------------

// listTeams serves GET /admin/teams.
func listTeams(store TeamStore, spend SpendReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teams := store.List()
		views := make([]teamView, 0, len(teams))
		for _, t := range teams {
			views = append(views, newTeamView(t, readSpent(r.Context(), spend, log, t.ID)))
		}
		writeJSON(w, log, http.StatusOK, views)
	}
}

// getTeam serves GET /admin/teams/{id}.
func getTeam(store TeamStore, spend SpendReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		team, err := store.Get(id)
		if err != nil {
			writeTeamLookupError(w, log, id, err)
			return
		}

		writeJSON(w, log, http.StatusOK, newTeamView(team, readSpent(r.Context(), spend, log, id)))
	}
}

// patchTeam serves PATCH /admin/teams/{id}.
//
// Every mutation is logged with an actor, before values, and after values,
// per Step 4.3's checklist. "Actor" is the caller's remote address: Part 1
// has no admin authentication (CLAUDE.md scopes auth to static API keys on
// the public API only), so a real operator identity does not exist to log —
// the address is the most honest thing available, not a stand-in for real
// audit identity.
func patchTeam(store TeamStore, spend SpendReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		before, err := store.Get(id)
		if err != nil {
			writeTeamLookupError(w, log, id, err)
			return
		}

		var body teamPatchRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"request body is not valid JSON: "+err.Error())
			return
		}

		patch := auth.TeamPatch{RPM: body.RPM, TPM: body.TPM}
		if body.MonthlyBudgetUSD != nil {
			micros := usdToMicros(*body.MonthlyBudgetUSD)
			patch.MonthlyBudgetMicros = &micros
		}

		after, err := store.Update(id, patch)
		if err != nil {
			if errors.Is(err, auth.ErrUnknownTeam) {
				writeError(w, log, http.StatusNotFound, "team_not_found", "no such team "+id)
				return
			}
			writeError(w, log, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin patched team",
			slog.String("actor", r.RemoteAddr),
			slog.String("team", id),
			slog.Group("before",
				slog.Int("rpm", before.RateLimits.RPM),
				slog.Int("tpm", before.RateLimits.TPM),
				slog.Float64("monthly_budget_usd", microsToUSD(before.MonthlyBudgetMicros)),
			),
			slog.Group("after",
				slog.Int("rpm", after.RateLimits.RPM),
				slog.Int("tpm", after.RateLimits.TPM),
				slog.Float64("monthly_budget_usd", microsToUSD(after.MonthlyBudgetMicros)),
			),
		)

		writeJSON(w, log, http.StatusOK, newTeamView(after, readSpent(r.Context(), spend, log, id)))
	}
}

// resetBudget serves POST /admin/teams/{id}/reset-budget.
func resetBudget(store TeamStore, spend SpendReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		team, err := store.Get(id)
		if err != nil {
			writeTeamLookupError(w, log, id, err)
			return
		}

		before := readSpent(r.Context(), spend, log, id)

		if err := spend.Reset(r.Context(), id); err != nil {
			log.ErrorContext(r.Context(), "resetting team budget",
				slog.String("team", id), slog.Any("error", err))
			writeError(w, log, http.StatusServiceUnavailable, "budget_reset_failed",
				"the gateway could not reset this team's budget; try again shortly")
			return
		}

		beforeUSD := 0.0
		if before != nil {
			beforeUSD = microsToUSD(*before)
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, "admin reset team budget",
			slog.String("actor", r.RemoteAddr),
			slog.String("team", id),
			slog.Float64("before_spent_usd", beforeUSD),
			slog.Float64("after_spent_usd", 0),
		)

		zero := int64(0)
		writeJSON(w, log, http.StatusOK, newTeamView(team, &zero))
	}
}

// writeTeamLookupError maps a TeamStore.Get failure onto its HTTP status —
// shared by every handler that starts by resolving {id}.
func writeTeamLookupError(w http.ResponseWriter, log *slog.Logger, id string, err error) {
	if errors.Is(err, auth.ErrUnknownTeam) {
		writeError(w, log, http.StatusNotFound, "team_not_found", "no such team "+id)
		return
	}
	log.Error("looking up team", slog.String("team", id), slog.Any("error", err))
	writeError(w, log, http.StatusInternalServerError, "internal_error", "the gateway could not resolve this team")
}
