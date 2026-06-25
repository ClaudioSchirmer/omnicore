package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type sampleReq struct{}

func TestDispatchName(t *testing.T) {
	cases := map[string]struct {
		req  any
		want string
	}{
		"pointer to named struct": {&sampleReq{}, "sampleReq"},
		"named struct value":      {sampleReq{}, "sampleReq"},
		"anonymous struct":        {struct{}{}, "request"},
		"nil":                     {nil, "request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := dispatchName(tc.req); got != tc.want {
				t.Errorf("dispatchName = %q, want %q", got, tc.want)
			}
		})
	}
}

var (
	sharedRecorder *tracetest.SpanRecorder
	tracerOnce     sync.Once
)

// installTestTracer installs a package-shared SDK provider (AlwaysSample) + the
// W3C propagator the FIRST time it runs, and returns a recorder reset for the
// calling test. A single install per binary is REQUIRED: the package-level
// dispatchTracer is a delegating tracer that locks onto the first provider set in
// the process, so per-test providers would silently route later tests' spans into
// the first test's recorder (a sibling test would then pass vacuously on an empty
// recorder). Resetting gives each (sequential) test a clean view.
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

func newTestAppContext() *configuration.AppContext {
	return configuration.NewAppContext(uuid.New(), configuration.LangENG)
}

func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// beginDispatchSpan must start "dispatch <Req>" as a child of the inbound server
// span carried on AppContext.TraceContext, thread it onto the cancellation parent
// so a downstream SpanFromContext(appCtx) resolves to it, and the finisher must
// restore the original parent so a reused AppContext starts siblings, not nests.
func TestBeginDispatchSpan_ChildOfServerSpanAndRestoresParent(t *testing.T) {
	sr := installTestTracer(t)

	serverCtx, serverSpan := otel.Tracer("test/server").Start(context.Background(), "server")
	ctx := newTestAppContext()
	orig := ctx.Parent()
	ctx.SetTraceContext(serverCtx)

	span, end := beginDispatchSpan(ctx, &sampleReq{})

	// While active, downstream infra (pgx/mongo/httpclient) starting from the
	// AppContext must attach to the dispatch span.
	if got := trace.SpanFromContext(ctx).SpanContext().SpanID(); got != span.SpanContext().SpanID() {
		t.Errorf("AppContext does not carry the dispatch span: got %s, want %s", got, span.SpanContext().SpanID())
	}

	end()
	serverSpan.End()

	if ctx.Parent() != orig {
		t.Errorf("finisher did not restore the original parent")
	}

	dispatch := findSpan(sr.Ended(), "dispatch sampleReq")
	if dispatch == nil {
		t.Fatal("dispatch span was not recorded")
	}
	if dispatch.Parent().SpanID() != serverSpan.SpanContext().SpanID() {
		t.Errorf("dispatch parent = %s, want server span %s", dispatch.Parent().SpanID(), serverSpan.SpanContext().SpanID())
	}
	if dispatch.SpanContext().TraceID() != serverSpan.SpanContext().TraceID() {
		t.Errorf("dispatch span is not in the server trace")
	}
	if dispatch.SpanKind() != trace.SpanKindInternal {
		t.Errorf("dispatch span kind = %v, want internal", dispatch.SpanKind())
	}
}

// A reused AppContext (the DispatchAll loop) must produce sibling dispatch spans
// under the same server span — never a chain where the second nests in the first.
func TestBeginDispatchSpan_ReusedContextStartsSiblings(t *testing.T) {
	sr := installTestTracer(t)

	serverCtx, serverSpan := otel.Tracer("test/server").Start(context.Background(), "server")
	ctx := newTestAppContext()
	ctx.SetTraceContext(serverCtx)

	s1, e1 := beginDispatchSpan(ctx, &sampleReq{})
	e1()
	s2, e2 := beginDispatchSpan(ctx, &sampleReq{})
	e2()
	serverSpan.End()

	if s1.SpanContext().SpanID() == s2.SpanContext().SpanID() {
		t.Fatal("expected two distinct dispatch spans")
	}
	serverID := serverSpan.SpanContext().SpanID()
	for _, s := range sr.Ended() {
		if s.Name() != "dispatch sampleReq" {
			continue
		}
		if s.Parent().SpanID() != serverID {
			t.Errorf("dispatch parent = %s, want server span %s (siblings, not nested)", s.Parent().SpanID(), serverID)
		}
	}
}

// recordDispatchOutcome marks the span errored ONLY on a non-nil error (the
// Exception/5xx class). A Failure carrying domain notifications surfaces err==nil
// and must leave the span status unset, so traces are not polluted with expected
// 4xx validation rejections.
func TestRecordDispatchOutcome(t *testing.T) {
	sr := installTestTracer(t)

	_, okSpan := otel.Tracer("test").Start(context.Background(), "ok")
	recordDispatchOutcome(okSpan, nil)
	okSpan.End()

	_, errSpan := otel.Tracer("test").Start(context.Background(), "boom")
	recordDispatchOutcome(errSpan, errors.New("kaboom"))
	errSpan.End()

	codesByName := map[string]codes.Code{}
	for _, s := range sr.Ended() {
		codesByName[s.Name()] = s.Status().Code
	}
	if codesByName["ok"] != codes.Unset {
		t.Errorf("nil-error span status = %v, want Unset (Failure/4xx must not error the span)", codesByName["ok"])
	}
	if codesByName["boom"] != codes.Error {
		t.Errorf("error span status = %v, want Error (Exception/5xx)", codesByName["boom"])
	}
}
