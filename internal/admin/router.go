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

// Auth is the dashboard identity surface, built in cmd/ and passed in whole.
// Router is mounted at /auth; Session and CSRF wrap the /admin group.
type Auth struct {
	Router  http.Handler
	Session Middleware
	CSRF    Middleware
}

// NewRouter builds the admin listener's handler.
//
// Steps 4.3 and 4.4 mount the team, provider, and reload endpoints here;
// Step 5.4 adds the health endpoint; Phase 9 will add promhttp at /metrics.
// Route paths carry an explicit /admin prefix even though the whole listener
// is already the admin port, leaving room for /metrics and future operator
// endpoints to live at the root without colliding with this namespace.
func NewRouter(ready func() bool, teams TeamStore, spend SpendReader, providers ProviderLister, healthReader HealthReader, breakers BreakerController, chaos ChaosController, reload Reloader, requestLog RequestLogReader, summarySvc SummaryService, cacheTuner CacheTuner, costCalc CostCalculator, qualityFeedback QualityFeedbackConfig, qualityEnabled bool, audit AuditRecorder, system SystemReporter, auth Auth, metrics *telemetry.Metrics, log *slog.Logger, middleware ...Middleware) http.Handler {
	r := chi.NewRouter()

	for _, mw := range middleware {
		r.Use(mw)
	}

	// Dashboard sign-in. Built in cmd/ from internal/identity and handed in as
	// opaque handlers and middleware, the same way Middleware already is: this
	// package never imports identity, so the two authentication systems cannot
	// leak into each other. Nil when OAuth is unconfigured.
	if auth.Router != nil {
		r.Mount("/auth", auth.Router)
	}

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(ready))
	r.Handle("/metrics", promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{}))

	// Everything under /admin now requires a signed-in session. A team API key
	// reaches none of it: the session middleware never reads the Authorization
	// header, so a key is not refused by a rule -- it is not an input.
	r.Group(func(r chi.Router) {
		if auth.Session != nil {
			r.Use(auth.Session)
		}
		if auth.CSRF != nil {
			r.Use(auth.CSRF)
		}

		r.Get("/admin/summary", handleSummary(summarySvc, healthReader, cacheTuner != nil, qualityEnabled, log))
		r.Get("/admin/requests", listRequests(requestLog, log))
		r.Get("/admin/requests/{id}", getRequest(requestLog, log))
		r.Get("/admin/costs", handleCosts(requestLog, log))
		r.Get("/admin/attribution", handleAttribution(requestLog, costCalc, log))
		r.Get("/admin/providers", listProviders(providers, log))
		r.Get("/admin/providers/health", listProviderHealth(healthReader, breakers, log))

		// Everything that crosses tenants. Superadmin-only until Phase 2
		// splits org admin from superadmin -- fail closed, not unscoped.
		r.Group(func(r chi.Router) {
			r.Use(requireSuperadmin(log))

			r.Route("/admin/teams", func(r chi.Router) {
				r.Get("/", listTeams(teams, spend, log))
				r.Post("/", createTeam(teams, providers, audit, log))
				r.Get("/{id}", getTeam(teams, spend, log))
				r.Patch("/{id}", patchTeam(teams, spend, audit, log))
				r.Delete("/{id}", deleteTeam(teams, audit, log))
				r.Post("/{id}/reset-budget", resetBudget(teams, spend, audit, log))
				r.Post("/{id}/key/rotate", rotateKey(teams, audit, log))
				r.Delete("/{id}/key", revokeKey(teams, audit, log))
			})

			r.Get("/admin/audit", listAudit(audit, log))
			r.Get("/admin/system", handleSystem(system, providers, log))
			r.Get("/admin/reconciliation", handleReconciliation(teams, spend, requestLog, log))
			r.Post("/admin/providers/{name}/breaker/reset", resetBreaker(breakers, providers, log))
			r.Post("/admin/reload", reloadConfig(reload, audit, log))
			r.Get("/admin/cache/tune", tuneCache(cacheTuner, log))
			r.Get("/admin/quality/feedback", handleQualityFeedback(requestLog, qualityFeedback, log))
			r.Delete("/admin/cache", purgeCache(cacheTuner, log))

			r.Route("/admin/chaos", func(r chi.Router) {
				r.Get("/", getChaos(chaos, log))
				r.Post("/", setChaos(chaos, log))
				r.Delete("/", deleteChaos(chaos, log))
			})
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
