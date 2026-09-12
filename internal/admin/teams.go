package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/auth"
	"github.com/Harshalsharma05/switchyard/internal/logstore"
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
// The mutations take a context and the reads do not: since Tier 1 Phase 2 a
// write goes to Postgres, while a read is answered from the in-memory snapshot
// and does no I/O at all.
type TeamStore interface {
	List() []auth.Team
	Get(id string) (auth.Team, error)
	Update(ctx context.Context, id string, patch auth.TeamPatch) (auth.Team, error)
	RotateKey(ctx context.Context, id, newHash, newMasked string) (auth.Team, error)
	RevokeKey(ctx context.Context, id string) (auth.Team, error)
	Create(ctx context.Context, t auth.Team) (auth.Team, error)
	Delete(ctx context.Context, id string) (auth.Team, error)
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
	OrganizationID    string         `json:"organization_id"`
	AllowedProviders  []string       `json:"allowed_providers"`
	AllowedModels     []string       `json:"allowed_models"`
	RateLimits        rateLimitsView `json:"rate_limits"`
	MonthlyBudgetUSD  float64        `json:"monthly_budget_usd"`
	SpentUSD          *float64       `json:"spent_usd"`
	BudgetUtilization *float64       `json:"budget_utilization"`
	Priority          string         `json:"priority"`
	IsAdmin           bool           `json:"is_admin"`

	// Key is the only key information any GET returns: where the current key
	// came from, a display-only mask for one this gateway minted, and when.
	// Never the key, never the hash — Step 6.4, tightened in Part 2 Step 1.
	Key keyView `json:"key"`
}

// keyView is a team's key metadata. Masked is empty for a config-seeded team —
// the gateway never saw that key's plaintext, so there is nothing honest to
// show, and inventing a mask from the hash would be fabricated data. CreatedAt
// is null in the same case.
type keyView struct {
	Source    string     `json:"source"`
	Masked    string     `json:"masked,omitempty"`
	CreatedAt *time.Time `json:"created_at"`
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

// keyViewOf renders a team's key metadata. An unset KeySource is read as
// "config": the older test fixtures and any team built before Part 2 Step 1
// predate the field, and every one of them is a YAML-seeded key.
func keyViewOf(t auth.Team) keyView {
	source := t.KeySource
	if source == "" {
		source = auth.KeySourceConfig
	}
	return keyView{Source: source, Masked: t.KeyMasked, CreatedAt: t.KeyCreatedAt}
}

// newTeamView builds the response shape for one team. spentMicros is nil
// when spend was not read (a Redis failure, logged by the caller) — see the
// teamView doc comment for why that stays null rather than becoming 0.
func newTeamView(t auth.Team, spentMicros *int64) teamView {
	v := teamView{
		ID:               t.ID,
		Name:             t.Name,
		OrganizationID:   t.OrganizationID,
		AllowedProviders: t.AllowedProviders,
		AllowedModels:    t.AllowedModels,
		RateLimits:       rateLimitsView{RPM: t.RateLimits.RPM, TPM: t.RateLimits.TPM},
		MonthlyBudgetUSD: microsToUSD(t.MonthlyBudgetMicros),
		Priority:         string(t.Priority),
		IsAdmin:          t.IsAdmin,
		Key:              keyViewOf(t),
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

// limitsDelta is the {rpm, tpm, monthly_budget_usd} snapshot an audit entry
// records for a team-limit change. Kept small and typed so before/after are
// directly comparable in the audit view.
func limitsDelta(t auth.Team) map[string]any {
	return map[string]any{
		"rpm":                t.RateLimits.RPM,
		"tpm":                t.RateLimits.TPM,
		"monthly_budget_usd": microsToUSD(t.MonthlyBudgetMicros),
	}
}

// auditUnavailable is the 503 a mutation returns when its audit entry could not
// be written. The mutation is not attempted: a change the audit log missed is
// exactly what audit-before-mutate exists to prevent.
func auditUnavailable(w http.ResponseWriter, log *slog.Logger) {
	writeError(w, log, http.StatusServiceUnavailable, "audit_unavailable",
		"the change was not applied because it could not be recorded to the audit log; try again shortly")
}

// patchTeam serves PATCH /admin/teams/{id}.
//
// The audit entry is written before store.Update, so a mutation the audit log
// missed cannot happen. Its `after` is the caller's requested values — if
// Update then rejects them (a non-positive limit), the entry stands as an
// attempt, which is the accepted cost of that ordering (see DECISIONS.md).
func patchTeam(store TeamStore, spend SpendReader, audit AuditRecorder, log *slog.Logger) http.HandlerFunc {
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

		requested := limitsDelta(before)
		if body.RPM != nil {
			requested["rpm"] = *body.RPM
		}
		if body.TPM != nil {
			requested["tpm"] = *body.TPM
		}
		if body.MonthlyBudgetUSD != nil {
			requested["monthly_budget_usd"] = *body.MonthlyBudgetUSD
		}

		if err := recordAudit(r.Context(), audit, logstore.AuditEntry{
			ActorTeamID:  actorID(r),
			ActorAddr:    r.RemoteAddr,
			Action:       "team.patch",
			TargetTeamID: id,
			Before:       limitsDelta(before),
			After:        requested,
		}); err != nil {
			log.ErrorContext(r.Context(), "writing team-patch audit entry", slog.Any("error", err))
			auditUnavailable(w, log)
			return
		}

		after, err := store.Update(r.Context(), id, patch)
		if err != nil {
			if errors.Is(err, auth.ErrUnknownTeam) {
				writeError(w, log, http.StatusNotFound, "team_not_found", "no such team "+id)
				return
			}
			writeError(w, log, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin patched team",
			slog.String("actor", actorID(r)),
			slog.String("actor_addr", r.RemoteAddr),
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
func resetBudget(store TeamStore, spend SpendReader, audit AuditRecorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		team, err := store.Get(id)
		if err != nil {
			writeTeamLookupError(w, log, id, err)
			return
		}

		before := readSpent(r.Context(), spend, log, id)
		beforeUSD := 0.0
		if before != nil {
			beforeUSD = microsToUSD(*before)
		}

		if err := recordAudit(r.Context(), audit, logstore.AuditEntry{
			ActorTeamID:  actorID(r),
			ActorAddr:    r.RemoteAddr,
			Action:       "team.budget_reset",
			TargetTeamID: id,
			Before:       map[string]any{"spent_usd": beforeUSD},
			After:        map[string]any{"spent_usd": 0.0},
		}); err != nil {
			log.ErrorContext(r.Context(), "writing budget-reset audit entry", slog.Any("error", err))
			auditUnavailable(w, log)
			return
		}

		if err := spend.Reset(r.Context(), id); err != nil {
			log.ErrorContext(r.Context(), "resetting team budget",
				slog.String("team", id), slog.Any("error", err))
			writeError(w, log, http.StatusServiceUnavailable, "budget_reset_failed",
				"the gateway could not reset this team's budget; try again shortly")
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin reset team budget",
			slog.String("actor", actorID(r)),
			slog.String("actor_addr", r.RemoteAddr),
			slog.String("team", id),
			slog.Float64("before_spent_usd", beforeUSD),
			slog.Float64("after_spent_usd", 0),
		)

		zero := int64(0)
		writeJSON(w, log, http.StatusOK, newTeamView(team, &zero))
	}
}

// rotateKeyResponse is POST /admin/teams/{id}/key/rotate's body. APIKey is the
// only time the plaintext key is ever returned — it is not stored, not logged,
// and no later GET can reproduce it.
type rotateKeyResponse struct {
	APIKey  string  `json:"api_key"`
	Key     keyView `json:"key"`
	Warning string  `json:"warning"`
}

const rotationWarning = "Copy this key now — it is shown once and never again. The rotation is durable: it survives a gateway restart and a POST /admin/reload. On a multi-replica deployment the previous key can still authenticate on other replicas until their next team-snapshot refresh."

// rotateKey serves POST /admin/teams/{id}/key/rotate: mint a new key, record
// the rotation, then swap it in. The old key stops working the instant
// store.RotateKey returns; the new key works on the very next request, no
// restart.
func rotateKey(store TeamStore, audit AuditRecorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		before, err := store.Get(id)
		if err != nil {
			writeTeamLookupError(w, log, id, err)
			return
		}

		raw, err := auth.GenerateKey(id)
		if err != nil {
			log.ErrorContext(r.Context(), "generating rotated key", slog.String("team", id), slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error",
				"the gateway could not generate a new key")
			return
		}
		masked := auth.MaskKey(raw)

		// The audit records only that the key source changed — never the key,
		// the hash, or even the mask.
		if err := recordAudit(r.Context(), audit, logstore.AuditEntry{
			ActorTeamID:  actorID(r),
			ActorAddr:    r.RemoteAddr,
			Action:       "key.rotate",
			TargetTeamID: id,
			Before:       map[string]any{"key_source": keyViewOf(before).Source},
			After:        map[string]any{"key_source": auth.KeySourceRotated},
		}); err != nil {
			log.ErrorContext(r.Context(), "writing key-rotate audit entry", slog.Any("error", err))
			auditUnavailable(w, log)
			return
		}

		after, err := store.RotateKey(r.Context(), id, auth.HashKey(raw), masked)
		if err != nil {
			if errors.Is(err, auth.ErrUnknownTeam) {
				writeError(w, log, http.StatusNotFound, "team_not_found", "no such team "+id)
				return
			}
			log.ErrorContext(r.Context(), "rotating team key", slog.String("team", id), slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error", "the key could not be rotated")
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin rotated team key",
			slog.String("actor", actorID(r)),
			slog.String("actor_addr", r.RemoteAddr),
			slog.String("team", id),
		)

		writeJSON(w, log, http.StatusOK, rotateKeyResponse{
			APIKey:  raw,
			Key:     keyViewOf(after),
			Warning: rotationWarning,
		})
	}
}

// revokeKey serves DELETE /admin/teams/{id}/key: remove a team's key entirely.
// The team stays; nothing authenticates as it until an admin rotates it a new
// key.
func revokeKey(store TeamStore, audit AuditRecorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		before, err := store.Get(id)
		if err != nil {
			writeTeamLookupError(w, log, id, err)
			return
		}

		if err := recordAudit(r.Context(), audit, logstore.AuditEntry{
			ActorTeamID:  actorID(r),
			ActorAddr:    r.RemoteAddr,
			Action:       "key.revoke",
			TargetTeamID: id,
			Before:       map[string]any{"key_source": keyViewOf(before).Source},
			After:        map[string]any{"key_source": auth.KeySourceRevoked},
		}); err != nil {
			log.ErrorContext(r.Context(), "writing key-revoke audit entry", slog.Any("error", err))
			auditUnavailable(w, log)
			return
		}

		after, err := store.RevokeKey(r.Context(), id)
		if err != nil {
			if errors.Is(err, auth.ErrUnknownTeam) {
				writeError(w, log, http.StatusNotFound, "team_not_found", "no such team "+id)
				return
			}
			log.ErrorContext(r.Context(), "revoking team key", slog.String("team", id), slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error", "the key could not be revoked")
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin revoked team key",
			slog.String("actor", actorID(r)),
			slog.String("actor_addr", r.RemoteAddr),
			slog.String("team", id),
		)

		// Spend is not read here — this response is about the key, and a Redis
		// hiccup must not turn a successful revoke into an error.
		writeJSON(w, log, http.StatusOK, newTeamView(after, nil))
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

// --- create and delete (Tier 1, Step 2.5) ---------------------------------

// createTeamRequest is POST /admin/teams' body. Flat, like a PATCH body, and
// with no id: the ID is derived from the name, once, and never changes.
type createTeamRequest struct {
	Name             string   `json:"name"`
	OrganizationID   string   `json:"organization_id"`
	Priority         string   `json:"priority"`
	RPM              int      `json:"rpm"`
	TPM              int      `json:"tpm"`
	MonthlyBudgetUSD float64  `json:"monthly_budget_usd"`
	AllowedProviders []string `json:"allowed_providers"`
	AllowedModels    []string `json:"allowed_models"`
	IsAdmin          bool     `json:"is_admin"`
}

// createTeamResponse is a superset of rotateKeyResponse, so the console's
// existing show-once key panel can render either one. APIKey is the only time
// this team's plaintext key is ever returned.
type createTeamResponse struct {
	Team    teamView `json:"team"`
	APIKey  string   `json:"api_key"`
	Key     keyView  `json:"key"`
	Warning string   `json:"warning"`
}

const creationWarning = "Copy this key now — it is shown once and never again. It works immediately and survives a gateway restart."

// teamSettings is what an audit entry records about a whole team: its settings,
// never its key, hash, or mask.
func teamSettings(t auth.Team) map[string]any {
	d := limitsDelta(t)
	d["name"] = t.Name
	d["priority"] = string(t.Priority)
	d["allowed_providers"] = t.AllowedProviders
	d["allowed_models"] = t.AllowedModels
	d["is_admin"] = t.IsAdmin
	if t.OrganizationID != "" {
		d["organization_id"] = t.OrganizationID
	}
	return d
}

// validateAllowlists checks a new team's allowlists against the live provider
// config, so a typo is a 400 now rather than a team whose every request 403s
// later. Point-in-time by nature: a later providers.yaml edit can still leave a
// stale entry, exactly as it can for a seeded team.
func validateAllowlists(providers ProviderLister, allowedProviders, allowedModels []string) error {
	knownProviders := map[string]bool{}
	knownModels := map[string]bool{}
	for _, cfg := range providers.Configs() {
		knownProviders[cfg.Name] = true
		for _, m := range cfg.Models {
			knownModels[m] = true
		}
	}
	for _, p := range allowedProviders {
		if !knownProviders[p] {
			return fmt.Errorf("allowed provider %q is not configured in providers.yaml", p)
		}
	}
	for _, m := range allowedModels {
		if !knownModels[m] {
			return fmt.Errorf("allowed model %q is not served by any configured provider", m)
		}
	}
	return nil
}

func writeTeamExists(w http.ResponseWriter, log *slog.Logger, id string) {
	writeError(w, log, http.StatusConflict, "team_exists",
		"a team with id "+id+" exists or once existed; team IDs come from the name and are never reused, so choose a different name")
}

// createTeam serves POST /admin/teams: validate, mint a key, record the creation,
// then persist it. The plaintext key is in the 201 body and nowhere else — the
// same show-once contract as a rotation — and the audit row records the team's
// settings, never the key.
func createTeam(store TeamStore, providers ProviderLister, audit AuditRecorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body createTeamRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"request body is not valid JSON: "+err.Error())
			return
		}

		id := auth.Slug(body.Name)
		if id == "" {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error",
				"name must contain at least one ASCII letter or digit")
			return
		}
		team := auth.Team{
			ID:                  id,
			OrganizationID:      body.OrganizationID,
			Name:                strings.TrimSpace(body.Name),
			AllowedProviders:    body.AllowedProviders,
			AllowedModels:       body.AllowedModels,
			RateLimits:          auth.RateLimits{RPM: body.RPM, TPM: body.TPM},
			MonthlyBudgetMicros: usdToMicros(body.MonthlyBudgetUSD),
			Priority:            auth.Priority(body.Priority),
			IsAdmin:             body.IsAdmin,
		}
		if err := team.Validate(); err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if err := validateAllowlists(providers, team.AllowedProviders, team.AllowedModels); err != nil {
			writeError(w, log, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		// Checked here as well as by the database, so an obvious conflict gets a
		// clear 409 without an audit row for an attempt that could never succeed.
		// A soft-deleted team's ID is not in the snapshot; the insert catches it.
		if _, err := store.Get(id); err == nil {
			writeTeamExists(w, log, id)
			return
		}

		raw, err := auth.GenerateKey(id)
		if err != nil {
			log.ErrorContext(r.Context(), "generating new team key", slog.String("team", id), slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error", "the gateway could not generate a key")
			return
		}
		// All of the key's metadata is set here, where the key is minted, rather
		// than split between this handler and the store.
		now := time.Now().UTC()
		team.KeyHash = auth.HashKey(raw)
		team.KeyMasked = auth.MaskKey(raw)
		team.KeySource = auth.KeySourceCreated
		team.KeyCreatedAt = &now

		if err := recordAudit(r.Context(), audit, logstore.AuditEntry{
			ActorTeamID:  actorID(r),
			ActorAddr:    r.RemoteAddr,
			Action:       "team.create",
			TargetTeamID: id,
			Before:       map[string]any{},
			After:        teamSettings(team),
		}); err != nil {
			log.ErrorContext(r.Context(), "writing team-create audit entry", slog.Any("error", err))
			auditUnavailable(w, log)
			return
		}

		created, err := store.Create(r.Context(), team)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrTeamExists):
				writeTeamExists(w, log, id)
			case errors.Is(err, auth.ErrUnknownOrganization):
				writeError(w, log, http.StatusBadRequest, "invalid_request_error",
					"no such organization "+body.OrganizationID)
			default:
				log.ErrorContext(r.Context(), "creating team", slog.String("team", id), slog.Any("error", err))
				writeError(w, log, http.StatusInternalServerError, "internal_error", "the team could not be created")
			}
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin created team",
			slog.String("actor", actorID(r)),
			slog.String("actor_addr", r.RemoteAddr),
			slog.String("team", id),
		)

		writeJSON(w, log, http.StatusCreated, createTeamResponse{
			Team:    newTeamView(created, nil),
			APIKey:  raw,
			Key:     keyViewOf(created),
			Warning: creationWarning,
		})
	}
}

// deleteTeam serves DELETE /admin/teams/{id}: a soft delete. The key stops
// working on the next request, the row stays so request-log and audit history
// keep pointing at a real team, and the ID is never reused.
func deleteTeam(store TeamStore, audit AuditRecorder, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		before, err := store.Get(id)
		if err != nil {
			writeTeamLookupError(w, log, id, err)
			return
		}

		if err := recordAudit(r.Context(), audit, logstore.AuditEntry{
			ActorTeamID:  actorID(r),
			ActorAddr:    r.RemoteAddr,
			Action:       "team.delete",
			TargetTeamID: id,
			Before:       teamSettings(before),
			After:        map[string]any{"deleted": true},
		}); err != nil {
			log.ErrorContext(r.Context(), "writing team-delete audit entry", slog.Any("error", err))
			auditUnavailable(w, log)
			return
		}

		if _, err := store.Delete(r.Context(), id); err != nil {
			if errors.Is(err, auth.ErrUnknownTeam) {
				writeError(w, log, http.StatusNotFound, "team_not_found", "no such team "+id)
				return
			}
			log.ErrorContext(r.Context(), "deleting team", slog.String("team", id), slog.Any("error", err))
			writeError(w, log, http.StatusInternalServerError, "internal_error", "the team could not be deleted")
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin deleted team",
			slog.String("actor", actorID(r)),
			slog.String("actor_addr", r.RemoteAddr),
			slog.String("team", id),
		)
		w.WriteHeader(http.StatusNoContent)
	}
}
