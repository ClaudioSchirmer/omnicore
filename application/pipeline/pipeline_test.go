package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// silentPipeline builds a Pipeline whose logger swallows everything — keeps
// the test output clean while still exercising the slog code paths.
func silentPipeline(t *testing.T) *Pipeline {
	t.Helper()
	p := New(nil) // uses translation.Default()
	return p.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestNew_NilTranslatorFallsBackToDefault(t *testing.T) {
	p := New(nil)
	if p == nil {
		t.Fatal("expected non-nil Pipeline")
	}
	if p.Translator() == nil {
		t.Error("expected Translator() to fall back to translation.Default(), got nil")
	}
}

func TestNew_NonNilTranslatorIsKept(t *testing.T) {
	tr := translation.New()
	p := New(tr)
	if p.Translator() != tr {
		t.Error("expected Translator() to return the injected instance")
	}
}

func TestPipeline_WithLogger_NilNoOp(t *testing.T) {
	p := New(nil)
	before := p.logger
	p.WithLogger(nil)
	if p.logger != before {
		t.Error("WithLogger(nil) must NOT replace the existing logger")
	}
}

func TestPipeline_WithLogger_SetsLogger(t *testing.T) {
	p := New(nil)
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := p.WithLogger(custom); got != p {
		t.Error("WithLogger should return the same Pipeline (chainable)")
	}
	if p.logger != custom {
		t.Error("WithLogger did not install the supplied logger")
	}
}

func TestRun_NilCtx_ReturnsContextNotInitializedFailure(t *testing.T) {
	p := silentPipeline(t)
	result := Run(p, nil, func() (int, error) { return 7, nil })

	if !result.IsFailure() {
		t.Fatalf("expected Failure, got state=%v", result.State())
	}
	if len(result.Notifications()) != 1 {
		t.Fatalf("expected 1 context DTO, got %d", len(result.Notifications()))
	}
	dto := result.Notifications()[0]
	if dto.Context != "Pipeline" {
		t.Errorf("expected context name 'Pipeline', got %q", dto.Context)
	}
}

func TestRun_HandlerReturnsValue(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	r := Run(p, ctx, func() (string, error) { return "hello", nil })

	if !r.IsSuccess() {
		t.Fatalf("expected Success, got state=%v", r.State())
	}
	if r.Value() != "hello" {
		t.Errorf("Value() = %q, want %q", r.Value(), "hello")
	}
}

func TestRun_HandlerReturnsCarrier_IsFailure(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	carrier := domain.NewDomainError([]*domain.NotificationContext{
		domain.NewNotificationContext("User"),
	})
	r := Run(p, ctx, func() (int, error) { return 0, carrier })

	if !r.IsFailure() {
		t.Fatalf("expected Failure on carrier, got state=%v", r.State())
	}
	if len(r.Notifications()) != 1 {
		t.Errorf("expected 1 context DTO, got %d", len(r.Notifications()))
	}
}

func TestRun_HandlerReturnsRawError_IsException(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	plain := errors.New("downstream failure")
	r := Run(p, ctx, func() (int, error) { return 0, plain })

	if !r.IsException() {
		t.Fatalf("expected Exception on raw error, got state=%v", r.State())
	}
	if !errors.Is(r.Err(), plain) {
		t.Errorf("Err() = %v, want %v", r.Err(), plain)
	}
}

func TestRun_DeadlineExceeded_IsTimeoutFailure(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	cases := map[string]error{
		"direct":  context.DeadlineExceeded,
		"wrapped": fmt.Errorf("query aborted: %w", context.DeadlineExceeded),
	}
	for name, retErr := range cases {
		t.Run(name, func(t *testing.T) {
			r := Run(p, ctx, func() (int, error) { return 0, retErr })

			if !r.IsFailure() {
				t.Fatalf("expected Failure on deadline, got state=%v", r.State())
			}
			if len(r.Notifications()) != 1 {
				t.Fatalf("expected 1 context DTO, got %d", len(r.Notifications()))
			}
			dto := r.Notifications()[0]
			if dto.Context != "Request" {
				t.Errorf("context = %q, want %q", dto.Context, "Request")
			}
			if len(dto.Messages) != 1 {
				t.Fatalf("expected 1 message, got %d", len(dto.Messages))
			}
			msg := dto.Messages[0]
			if msg.NotificationKey != "RequestTimeoutNotification" {
				t.Errorf("NotificationKey = %q, want RequestTimeoutNotification", msg.NotificationKey)
			}
			if msg.Semantic != domain.SemanticGatewayTimeout {
				t.Errorf("Semantic = %v, want SemanticGatewayTimeout", msg.Semantic)
			}
		})
	}
}

func TestRun_PanicRecovered_IsException(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	r := Run(p, ctx, func() (int, error) { panic("boom") })

	if !r.IsException() {
		t.Fatalf("expected Exception on panic, got state=%v", r.State())
	}
	want := "panic recovered: boom"
	if got := fmt.Sprintf("%v", r.Err()); got != want {
		t.Errorf("Err() = %q, want %q", got, want)
	}
}

// --- Dispatch / DispatchAll exercise the Handler[TReq,TRes] indirection ---

type echoCommand struct {
	CommandBase
	Msg string
}

type echoHandler struct{}

func (echoHandler) Handle(_ *configuration.AppContext, c echoCommand) (string, error) {
	return c.Msg, nil
}

type failingHandler struct {
	err error
}

func (h failingHandler) Handle(_ *configuration.AppContext, _ echoCommand) (string, error) {
	return "", h.err
}

func TestDispatch_HappyPath(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	r := Dispatch[echoCommand, string](p, ctx, echoCommand{Msg: "hi"}, echoHandler{})
	if !r.IsSuccess() || r.Value() != "hi" {
		t.Errorf("Dispatch happy path failed: state=%v value=%q", r.State(), r.Value())
	}
}

func TestDispatch_HandlerErrorBecomesException(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	plain := errors.New("nope")
	r := Dispatch[echoCommand, string](p, ctx, echoCommand{}, failingHandler{err: plain})
	if !r.IsException() {
		t.Fatalf("expected Exception, got state=%v", r.State())
	}
}

func TestDispatchAll_RunsAllHandlersInOrder(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	results := DispatchAll[echoCommand, string](p, ctx, echoCommand{Msg: "a"},
		[]Handler[echoCommand, string]{
			echoHandler{},
			failingHandler{err: errors.New("boom")},
			echoHandler{},
		})

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if !results[0].IsSuccess() {
		t.Errorf("result[0] should be Success, got %v", results[0].State())
	}
	if !results[1].IsException() {
		t.Errorf("result[1] should be Exception, got %v", results[1].State())
	}
	if !results[2].IsSuccess() {
		t.Errorf("result[2] should be Success, got %v", results[2].State())
	}
}

func TestDispatchAll_EmptyHandlerListReturnsEmptySlice(t *testing.T) {
	p := silentPipeline(t)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	results := DispatchAll[echoCommand, string](p, ctx, echoCommand{}, nil)
	if len(results) != 0 {
		t.Errorf("expected empty results, got %d", len(results))
	}
}
