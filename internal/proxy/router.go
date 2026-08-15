package proxy

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter builds the public listener's handler.
//
// # Middleware chain order
//
// The order is defined here and nowhere else. Each layer wraps everything below
// it, so the first entry is the outermost.
//
//  1. Recoverer  — outermost, so it catches a panic raised in any layer below,
//     including one raised inside another middleware.
//  2. RequestID  — must run before Logger, or log lines have no correlation ID.
//     It also sets a response header, so it must run before anything
//     that could write a response.
//  3. Timing     — outside Logger so the overhead it reports covers every layer
//     below it. Anything inserted after this point is counted as
//     gateway overhead, which is the honest accounting: work the
//     gateway does is the gateway's cost.
//  4. Logger     — innermost, so the duration it records covers the handler and
//     nothing else's setup, and so it can read the finished timings.
//
// Phases 3, 4, and 8 insert auth, rate limiting, budget, and tracing between
// Timing and Logger — inside Timing on purpose, so their latency shows up in the
// overhead number rather than hiding from it. Auth goes after RequestID so a
// rejected request is still correlated; tracing goes outside auth so a rejection
// still produces a span.
func NewRouter(resolver Resolver, log *slog.Logger, ready func() bool) http.Handler {
	h := NewHandler(resolver, log)

	r := chi.NewRouter()

	r.Use(Recoverer(log))
	r.Use(RequestID)
	r.Use(Timing)
	r.Use(Logger(log))

	r.Post("/v1/chat/completions", h.ChatCompletions)

	// Probes live on the public listener because that is what a load balancer
	// in front of the gateway can reach. They are also mounted on the admin
	// listener for operators.
	r.Get("/healthz", Healthz)
	r.Get("/readyz", Readyz(ready))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeError(w, log, http.StatusNotFound, "not_found", "no such endpoint")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		writeError(w, log, http.StatusMethodNotAllowed, "method_not_allowed",
			"that method is not allowed on this endpoint")
	})

	return r
}

// Healthz reports process liveness. It is deliberately trivial: it answers 200
// whenever the process can serve HTTP at all, and checks nothing else. A
// liveness probe that consults dependencies causes restarts during an outage
// that the restart cannot fix.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// Readyz reports whether the gateway can serve traffic — in Phase 1, whether
// config loaded and the registry was built.
//
// Phase 5 extends this to consider provider health, where the rule is that only
// *all* providers being down makes the gateway unready. One bad provider is a
// routing problem, not a reason to be pulled from a load balancer.
func Readyz(ready func() bool) http.HandlerFunc {
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
