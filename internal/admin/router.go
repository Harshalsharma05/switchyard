// Package admin serves the operator API on a listener separate from the public
// one.
//
// The separation is the point: Prometheus metrics, team spend, and the Phase 7
// chaos controls must never be reachable on the port serving customer traffic,
// and binding them to a different listener makes that a network fact rather than
// a routing convention.
package admin

import (
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
// Phase 4 mounts the team and provider endpoints here, and Phase 9 mounts
// promhttp at /metrics. For now it carries only the probes, so operators can
// check readiness without going through the public port.
func NewRouter(ready func() bool, middleware ...Middleware) http.Handler {
	r := chi.NewRouter()

	for _, mw := range middleware {
		r.Use(mw)
	}

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(ready))

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
