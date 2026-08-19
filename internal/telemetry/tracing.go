// Package telemetry owns OpenTelemetry setup and, from Phase 9 onward,
// Prometheus metric definitions. Per CLAUDE.md's package table it is
// imported everywhere and depends on nothing internal -- this file holds
// only stdlib and OTel SDK imports, so that stays true.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// tracerName identifies this instrumentation to whatever TracerProvider is
// active. It is the OTel convention to use the instrumenting module's import
// path here, not a display name.
const tracerName = "github.com/Harshalsharma05/switchyard"

// Tracer returns SwitchYard's named tracer. Every package that starts a
// Step 8.2 span calls this rather than otel.Tracer(tracerName) directly, so
// the instrumentation name exists in exactly one place.
//
// This is safe to call before Setup runs, and in tests that never call Setup
// at all: otel.Tracer looks up the *current* global TracerProvider at each
// call, and the untouched global default is a working no-op provider — so a
// span created without Setup having run costs a few no-op method calls, not
// a nil pointer.
func Tracer() oteltrace.Tracer {
	return otel.Tracer(tracerName)
}

func Propagator() propagation.TextMapPropagator {
	return otel.GetTextMapPropagator()
}

// Config configures Setup. Every field describes how this deployment wants
// to trace, not something that differs per provider or team, so
// cmd/gateway reads it from environment variables -- the same category as
// SWITCHYARD_DRAIN_TIMEOUT and Phase 7's breaker tuning.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string

	// OTLPEndpoint is host:port only, no scheme -- otlptracehttp appends
	// /v1/traces itself. "localhost:4318" reaches Jaeger's OTLP/HTTP
	// receiver.
	OTLPEndpoint string

	// SampleRatio is the fraction of root traces kept, in [0,1]. The plan
	// asks for 100% in dev and a configurable ratio for "production"; 1.0
	// is cmd/gateway's default, and an operator turns it down with
	// SWITCHYARD_TRACE_SAMPLE_RATIO. It only decides traces with no inbound
	// parent -- see newSampler for why a propagated traceparent overrides it.
	SampleRatio float64
}

// Setup builds the global TracerProvider and W3C propagator and returns a
// shutdown func that flushes and closes the exporter.
//
// It never blocks on reaching the collector: otlptracehttp.New does not
// dial eagerly, and the batch span processor it's wrapped in exports
// asynchronously, off the request path. A dead or unreachable Jaeger
// therefore costs nothing at request time -- this is what makes CLAUDE.md's
// "telemetry down must never block a request" true by construction rather
// than by a timeout somewhere on the hot path. A failed export is dropped
// after the exporter's own retry budget and only ever surfaces through the
// error handler below, never as an error returned to a caller.
func Setup(ctx context.Context, cfg Config, log *slog.Logger) (func(context.Context) error, error) {
	// The default error handler writes to stderr outside slog's JSON
	// format, which would break Phase 1's "logs are JSON, one line per
	// request." Routing it through the same logger keeps every export
	// failure in the structured stream an operator already greps -- and,
	// per the doc comment above, this is the *only* place an export
	// failure is ever observable.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("telemetry error", slog.Any("error", err))
	}))

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.OTLPEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("building OTLP/HTTP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("building telemetry resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(newSampler(cfg.SampleRatio)),
	)

	otel.SetTracerProvider(tp)

	// TraceContext (W3C traceparent) plus Baggage, registered globally now
	// even though Step 8.4's middleware -- the thing that actually reads an
	// inbound traceparent and injects one into provider calls -- comes
	// later. Same "declare the shape now, wire it up when its phase
	// arrives" pattern Provider.Ping got in Phase 1: whoever writes 8.4
	// finds a propagator already configured via otel.GetTextMapPropagator,
	// not a decision still open.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// newSampler wraps a ratio sampler in ParentBased so an inbound sampled
// trace -- a W3C traceparent whose sampled flag is already set -- always
// continues sampled, regardless of ratio: a caller who decided to trace a
// request should never have SwitchYard silently drop the middle of it. The
// ratio governs only root traces, the ones with no inbound parent, which is
// what makes it the right knob for "sample everything in dev, a slice in
// production."
func newSampler(ratio float64) sdktrace.Sampler {
	switch {
	case ratio < 0:
		ratio = 0
	case ratio > 1:
		ratio = 1
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}
