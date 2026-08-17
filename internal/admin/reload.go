package admin

import (
	"context"
	"log/slog"
	"net/http"
)

// ReloadSummary is what a successful reload reports back — enough for an
// operator to sanity-check "did this pick up the change I expected" without
// this package needing to know anything about what providers or teams are.
type ReloadSummary struct {
	Providers int `json:"providers"`
	Teams     int `json:"teams"`
}

// Reloader is what POST /admin/reload calls. cmd/gateway supplies the
// concrete implementation: reading configs/*.yaml, rebuilding every
// registry, and swapping them in only if every step succeeds. Declared here,
// by the consumer, for the same reason TeamStore and ProviderLister are —
// this package only needs to trigger a reload and report the outcome, not
// orchestrate one, and orchestrating across config, provider, auth, and
// budget is exactly the cross-package wiring only cmd/ is allowed to do.
type Reloader func(ctx context.Context) (ReloadSummary, error)

type reloadResponse struct {
	Status    string `json:"status"`
	Providers int    `json:"providers"`
	Teams     int    `json:"teams"`
}

// reloadConfig serves POST /admin/reload.
//
// A rejected reload is reported as 400 with the validation error and
// changes nothing — Reloader's contract (see cmd/gateway/reload.go) is that
// a failure never touches whatever is currently live, so the gateway keeps
// serving the old, still-valid config exactly as the Step 4.4 checklist
// requires.
//
// Both outcomes are logged, per the checklist's "reload event logged and
// exposed as a metric" — the counter half of that arrives with the rest of
// Phase 9's metrics; a log line is the whole story until then, the same
// stopgap every other pre-Phase-9 event in this codebase uses.
func reloadConfig(reload Reloader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		summary, err := reload(r.Context())
		if err != nil {
			log.LogAttrs(r.Context(), slog.LevelWarn, "admin config reload rejected",
				slog.String("actor", r.RemoteAddr),
				slog.Any("error", err),
			)
			writeError(w, log, http.StatusBadRequest, "invalid_config", "reload rejected: "+err.Error())
			return
		}

		log.LogAttrs(r.Context(), slog.LevelInfo, "admin config reloaded",
			slog.String("actor", r.RemoteAddr),
			slog.Int("providers", summary.Providers),
			slog.Int("teams", summary.Teams),
		)
		writeJSON(w, log, http.StatusOK, reloadResponse{Status: "reloaded", Providers: summary.Providers, Teams: summary.Teams})
	}
}
