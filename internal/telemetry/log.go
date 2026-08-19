// Wraps a slog.Handler so every log record carries the current trace ID.
package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type tracingHandler struct {
	slog.Handler
}

func NewLogHandler(h slog.Handler) slog.Handler {
	return &tracingHandler{Handler: h}
}

func (h *tracingHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *tracingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &tracingHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *tracingHandler) WithGroup(name string) slog.Handler {
	return &tracingHandler{Handler: h.Handler.WithGroup(name)}
}
