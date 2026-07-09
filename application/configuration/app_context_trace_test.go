package configuration

import (
	"context"
	"testing"
)

// Parent() exposes the cancellation/value parent for the tracing layer —
// context.Background() when unset, the injected ctx verbatim when set.

func TestAppContext_Parent_DefaultsToBackground(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	if got := ctx.Parent(); got != context.Background() {
		t.Errorf("expected Parent() to fall back to context.Background(), got %v", got)
	}
}

func TestAppContext_Parent_ReturnsInjectedParent(t *testing.T) {
	type ctxKey struct{}
	ctx := NewAppContextWithRandomID(LangPTBR)
	parent := context.WithValue(context.Background(), ctxKey{}, "request-ctx")
	ctx.SetParent(parent)

	got := ctx.Parent()
	if got != parent {
		t.Errorf("expected Parent() to return the injected parent verbatim, got %v", got)
	}
	if got.Value(ctxKey{}) != "request-ctx" {
		t.Errorf("expected the parent's values to be reachable via Parent()")
	}
}

// TraceContext() prefers the inbound-span ctx, falls back to the
// cancellation parent, then context.Background() — the pipeline span code
// always gets a non-nil base.

func TestAppContext_TraceContext_PrefersInboundSpanContext(t *testing.T) {
	type spanKey struct{}
	type parentKey struct{}
	ctx := NewAppContextWithRandomID(LangPTBR)
	ctx.SetParent(context.WithValue(context.Background(), parentKey{}, "parent"))
	traceCtx := context.WithValue(context.Background(), spanKey{}, "server-span")
	ctx.SetTraceContext(traceCtx)

	if got := ctx.TraceContext(); got != traceCtx {
		t.Errorf("expected TraceContext() to return the inbound-span ctx, got %v", got)
	}
}

func TestAppContext_TraceContext_FallsBackToParent(t *testing.T) {
	type parentKey struct{}
	ctx := NewAppContextWithRandomID(LangPTBR)
	parent := context.WithValue(context.Background(), parentKey{}, "parent")
	ctx.SetParent(parent)

	if got := ctx.TraceContext(); got != parent {
		t.Errorf("expected TraceContext() to fall back to the parent, got %v", got)
	}
}

func TestAppContext_TraceContext_BackgroundWhenNothingSet(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	if got := ctx.TraceContext(); got != context.Background() {
		t.Errorf("expected TraceContext() to fall back to context.Background(), got %v", got)
	}
}

func TestAppContext_SetTraceContext_NilClears(t *testing.T) {
	type spanKey struct{}
	ctx := NewAppContextWithRandomID(LangPTBR)
	ctx.SetTraceContext(context.WithValue(context.Background(), spanKey{}, "span"))
	ctx.SetTraceContext(nil) //nolint:staticcheck // SA1012: the nil fallback to Background() is the behavior under test.

	if got := ctx.TraceContext(); got != context.Background() {
		t.Errorf("expected nil SetTraceContext to restore the background fallback, got %v", got)
	}
}
