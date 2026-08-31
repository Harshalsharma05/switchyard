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
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Harshalsharma05/switchyard/internal/telemetry"
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
// Step 5.4 adds the health endpoint; Phase 9 will add promhttp at /metrics.
// Route paths carry an explicit /admin prefix even though the whole listener
// is already the admin port, leaving room for /metrics and future operator
// endpoints to live at the root without colliding with this namespace.
func NewRouter(ready func() bool, teams TeamStore, spend SpendReader, providers ProviderLister, healthReader HealthReader, breakers BreakerController, chaos ChaosController, reload Reloader, requestLog RequestLogReader, authr KeyAuthenticator, summarySvc SummaryService, cacheTuner CacheTuner, costCalc CostCalculator, routing RoutingInfo, metrics *telemetry.Metrics, log *slog.Logger, middleware ...Middleware) http.Handler {
	r := chi.NewRouter()

	for _, mw := range middleware {
		r.Use(mw)
	}

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(ready))
	r.Handle("/metrics", promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{}))

	// Routes that take a bearer key and scope their own response to the caller.
	// /admin/me is the frontend's first call on load; the request-log routes
	// pin a non-admin to its own rows internally. Neither is behind the admin
	// gate — any valid key reaches them.
	r.Get("/admin/me", handleMe(authr, spend, routing, log))
	r.Get("/admin/summary", handleSummary(summarySvc, healthReader, cacheTuner != nil, authr, log))
	r.Get("/admin/requests", listRequests(requestLog, authr, log))
	r.Get("/admin/requests/{id}", getRequest(requestLog, authr, log))
	r.Get("/admin/costs", handleCosts(requestLog, authr, log))
	r.Get("/admin/attribution", handleAttribution(requestLog, costCalc, authr, log))

	// Read-only operator surface: no key, same as the Prometheus scrape.
	r.Get("/admin/providers", listProviders(providers, log))
	r.Get("/admin/providers/health", listProviderHealth(healthReader, breakers, log))

	// Everything mutating or cross-team now requires an admin key (Step 2.1).
	// requireAdmin is inert when authr is nil, so tests that drive these
	// handlers directly still work and a registry-less build keeps Part 1's
	// open operator port.
	r.Group(func(r chi.Router) {
		r.Use(requireAdmin(authr, log))

		r.Route("/admin/teams", func(r chi.Router) {
			r.Get("/", listTeams(teams, spend, log))
			r.Get("/{id}", getTeam(teams, spend, log))
			r.Patch("/{id}", patchTeam(teams, spend, log))
			r.Post("/{id}/reset-budget", resetBudget(teams, spend, log))
		})

		// Step 6.1: cross-checks Redis budget counters against the request log.
		r.Get("/admin/reconciliation", handleReconciliation(teams, spend, requestLog, log))

		// Registered after /admin/providers/health so chi's static-over-wildcard
		// precedence is not the only thing keeping "health" from being read as a
		// provider name; the literal route is more specific either way.
		r.Post("/admin/providers/{name}/breaker/reset", resetBreaker(breakers, providers, log))
		r.Post("/admin/reload", reloadConfig(reload, log))

		// Step 7.3's threshold tuning sweep. Answers 404 when the cache is
		// disabled, matching the chaos routes: the routing table stays the
		// same shape in every environment.
		r.Get("/admin/cache/tune", tuneCache(cacheTuner, log))

		// Step 7.4's manual invalidation. Registered under the admin gate
		// because a purge crosses every team's entries.
		r.Delete("/admin/cache", purgeCache(cacheTuner, log))

		// Step 7.5's chaos controls. The routes are always registered and answer
		// 404 unless the harness is available, rather than being conditionally
		// mounted: the guard that matters is inside the harness, and keeping the
		// routing table identical in every environment means a production gateway
		// is indistinguishable from a build that never had chaos compiled in.
		r.Route("/admin/chaos", func(r chi.Router) {
			r.Get("/", getChaos(chaos, log))
			r.Post("/", setChaos(chaos, log))
			r.Delete("/", deleteChaos(chaos, log))
		})
	})

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
