package infra

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

// installOutboxTestTracer swaps in a real SDK provider + the W3C TraceContext
// propagator so TraceparentFromContext renders a real header, and restores the
// globals on cleanup. Mirrors the harness the web/tracing tests use.
func installOutboxTestTracer(t *testing.T) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
}

// writeOutbox must stamp the producing request's W3C traceparent on the row so
// the async projection can link back to its trace. $5 is the traceparent slot.
func TestWriteOutbox_StampsTraceparentWhenSpanActive(t *testing.T) {
	installOutboxTestTracer(t)

	spanCtx, span := otel.Tracer("test/producer").Start(context.Background(), "producer")
	defer span.End()

	tx := newFakeTx()
	if err := writeOutbox(spanCtx, tx, "users", "INSERTED", "id-1", map[string]any{"x": 1}); err != nil {
		t.Fatalf("writeOutbox: %v", err)
	}
	if len(tx.execArgs) != 1 {
		t.Fatalf("want 1 Exec, got %d", len(tx.execArgs))
	}
	args := tx.execArgs[0]
	if len(args) != 5 {
		t.Fatalf("want 5 args, got %d", len(args))
	}
	tp, ok := args[4].(string)
	if !ok || tp == "" {
		t.Fatalf("traceparent arg ($5) = %v, want a non-empty W3C string", args[4])
	}
	if want := tracing.TraceparentFromContext(spanCtx); tp != want {
		t.Errorf("traceparent = %q, want %q", tp, want)
	}
}

// With tracing off (no active span) the slot stays nil so pgx stores NULL —
// never an empty string.
func TestWriteOutbox_NullTraceparentWhenTracingOff(t *testing.T) {
	tx := newFakeTx()
	if err := writeOutbox(context.Background(), tx, "users", "INSERTED", "id-1", nil); err != nil {
		t.Fatalf("writeOutbox: %v", err)
	}
	if len(tx.execArgs) != 1 {
		t.Fatalf("want 1 Exec, got %d", len(tx.execArgs))
	}
	if got := tx.execArgs[0][4]; got != nil {
		t.Errorf("traceparent arg ($5) = %v, want nil (NULL) when tracing off", got)
	}
}
