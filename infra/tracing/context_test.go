package tracing

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// sampledContext builds a context carrying a valid, sampled remote span context.
func sampledContext(t *testing.T) (context.Context, trace.TraceID) {
	t.Helper()
	tid := TraceIDFromUUID(uuid.New())
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), tid
}

func TestTraceparentInjectExtractRoundTrip(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	ctx, tid := sampledContext(t)
	tp := TraceparentFromContext(ctx)
	if !strings.HasPrefix(tp, "00-"+tid.String()) {
		t.Fatalf("traceparent %q does not carry trace id %s", tp, tid)
	}

	back := ContextFromTraceparent(context.Background(), tp)
	sc := trace.SpanContextFromContext(back)
	if !sc.IsValid() || sc.TraceID() != tid {
		t.Fatalf("extract lost trace id: valid=%v id=%s", sc.IsValid(), sc.TraceID())
	}

	// Empty traceparent returns the context unchanged.
	if ContextFromTraceparent(ctx, "") != ctx {
		t.Fatal("empty traceparent must return ctx unchanged")
	}
}

func TestTraceparentFromContextNoSpan(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if got := TraceparentFromContext(context.Background()); got != "" {
		t.Fatalf("no span should yield empty traceparent, got %q", got)
	}
}

func TestLinkFromTraceparent(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx, tid := sampledContext(t)
	tp := TraceparentFromContext(ctx)

	link, ok := LinkFromTraceparent(tp)
	if !ok || link.SpanContext.TraceID() != tid {
		t.Fatalf("valid traceparent should link to %s, ok=%v", tid, ok)
	}
	if _, ok := LinkFromTraceparent(""); ok {
		t.Fatal("empty traceparent must not link")
	}
	if _, ok := LinkFromTraceparent("not-a-traceparent"); ok {
		t.Fatal("garbage traceparent must not link")
	}
}

func TestTraceIDFromContext(t *testing.T) {
	ctx, tid := sampledContext(t)
	if got := TraceIDFromContext(ctx); got != tid.String() {
		t.Fatalf("trace id = %q want %s", got, tid)
	}
	if got := TraceIDFromContext(context.Background()); got != "" {
		t.Fatalf("no span should yield empty trace id, got %q", got)
	}
}

func TestStartConsumerSpanWithProvider(t *testing.T) {
	p, err := Setup(context.Background(), enabled(func(c *Config) {
		c.Exporter = ExporterNone
		c.Sampler = SamplerAlwaysOn
	}))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer p.Shutdown(context.Background())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	parent, tid := sampledContext(t)
	tp := TraceparentFromContext(parent)

	ctx, span := StartConsumerSpan(context.Background(),
		"github.com/ClaudioSchirmer/omnicore/test", "consume thing", tp)
	defer span.End()

	if !span.SpanContext().IsValid() {
		t.Fatal("consumer span should be valid under an installed provider")
	}
	// The span carries its own (new) trace; the producer trace is a LINK, so
	// the consumer's trace id differs from the producer's.
	if span.SpanContext().TraceID() == tid {
		t.Fatal("consumer span should start a new trace, linking (not parenting) the producer")
	}
	if sc := trace.SpanContextFromContext(ctx); !sc.IsValid() {
		t.Fatal("returned context should carry the span")
	}
}

// StartConsumerSpanIf gates the consumer span on the `kafka` instrument toggle.
func TestStartConsumerSpanIf(t *testing.T) {
	p, err := Setup(context.Background(), enabled(func(c *Config) {
		c.Exporter = ExporterNone
		c.Sampler = SamplerAlwaysOn
	}))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer p.Shutdown(context.Background())

	base := context.Background()
	// Off → ctx returned unchanged, span is a no-op (not recording), even with a
	// provider installed.
	gotCtx, span := StartConsumerSpanIf(false, base, "tracer", "consume", "")
	if gotCtx != base {
		t.Error("disabled toggle must return the context unchanged")
	}
	if span.IsRecording() {
		t.Error("disabled toggle must yield a no-op (non-recording) span")
	}
	span.End()

	// On → delegates to StartConsumerSpan, producing a real recording span.
	_, on := StartConsumerSpanIf(true, base, "tracer", "consume", "")
	if !on.IsRecording() {
		t.Error("enabled toggle must yield a recording span under a provider")
	}
	on.End()
}
