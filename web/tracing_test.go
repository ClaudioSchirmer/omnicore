package web

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/gofiber/fiber/v3"
)

var (
	sharedRecorder *tracetest.SpanRecorder
	tracerOnce     sync.Once
)

// installTestTracer installs a package-shared SDK provider (AlwaysSample) + the
// W3C TraceContext propagator the FIRST time it runs, and returns a recorder
// reset for the calling test. A single install per test binary is REQUIRED, not
// a nicety: the package-level serverTracer is a delegating tracer that locks onto
// the first provider set in the process, so per-test providers would silently
// route every later test's spans into the first test's recorder. Resetting the
// shared recorder gives each (sequential) test a clean view.
func installTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	tracerOnce.Do(func() {
		sharedRecorder = tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sharedRecorder),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		)
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
	sharedRecorder.Reset()
	return sharedRecorder
}

func TestUUIDFromTraceIDRoundTrip(t *testing.T) {
	id := uuid.New()
	var tid trace.TraceID
	copy(tid[:], id[:])
	if got := uuidFromTraceID(tid); got != id {
		t.Fatalf("round trip: got %s want %s", got, id)
	}
}

// With the http instrument ON and a provider installed, the middleware starts a
// server span, continues an inbound traceparent, renames the span to the route
// template, and keeps CorrelationID == the active trace id.
func TestAppContextMiddleware_ServerSpanTracingOn(t *testing.T) {
	sr := installTestTracer(t)

	const inboundHex = "0af7651916cd43dd8448eb211c80319c"
	inbound, err := trace.TraceIDFromHex(inboundHex)
	if err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	app.Use(AppContextMiddleware(WithServerSpanTracing(true)))
	var gotCorr uuid.UUID
	app.Get("/users/:id", func(c fiber.Ctx) error {
		gotCorr = AppContext(c).CorrelationID()
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest("GET", "/users/42", nil)
	req.Header.Set("traceparent", "00-"+inboundHex+"-b7ad6b7169203331-01")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// CorrelationID mirrors the continued inbound trace id, byte-for-byte.
	if gotCorr != uuidFromTraceID(inbound) {
		t.Errorf("correlationID = %s, want bridge of %s", gotCorr, inbound)
	}

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("want 1 recorded span, got %d", len(ended))
	}
	span := ended[0]
	if span.SpanContext().TraceID() != inbound {
		t.Errorf("server span trace id = %s, want continued %s", span.SpanContext().TraceID(), inbound)
	}
	if span.Name() != "GET /users/:id" {
		t.Errorf("span name = %q, want route template", span.Name())
	}
	if span.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", span.SpanKind())
	}
}

// With the http instrument OFF (no option) the middleware starts NO server span
// even when a provider is installed — the gate works — and leaves CorrelationID
// at its zero value (no trace bridge).
func TestAppContextMiddleware_ServerSpanTracingOff(t *testing.T) {
	sr := installTestTracer(t)

	app := fiber.New()
	app.Use(AppContextMiddleware()) // no WithServerSpanTracing → http off
	var gotCorr uuid.UUID
	app.Get("/x", func(c fiber.Ctx) error {
		gotCorr = AppContext(c).CorrelationID()
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/x", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if n := len(sr.Ended()); n != 0 {
		t.Fatalf("http instrument off must record no server span, got %d", n)
	}
	if gotCorr != uuid.Nil {
		t.Errorf("correlationID = %s, want zero (no bridge when http off)", gotCorr)
	}
}

// A handler-returned error must mark the ROOT server span errored, so a 5xx is
// visible at the trace root, not only on the child dispatch span.
func TestAppContextMiddleware_ServerSpanRecordsError(t *testing.T) {
	sr := installTestTracer(t)

	app := fiber.New()
	app.Use(AppContextMiddleware(WithServerSpanTracing(true)))
	app.Get("/boom", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusInternalServerError, "boom")
	})

	if _, err := app.Test(httptest.NewRequest("GET", "/boom", nil)); err != nil {
		t.Fatal(err)
	}

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("want 1 recorded span, got %d", len(ended))
	}
	if got := ended[0].Status().Code; got != codes.Error {
		t.Errorf("server span status = %v, want Error on a 5xx outcome", got)
	}
}

// fiberHeaderCarrier round-trips header access against a live fiber.Ctx.
func TestFiberHeaderCarrier(t *testing.T) {
	app := fiber.New()
	app.Get("/c", func(c fiber.Ctx) error {
		car := fiberHeaderCarrier{c: c}
		if got := car.Get("X-Probe"); got != "in" {
			t.Errorf("Get = %q, want in", got)
		}
		car.Set("X-Echo", "out")
		if car.Keys() != nil {
			t.Error("Keys is intentionally nil for extract-only use")
		}
		return c.SendStatus(fiber.StatusOK)
	})
	req := httptest.NewRequest("GET", "/c", nil)
	req.Header.Set("X-Probe", "in")
	if _, err := app.Test(req); err != nil {
		t.Fatal(err)
	}
}
