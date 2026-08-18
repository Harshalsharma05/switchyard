package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/health"
)

// HealthReader is the slice of health.Monitor this package needs.
type HealthReader interface {
	Snapshots() []health.ProviderHealth
}

// transitionView is one recorded status change, as GET /admin/providers/health
// reports it.
type transitionView struct {
	At     time.Time `json:"at"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
}

func newTransitionView(t health.Transition) transitionView {
	return transitionView{At: t.At, From: t.From.String(), To: t.To.String(), Reason: t.Reason}
}

// providerHealthView is GET /admin/providers/health's response shape for one
// provider: Step 5.4's status, error rate, p99, last check, and transition
// history.
type providerHealthView struct {
	Provider         string           `json:"provider"`
	Status           string           `json:"status"`
	ErrorRate        float64          `json:"error_rate"`
	P99LatencyMillis float64          `json:"p99_latency_ms"`
	LastCheckAt      *time.Time       `json:"last_check_at,omitempty"`
	LastTransition   *transitionView  `json:"last_transition,omitempty"`
	History          []transitionView `json:"history"`
}

func newProviderHealthView(h health.ProviderHealth) providerHealthView {
	view := providerHealthView{
		Provider:         h.Provider,
		Status:           h.Status.String(),
		ErrorRate:        h.ErrorRate,
		P99LatencyMillis: float64(h.P99Latency) / float64(time.Millisecond),
		History:          make([]transitionView, 0, len(h.History)),
	}

	// LastCheckAt is the zero time until Observe has run at least once for
	// this provider — omitted rather than serialized as "0001-01-01," which
	// would read as a real, very stale timestamp instead of "no check yet."
	if !h.LastCheckAt.IsZero() {
		t := h.LastCheckAt
		view.LastCheckAt = &t
	}
	if h.LastTransition != nil {
		tv := newTransitionView(*h.LastTransition)
		view.LastTransition = &tv
	}
	for _, t := range h.History {
		view.History = append(view.History, newTransitionView(t))
	}

	return view
}

// listProviderHealth serves GET /admin/providers/health: Step 5.4's status,
// error rate, p99 latency, last active check, and transition history for
// every provider, in configs/providers.yaml order.
func listProviderHealth(reader HealthReader, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snaps := reader.Snapshots()
		views := make([]providerHealthView, 0, len(snaps))
		for _, s := range snaps {
			views = append(views, newProviderHealthView(s))
		}
		writeJSON(w, log, http.StatusOK, views)
	}
}

// BreakerResetter is the slice of resilience.BreakerRegistry this package
// needs. The int is how many breakers were reset, which is what distinguishes
// a provider name that matched nothing from one whose breakers were all
// already closed.
type BreakerResetter interface {
	Reset(ctx context.Context, providerName string) (int, error)
}

// breakerResetView is POST /admin/providers/{name}/breaker/reset's response.
type breakerResetView struct {
	Provider string `json:"provider"`
	Reset    int    `json:"breakers_reset"`
}

// resetBreaker serves Step 7.4's manual intervention: force every breaker
// belonging to one provider back to Closed, without waiting out a cooldown.
//
// It is scoped to a provider rather than to a single provider+model because
// that is the path the plan specifies, and because it matches the situation
// it exists for — an operator who has just fixed something and knows the
// whole provider is good again.
//
// An unknown provider name is a 404 rather than a silent success, so a typo
// in an incident-time command does not read as "done." Note that a provider
// with no breakers *built yet* is indistinguishable from an unknown one here,
// which is correct in effect: in both cases nothing was open, so there was
// nothing to reset.
func resetBreaker(resetter BreakerResetter, providers ProviderLister, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")

		if !providerExists(providers, name) {
			writeError(w, log, http.StatusNotFound, "provider_not_found",
				"no provider named "+strconv.Quote(name)+" is configured")
			return
		}

		count, err := resetter.Reset(r.Context(), name)
		if err != nil {
			// The local reset still happened — see Breaker.Reset — so this is
			// reported as a partial success rather than a flat failure: this
			// replica is clear, the rest of the fleet may not be.
			log.ErrorContext(r.Context(), "resetting circuit breakers",
				slog.String("provider", name), slog.Any("error", err))
			writeError(w, log, http.StatusBadGateway, "breaker_reset_failed",
				"breakers were reset on this gateway instance but the shared state could not be cleared: "+err.Error())
			return
		}

		// Step 4.3's rule that every mutation is logged applies here too.
		log.InfoContext(r.Context(), "circuit breakers reset",
			slog.String("provider", name), slog.Int("breakers_reset", count))

		writeJSON(w, log, http.StatusOK, breakerResetView{Provider: name, Reset: count})
	}
}

// providerExists reports whether name is a configured provider, so the
// handler can 404 a typo instead of reporting a no-op as a success.
func providerExists(providers ProviderLister, name string) bool {
	for _, cfg := range providers.Configs() {
		if cfg.Name == name {
			return true
		}
	}
	return false
}
