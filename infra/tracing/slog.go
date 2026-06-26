package tracing

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// slogHandler wraps a base slog.Handler and, for every record whose context
// carries a valid (sampled or not) span, adds traceId/spanId attributes. This
// is what stitches the stdout logs to the OTLP traces: read an error line, copy
// its traceId, open the whole request in the collector. The audit echo, the
// http.outbound record, and pipeline failures all flow through here unchanged
// otherwise — the wire only GAINS two fields, nothing is removed or reordered.
//
// Enrichment is context-driven: only records emitted via the *Context slog
// methods (InfoContext/WarnContext/…) with a span-carrying context are tagged;
// a plain logger.Info() with no context is passed through untouched. Call sites
// that want the id without a context keep adding it explicitly (the existing
// threadId field is unaffected).
type slogHandler struct {
	inner slog.Handler
}

// NewSlogHandler wraps base so trace ids are injected from the record context.
// bootstrap installs it around the JSON handler when tracing is enabled; when
// disabled the framework keeps the bare JSON handler and pays nothing.
func NewSlogHandler(base slog.Handler) slog.Handler {
	return &slogHandler{inner: base}
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("traceId", sc.TraceID().String()),
			slog.String("spanId", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &slogHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	return &slogHandler{inner: h.inner.WithGroup(name)}
}
