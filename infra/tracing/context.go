package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TraceparentFromContext renders the active span's W3C traceparent so producers
// can persist it on a carrier row (outbox, integration_events) and it can travel
// through Debezium/Kafka to the consumer, which re-links the trace across the
// async gap. Returns "" when tracing is off (no-op propagator) or no span is
// active — the carrier column then stores NULL.
//
// Lives in this leaf package (imports only OpenTelemetry) so infra, infra/audit,
// and infra/integration can all share it without an import cycle — infra imports
// infra/audit, so the helper could not live in infra itself.
func TraceparentFromContext(ctx context.Context) string {
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c["traceparent"]
}

// ContextFromTraceparent rebuilds a context carrying the remote span context
// from a received traceparent, so a consumer span can LINK back to the producing
// trace (the OTel messaging convention — a link, not a parent/child edge, since
// producer and consumer are causally related but temporally far apart). Returns
// ctx unchanged when traceparent is empty.
func ContextFromTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx,
		propagation.MapCarrier{"traceparent": traceparent})
}

// LinkFromTraceparent returns a span Link to the trace carried by traceparent,
// for consumer spans that want the producer trace as a link rather than as the
// parent. The boolean is false when traceparent is empty or invalid.
func LinkFromTraceparent(traceparent string) (trace.Link, bool) {
	if traceparent == "" {
		return trace.Link{}, false
	}
	sc := trace.SpanContextFromContext(
		otel.GetTextMapPropagator().Extract(context.Background(),
			propagation.MapCarrier{"traceparent": traceparent}))
	if !sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc}, true
}

// StartConsumerSpanIf is StartConsumerSpan gated by the tracing `kafka`
// instrument toggle. When on is false it returns ctx unchanged plus a no-op
// span (so a deferred span.End() at the call site stays safe), and no consumer
// span is recorded even if a tracer provider is installed. bootstrap passes
// tracing.Instruments(SubKafka) via each engine's WithKafkaTracing.
func StartConsumerSpanIf(on bool, ctx context.Context, tracerName, spanName, traceparent string) (context.Context, trace.Span) {
	if !on {
		return ctx, noop.Span{}
	}
	return StartConsumerSpan(ctx, tracerName, spanName, traceparent)
}

// StartConsumerSpan opens a Kafka consumer span named under tracerName, linked
// (not parented) to the producing trace carried by traceparent — the messaging
// convention, since producer and consumer are temporally far apart. Returns a
// child context carrying the span so downstream pg/mongo spans attach to it. A
// no-op span when tracing is disabled.
func StartConsumerSpan(ctx context.Context, tracerName, spanName, traceparent string) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindConsumer)}
	if link, ok := LinkFromTraceparent(traceparent); ok {
		opts = append(opts, trace.WithLinks(link))
	}
	return otel.Tracer(tracerName).Start(ctx, spanName, opts...)
}

// TraceIDFromContext returns the active span's trace id as a 32-char hex string,
// or "" when no valid span is in the context. Used to stamp a pivot column
// (audit_events.trace_id) so a forensic row links to its trace in the collector.
func TraceIDFromContext(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}
