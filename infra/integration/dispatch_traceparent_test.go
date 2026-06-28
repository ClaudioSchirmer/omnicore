package integration

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

// writeIntegrationEventRow binds nullableString(tracing.TraceparentFromContext(ctx))
// as the 11th INSERT arg ($11) of sqlInsertIntegrationEvent — the W3C traceparent
// the Receiver links the consumed event back to. This asserts that producer-side
// stamping directly (the row builder uses the neutral db.Tx seam via UnwrapTx, not
// driver-specific handles): a non-NULL string when a span is active, NULL when tracing is off.
func TestIntegrationEventTraceparentBinding(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	spanCtx, span := otel.Tracer("test/producer").Start(context.Background(), "producer")
	defer span.End()

	if got := nullableString(tracing.TraceparentFromContext(spanCtx)); got == nil {
		t.Error("active span must bind a non-nil traceparent arg ($11)")
	} else if s, ok := got.(string); !ok || s == "" {
		t.Errorf("traceparent arg = %v, want a non-empty W3C string", got)
	}

	if got := nullableString(tracing.TraceparentFromContext(context.Background())); got != nil {
		t.Errorf("tracing off must bind nil (NULL), got %v", got)
	}
}
