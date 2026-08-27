package proxy

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Harshalsharma05/switchyard/internal/resilience"
	"github.com/Harshalsharma05/switchyard/internal/telemetry"
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
//  4. Tracing    — opens switchyard.request, Step 8.2's root span, and wraps
//     the whole router (probes included) for the same reason Timing
//     and Logger do: switchyard.auth and switchyard.ratelimit must be
//     children of it, which only holds if the span exists before Auth
//     runs. (An earlier version of this comment placed tracing inside
//     Auth's route group; that would have made the root span start
//     after Auth already ran, which cannot produce the child spans
//     Step 8.2 asks for. Corrected here rather than left to rot.)
//  5. Logger     — outside Auth (and, from Phase 4, rate limiting and budget),
//     so every request gets exactly one log line, including a 401 or
//     a 429 — those middlewares reject by returning without calling
//     next, which would make a Logger placed *inside* them silently
//     skip logging the very requests it matters most to see.
//  6. Metrics    — Phase 9, mounted on the API route group only, not the
//     whole router: switchyard_requests_total's {team, provider, model}
//     labels mean nothing for a load balancer's liveness probe, and
//     giving that traffic its own always-empty label combination would
//     just be noise. Outside Auth within the group for the same reason
//     Logger is outside it at the router level — a 401 or 429 rejection
//     must still be counted, not silently skipped because the middleware
//     that would have recorded it never got called.
//  7. RequestLog — Part 2 Phase 1, mounted on the API route group for the same
//     reason Metrics is, and outside Auth so a 429, 402, or 403 is
//     logged too. A 401 never authenticated, so it has no team to
//     attribute a row to and is skipped.
//  8. Auth       — mounted only on the API route group below, not on the
//     probes: a load balancer's health check carries no API key, and
//     Auth would 401 every one of them if it wrapped the whole router.
//     Auth's own latency still lands inside Timing's overhead number,
//     since Timing wraps Logger which wraps Auth.
//  9. RateLimit   — inside Auth, since it needs the *auth.Team Auth attaches
//     to the context. This only enforces RPM; TPM needs the decoded
//     body, which nothing before the handler has parsed, so it is
//     checked inside ChatCompletions instead (see reserveTokens in
//     handler.go).
//
// Step 4.2's budget check is not a middleware layer either, for the same
// reason TPM isn't: it needs the decoded body to estimate a cost, so it runs
// inside ChatCompletions too (see reserveBudget in handler.go), right after
// the TPM reservation succeeds.
// chaos is the Step 7.5 harness, and may be nil. Note that it is injected on
// the request path here but exposed nowhere on this router: its controls are
// mounted on the admin listener only, so nothing reachable from the public
// port can turn fault injection on.
func NewRouter(resolver Resolver, authr Authenticator, limiter RateLimiter, budgetTracker BudgetTracker, calc CostCalculator, healthRecorder HealthRecorder, healthOracle HealthOracle, breakers Breakers, chaos *Chaos, retryConfig resilience.Config, promMetrics *telemetry.Metrics, reqLog RequestLogger, log *slog.Logger, ready func() bool) http.Handler {
	h := NewHandler(resolver, limiter, budgetTracker, calc, healthRecorder, healthOracle, breakers, chaos, retryConfig, promMetrics, log)

	r := chi.NewRouter()

	r.Use(Recoverer(log))
	r.Use(RequestID)
	r.Use(Timing)
	r.Use(Tracing)
	r.Use(Logger(log))

	r.Group(func(r chi.Router) {
		r.Use(Metrics(promMetrics))
		r.Use(RequestLog(reqLog))
		r.Use(Auth(authr, log))
		r.Use(RateLimit(limiter, promMetrics, log))
		r.Post("/v1/chat/completions", h.ChatCompletions)
	})

	// Probes live on the public listener because that is what a load balancer
	// in front of the gateway can reach. They are also mounted on the admin
	// listener for operators. Neither goes through Auth: a health check has no
	// team to authenticate as.
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

// Readyz reports whether the gateway can serve traffic. ready is composed in
// cmd/gateway from two things as of Step 5.4: whether config loaded and the
// registry was built (Phase 1), and whether at least one provider is not
// Down (health.Monitor.AllDown). Readyz itself stays a plain bool check —
// the OR of those two conditions lives where they're both in scope, not
// here — so only *every* provider being down makes the gateway unready. One
// bad provider is a routing problem for Phase 6's fallback chains, not a
// reason to be pulled from a load balancer.
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
