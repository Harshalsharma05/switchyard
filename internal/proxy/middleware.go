package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// Response headers SwitchYard adds to every reply. They exist so a caller can
// tell what actually served a request without reading the gateway's logs —
// which matters most once Phase 6 can serve a request from a provider the caller
// did not ask for.
const (
	// HeaderRequestID carries the request's correlation ID.
	HeaderRequestID = "X-Switchyard-Request-Id"
	// HeaderProvider names the provider instance that served the request.
	HeaderProvider = "X-Switchyard-Provider"
	// HeaderRequestedModel names the model the caller asked for.
	HeaderRequestedModel = "X-Switchyard-Requested-Model"
	// HeaderServedModel names the model actually served, which differs from
	// the requested one whenever Step 6.2's fallback chain moved the request.
	HeaderServedModel = "X-Switchyard-Served-Model"
	// HeaderFallback reports whether the request was served by something
	// other than what the caller asked for. It is present on every response
	// that reached a provider, "false" included: a caller scripting against
	// it should not have to treat an absent header as a negative.
	HeaderFallback = "X-Switchyard-Fallback"
	// HeaderOverhead is the gateway's own latency in milliseconds: everything
	// the request spent inside SwitchYard, excluding the provider call.
	HeaderOverhead = "X-Switchyard-Overhead-Ms"
)

// maxRequestIDLen bounds an inbound X-Request-ID.
const maxRequestIDLen = 64

// ctxKey is a private type so no other package can collide with these context
// keys — the reason context keys are never plain strings.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	metricsKey
	teamKey
)

// RequestIDFrom returns the correlation ID attached to a request's context, or
// the empty string if the middleware did not run.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestMetrics accumulates what the response headers and the log line report.
//
// It carries no lock. Every field is written by the handler and read by the
// writer wrapper, both on the goroutine net/http dedicates to this one request,
// so there is no concurrent access to protect against. Phase 8 adds span
// attributes from the same struct on the same goroutine.
type requestMetrics struct {
	start time.Time

	// providerName and servedModel stay empty when no provider was reached —
	// an unknown model or a request rejected during validation.
	providerName string
	servedModel  string

	// requestedModel is what the caller asked for. It is set as soon as the
	// body decodes, so it survives a request that never reached a provider,
	// unlike servedModel above.
	requestedModel string

	// fellBack is true when servedModel came from somewhere other than the
	// head of Step 6.2's chain — that is, when the request was served by a
	// provider or model the caller did not ask for.
	fellBack bool

	// providerTime is what the request spent waiting on the upstream. Gateway
	// overhead is everything else.
	providerTime time.Duration

	// usage is the token accounting for the request, zero-valued when no
	// provider was reached. Both the streaming and non-streaming paths set it,
	// so Logger reports token counts identically for either — this is how Step
	// 2.3's "token counts logged correctly for streaming requests" is met,
	// rather than a bespoke log line living only in the streaming handler.
	usage provider.Usage

	// costMicros is the request's price in integer micro-dollars, set by
	// handler.go's recordCost once usage is final. It stays zero whenever
	// usage does — no provider reached, or a mid-stream failure that never
	// received a terminal usage-bearing chunk (see streamChatCompletions).
	costMicros int64
}

// metricsFrom returns the per-request metrics, or nil if Timing did not run.
func metricsFrom(ctx context.Context) *requestMetrics {
	m, _ := ctx.Value(metricsKey).(*requestMetrics)
	return m
}

// overhead is the time the request spent inside SwitchYard rather than waiting
// on a provider. It is the project's headline number.
func (m *requestMetrics) overhead() time.Duration {
	d := time.Since(m.start) - m.providerTime
	if d < 0 {
		// Cannot happen with a monotonic clock, but a negative headline number
		// would be worse than a zero.
		return 0
	}
	return d
}

// applyHeaders writes the SwitchYard headers. It must run before the status
// line goes out, which is what the writer wrapper below guarantees.
func (m *requestMetrics) applyHeaders(h http.Header) {
	if m.providerName != "" {
		h.Set(HeaderProvider, m.providerName)
	}
	if m.requestedModel != "" {
		h.Set(HeaderRequestedModel, m.requestedModel)
	}
	if m.servedModel != "" {
		h.Set(HeaderServedModel, m.servedModel)
		// Only alongside a served model: a request that never reached a
		// provider has no fallback outcome to report, and "false" there would
		// claim more than the gateway knows.
		h.Set(HeaderFallback, strconv.FormatBool(m.fellBack))
	}
	h.Set(HeaderOverhead, formatMillis(m.overhead()))
}

// formatMillis renders a duration as fractional milliseconds.
//
// Three decimal places is microsecond resolution, and that precision is the
// point: Phase 1 overhead should sit well under one millisecond, and an integer
// format would report every request as "0" — useless for the one number this
// project is built to defend.
func formatMillis(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
}

// Timing measures gateway overhead and emits the SwitchYard response headers.
//
// Headers have to be set before the status line is written, but the numbers are
// not known until the handler has done its work. The wrapper below resolves that
// by injecting them at the moment of the first write, rather than requiring
// every handler to remember to do it.
func Timing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := &requestMetrics{start: time.Now()}
		hooked := &headerHook{ResponseWriter: w, metrics: m}

		next.ServeHTTP(hooked, r.WithContext(context.WithValue(r.Context(), metricsKey, m)))

		// A handler that returned without writing still gets headers: net/http
		// writes the status only after ServeHTTP returns.
		hooked.applyOnce()
	})
}

// headerHook injects the timing headers just before the first byte leaves.
//
// For a streaming response in Phase 2 this lands on the first chunk, which makes
// the reported overhead a time-to-first-token measurement — the right boundary
// for a stream, where total duration is dominated by the model generating.
type headerHook struct {
	http.ResponseWriter
	metrics *requestMetrics
	applied bool
}

func (w *headerHook) applyOnce() {
	if w.applied {
		return
	}
	w.applied = true
	w.metrics.applyHeaders(w.ResponseWriter.Header())
}

func (w *headerHook) WriteHeader(status int) {
	w.applyOnce()
	w.ResponseWriter.WriteHeader(status)
}

func (w *headerHook) Write(b []byte) (int, error) {
	w.applyOnce()
	return w.ResponseWriter.Write(b)
}

// Flush keeps http.Flusher reachable, for the same reason recorder does.
func (w *headerHook) Flush() {
	w.applyOnce()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *headerHook) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RequestID attaches a correlation ID to every request, honouring an inbound
// X-Request-ID so a caller's ID survives into SwitchYard's logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = newRequestID()
		}

		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// sanitizeRequestID accepts an inbound ID only if it is short and consists of
// characters that cannot corrupt a log line.
//
// This is not cosmetic. The ID is written into structured logs, and a value
// containing newlines or control characters would let a caller forge log
// entries. Rejecting rather than escaping keeps the rule simple: anything odd
// gets a fresh generated ID.
func sanitizeRequestID(id string) string {
	if id == "" || len(id) > maxRequestIDLen {
		return ""
	}

	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
		default:
			return ""
		}
	}
	return id
}

// newRequestID returns a random 128-bit hex ID.
//
// crypto/rand is used rather than math/rand because IDs appear in logs a
// customer may see, and predictable identifiers invite guessing at adjacent
// requests. crypto/rand.Read never returns an error in current Go, so there is
// no fallback path to get wrong.
func newRequestID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// recorder wraps a ResponseWriter to capture what was sent.
//
// The Flush and Unwrap methods are the load-bearing part. Wrapping a
// ResponseWriter hides the interfaces the original satisfied, and net/http
// checks for http.Flusher by type assertion — so a wrapper without Flush
// silently disables streaming for every handler behind it. Phase 2 depends on
// this staying correct.
type recorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (w *recorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		// net/http infers 200 on a bare Write; record the same thing.
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += n
	return n, err
}

// Flush keeps http.Flusher reachable through the wrapper.
func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer, which is how
// deadline control and Flush are meant to be obtained from Go 1.20 onward.
func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Logger emits one structured line per request.
//
// The line is written after the handler returns so it can carry the status and
// duration. Logging is never allowed to affect the response.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}

			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", r.RemoteAddr),
			}

			// The same numbers the caller gets in headers also go to the log, so
			// overhead can be aggregated from logs before Phase 9's metrics exist.
			if m := metricsFrom(r.Context()); m != nil {
				attrs = append(attrs,
					slog.Duration("provider_duration", m.providerTime),
					slog.String("overhead_ms", formatMillis(m.overhead())),
				)
				if m.providerName != "" {
					attrs = append(attrs,
						slog.String("provider", m.providerName),
						slog.String("served_model", m.servedModel),
					)
				}
				if m.usage.InputTokens != 0 || m.usage.OutputTokens != 0 {
					attrs = append(attrs,
						slog.Int("input_tokens", m.usage.InputTokens),
						slog.Int("output_tokens", m.usage.OutputTokens),
						slog.Int64("cost_micros", m.costMicros),
					)
				}
			}

			log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
		})
	}
}

// Recoverer converts a panic in any downstream handler into a 500 rather than
// letting it kill the process.
//
// This is the one place a panic is caught rather than avoided: the gateway must
// never be the reason a request fails, and that includes never being the reason
// every *other* in-flight request fails because the process died.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is net/http's own signal to drop a
				// connection silently. Swallowing it would break that contract.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				log.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)

				writeError(w, log, http.StatusInternalServerError, "internal_error",
					"the gateway encountered an unexpected error")
			}()

			next.ServeHTTP(w, r)
		})
	}
}
