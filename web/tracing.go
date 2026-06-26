package web

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// serverTracer names the web-layer instrumentation scope. The OTel trace API is
// imported here as a third-party dependency (same class as fiber/uuid), NOT the
// SDK and NOT infra — so no web→infra edge is created. No-op + free when no
// provider is installed (tracing disabled).
var serverTracer = otel.Tracer("github.com/ClaudioSchirmer/omnicore/web")

// fiberHeaderCarrier adapts inbound fiber request headers to the OTel
// TextMapCarrier interface so the configured propagator can extract a W3C
// traceparent. Only Get is exercised by the TraceContext propagator on extract;
// Set/Keys round out the interface.
type fiberHeaderCarrier struct{ c fiber.Ctx }

func (f fiberHeaderCarrier) Get(key string) string { return f.c.Get(key) }
func (f fiberHeaderCarrier) Set(key, val string)   { f.c.Set(key, val) }
func (f fiberHeaderCarrier) Keys() []string        { return nil }

// startServerSpan extracts any inbound trace context from the request headers
// and starts the server span as its child (or a root span when no traceparent
// arrives). The returned context carries the span; the caller ends it.
func startServerSpan(c fiber.Ctx) (context.Context, trace.Span) {
	parent := otel.GetTextMapPropagator().Extract(c.Context(), fiberHeaderCarrier{c: c})
	return serverTracer.Start(parent, c.Method()+" "+c.Path(),
		trace.WithSpanKind(trace.SpanKindServer))
}

// uuidFromTraceID reinterprets an OTel TraceID (16 bytes) as a uuid.UUID. It is
// the byte-for-byte twin of infra/tracing.UUIDFromTraceID, inlined here because
// web must not import infra; the trivial reinterpretation is stable. Used to
// keep AppContext.CorrelationID() equal to the active trace_id.
func uuidFromTraceID(t trace.TraceID) uuid.UUID {
	var id uuid.UUID
	copy(id[:], t[:])
	return id
}
