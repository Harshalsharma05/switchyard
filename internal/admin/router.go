// Package admin serves the operator API on a listener separate from the public
// one.
//
// The separation is the point: Prometheus metrics, team spend, and the Phase 7
// chaos controls must never be reachable on the port serving customer traffic,
// and binding them to a different listener makes that a network fact rather than
// a routing convention.
package admin

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Middleware is the standard net/http decorator shape.
//
// The middleware itself is passed in from cmd/ rather than imported. This
// package deliberately does not import internal/proxy — only cmd/ may — so the
// composition happens at the one place that is allowed to know about both.
type Middleware func(http.Handler) http.Handler

// NewRouter builds the admin listener's handler.
//
// Steps 4.3 and 4.4 mount the team, provider, and reload endpoints here;
// Phase 9 will add promhttp at /metrics. Route paths carry an explicit
// /admin prefix even though the whole listener is already the admin port,
// leaving room for /metrics and future operator endpoints to live at the
// root without colliding with this namespace.
func NewRouter(ready func() bool, teams TeamStore, spend SpendReader, providers ProviderLister, reload Reloader, log *slog.Logger, middleware ...Middleware) http.Handler {
	r := chi.NewRouter()

	for _, mw := range middleware {
		r.Use(mw)
	}

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(ready))

	r.Route("/admin/teams", func(r chi.Router) {
		r.Get("/", listTeams(teams, spend, log))
		r.Get("/{id}", getTeam(teams, spend, log))
		r.Patch("/{id}", patchTeam(teams, spend, log))
		r.Post("/{id}/reset-budget", resetBudget(teams, spend, log))
	})

	r.Get("/admin/providers", listProviders(providers, log))
	r.Post("/admin/reload", reloadConfig(reload, log))

	return r
}

// healthz reports process liveness. It is deliberately trivial: it answers 200
// whenever the process can serve HTTP at all and checks nothing else. A liveness
// probe that consults dependencies causes restarts during an outage that a
// restart cannot fix.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// readyz reports whether the gateway can serve traffic.
func readyz(ready func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not ready"}` + "\n"))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}` + "\n"))
	}
}
