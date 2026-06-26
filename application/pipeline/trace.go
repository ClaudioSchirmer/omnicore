package pipeline

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// dispatchTracer names the application-layer instrumentation scope. The trace
// API is imported here (not the SDK) — the observability equivalent of slog,
// interface-only and a no-op when no provider is installed, so a service with
// tracing off pays essentially nothing. This is the single business
// unit-of-work span; every command/query, Auto or manual, REST or GraphQL,
// funnels through Dispatch, so all inherit it identically (no per-handler code).
var dispatchTracer = otel.Tracer("github.com/ClaudioSchirmer/omnicore/application/pipeline")

// beginDispatchSpan starts the "dispatch <Req>" span as a child of the span
// already in the request context (the inbound server span), threads its context
// onto the AppContext so downstream infra spans (pgx/mongo/httpclient) attach to
// it, and returns the span plus a finisher.
//
// The span is started from ctx.Parent() — the fiber request context — NOT from
// the AppContext, because setting a span context derived from the AppContext
// back as its own parent would create a Value/Deadline delegation cycle. The
// finisher restores the original parent (so a reused AppContext, e.g. in
// DispatchAll, starts each span as a sibling, never nested) and ends the span.
func beginDispatchSpan(ctx *configuration.AppContext, req any) (trace.Span, func()) {
	base := ctx.TraceContext() // inbound server span context (or background)
	orig := ctx.Parent()
	spanCtx, span := dispatchTracer.Start(base, "dispatch "+dispatchName(req),
		trace.WithSpanKind(trace.SpanKindInternal))
	// Thread the dispatch span onto the cancellation parent so downstream infra
	// spans (pgx/mongo/httpclient), which start from the AppContext handed to
	// the handler, attach to it. Restored on finish so a reused AppContext
	// (DispatchAll) starts each span as a sibling, not nested.
	ctx.SetParent(spanCtx)
	return span, func() {
		span.End()
		ctx.SetParent(orig)
	}
}

// recordDispatchOutcome marks the span errored only on an Exception (a recovered
// panic or a non-notification error — the 5xx class). A Failure carrying domain
// notifications is a normal business outcome (4xx) and leaves the span status
// unset, so traces are not polluted with expected validation rejections.
func recordDispatchOutcome(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// dispatchName renders a stable, dependency-light span suffix from the request
// type: "*commands.InsertUserCommand" → "InsertUserCommand".
func dispatchName(req any) string {
	t := reflect.TypeOf(req)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Name() == "" {
		return "request"
	}
	return t.Name()
}
