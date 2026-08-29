package admin

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/health"
	"github.com/Harshalsharma05/switchyard/internal/resilience"
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
	Provider         string             `json:"provider"`
	Status           string             `json:"status"`
	ErrorRate        float64            `json:"error_rate"`
	P99LatencyMillis float64            `json:"p99_latency_ms"`
	LastCheckAt      *time.Time         `json:"last_check_at,omitempty"`
	LastTransition   *transitionView    `json:"last_transition,omitempty"`
	History          []transitionView   `json:"history"`
	Breakers         []breakerModelView `json:"breakers"`
}

// breakerModelView is one provider+model circuit breaker's current state, as
// Overview's breaker panel and Phase 4's Live Ops both read it.
type breakerModelView struct {
	Model string `json:"model"`
	State string `json:"state"`
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
	view.Breakers = []breakerModelView{}

	return view
}

// listProviderHealth serves GET /admin/providers/health: Step 5.4's status,
// error rate, p99 latency, last active check, and transition history for every
// provider, in configs/providers.yaml order — plus, since Step 2.4, the
// current state of each provider+model circuit breaker so Overview can show
// health and breaker state from one call.
func listProviderHealth(reader HealthReader, breakers BreakerController, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snaps := reader.Snapshots()

		// Group breaker states by provider once. breakers is nil in a build
		// without the resilience wiring; the field then stays an empty slice.
		byProvider := map[string][]breakerModelView{}
		if breakers != nil {
			for labels, state := range breakers.States() {
				byProvider[labels.Provider] = append(byProvider[labels.Provider],
					breakerModelView{Model: labels.Model, State: state.String()})
			}
			for p := range byProvider {
				sort.Slice(byProvider[p], func(i, j int) bool {
					return byProvider[p][i].Model < byProvider[p][j].Model
				})
			}
		}

		views := make([]providerHealthView, 0, len(snaps))
		for _, s := range snaps {
			v := newProviderHealthView(s)
			if bs := byProvider[s.Provider]; bs != nil {
				v.Breakers = bs
			}
			views = append(views, v)
		}
		writeJSON(w, log, http.StatusOK, views)
	}
}

// BreakerController is the slice of resilience.BreakerRegistry this package
// needs: the manual reset for Step 7.4, and the state read Step 2.4's health
// endpoint reports.
type BreakerController interface {
	// Reset forces every breaker for a provider closed. The int is how many
	// were reset, which distinguishes a provider name that matched nothing
	// from one whose breakers were all already closed.
	Reset(ctx context.Context, providerName string) (int, error)
	// States is the current state of every breaker, keyed by provider+model.
	States() map[resilience.Labels]resilience.State
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
func resetBreaker(resetter BreakerController, providers ProviderLister, log *slog.Logger) http.HandlerFunc {
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
