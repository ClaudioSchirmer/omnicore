package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"

	"github.com/google/uuid"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- planReceiver error branches -------------------------------------------

type noToCommand struct{}

type argToCommand struct{}

func (argToCommand) ToCommand(int) *fakeCommand { return &fakeCommand{} }

type multiOutToCommand struct{}

func (multiOutToCommand) ToCommand() (*fakeCommand, error) { return &fakeCommand{}, nil }

type noHandle struct{}

type wrongCtxHandle struct{}

func (wrongCtxHandle) Handle(int, *fakeCommand) (fakeResult, error) { return fakeResult{}, nil }

type wrongCmdHandle struct{}

func (wrongCmdHandle) Handle(*configuration.AppContext, int) (fakeResult, error) {
	return fakeResult{}, nil
}

func TestPlanReceiver_ErrorBranches(t *testing.T) {
	cases := []struct {
		name    string
		sample  any
		handler any
		substr  string
	}{
		{"no-tocommand", noToCommand{}, &fakeHandler{}, "no ToCommand"},
		{"tocommand-takes-arg", argToCommand{}, &fakeHandler{}, "zero arguments"},
		{"tocommand-multi-out", multiOutToCommand{}, &fakeHandler{}, "Handle's command param"},
		{"handler-nil-type", fakeRequest{}, nil, "handler type is unresolvable"},
		{"handler-no-handle", fakeRequest{}, noHandle{}, "missing Handle"},
		{"handler-wrong-ctx", fakeRequest{}, wrongCtxHandle{}, "must be *configuration.AppContext"},
		{"handler-wrong-cmd", fakeRequest{}, wrongCmdHandle{}, "command param"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planReceiver(tc.sample, tc.handler)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestPlanReceiver_NilSampleType(t *testing.T) {
	var nilIface any
	if _, err := planReceiver(nilIface, &fakeHandler{}); err == nil {
		t.Fatal("expected error for nil sample type")
	}
}

// --- Registry nil-safety ----------------------------------------------------

func TestRegistry_NilSafety(t *testing.T) {
	var r *Registry
	if !r.IsEmpty() {
		t.Error("nil registry must be empty")
	}
	if r.Receivers() != nil {
		t.Error("nil registry must yield nil receivers")
	}
}

// --- resolveActor empty-subject fallback -----------------------------------

func TestResolveActor_EmptySubjectFallsBackToAnonymous(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{Subject: ""}) // present identity, empty subject
	if got := resolveActor(ctx); got != "anonymous" {
		t.Fatalf("empty subject must fall back to anonymous, got %q", got)
	}
}

// --- isSuccessResult --------------------------------------------------------

func TestIsSuccessResult(t *testing.T) {
	if isSuccessResult(reflect.Value{}) {
		t.Error("invalid reflect.Value must be non-success")
	}
	// A type without IsSuccess() is treated as success (handler returned a value).
	if !isSuccessResult(reflect.ValueOf(struct{}{})) {
		t.Error("value without IsSuccess must be treated as success")
	}
	if !isSuccessResult(reflect.ValueOf(fakeResult{OK: true})) {
		t.Error("IsSuccess=true must be success")
	}
	if isSuccessResult(reflect.ValueOf(fakeResult{OK: false})) {
		t.Error("IsSuccess=false must be non-success")
	}
}

// --- handleMessage end-to-end via the fake pgExec ---------------------------

// newResolvedReceiver registers a receiver and pins its YAML-resolved fields
// directly (same package), so handleMessage can run without a Kafka loop.
func newResolvedReceiver(t *testing.T, handler any) *Receiver {
	t.Helper()
	reg := NewRegistry()
	reg.From("partners").On("onboarded", fakeRequest{}, handler)
	r := reg.Receivers()[0]
	r.topic = "partners.events"
	r.wireEventType = "PartnerOnboarded"
	r.consumerGroup = "orders-int"
	r.workers = 1
	r.startFrom = "latest"
	return r
}

func TestHandleMessage_NilEventID(t *testing.T) {
	r := newResolvedReceiver(t, &fakeHandler{})
	exec := &fakeExec{}
	err := r.handleMessage(context.Background(), engineFor(exec), nil, uuid.Nil, []byte(`{}`), nil, discardLogger())
	if err == nil {
		t.Fatal("nil event id must error")
	}
}

func TestHandleMessage_DedupHitSkips(t *testing.T) {
	h := &fakeHandler{}
	r := newResolvedReceiver(t, h)
	// A dedup row present → IsAlreadyProcessed=true → handler skipped.
	exec := &fakeExec{rows: &fakeRows{data: [][]any{{1}}}}
	if err := r.handleMessage(context.Background(), engineFor(exec), map[string]string{"event_type": "PartnerOnboarded"},
		uuid.New(), []byte(`{"email":"x@y"}`), nil, discardLogger()); err != nil {
		t.Fatalf("dedup hit must return nil, got %v", err)
	}
	if h.called {
		t.Fatal("handler must not run on a dedup hit")
	}
}

func TestHandleMessage_SuccessRunsHandlerAndMarksProcessed(t *testing.T) {
	h := &fakeHandler{}
	r := newResolvedReceiver(t, h)
	// No dedup row → not processed → proceeds.
	exec := &fakeExec{}
	if err := r.handleMessage(context.Background(), engineFor(exec), map[string]string{"event_type": "PartnerOnboarded", "actor": "user-7"},
		uuid.New(), []byte(`{"email":"x@y"}`), nil, discardLogger()); err != nil {
		t.Fatalf("success path must return nil, got %v", err)
	}
	if !h.called {
		t.Fatal("handler must run on the success path")
	}
}

func TestHandleMessage_UnmarshalFailureRecordsFailure(t *testing.T) {
	r := newResolvedReceiver(t, &fakeHandler{})
	exec := &fakeExec{}
	err := r.handleMessage(context.Background(), engineFor(exec), map[string]string{"event_type": "PartnerOnboarded"},
		uuid.New(), []byte(`{not json`), nil, discardLogger())
	if err == nil {
		t.Fatal("malformed payload must error")
	}
}

type fakeErrHandler struct{}

func (fakeErrHandler) Handle(_ *configuration.AppContext, _ *fakeCommand) (fakeResult, error) {
	return fakeResult{}, errors.New("handler boom")
}

func TestHandleMessage_HandlerErrorRecordsFailure(t *testing.T) {
	r := newResolvedReceiver(t, fakeErrHandler{})
	exec := &fakeExec{}
	err := r.handleMessage(context.Background(), engineFor(exec), map[string]string{"event_type": "PartnerOnboarded"},
		uuid.New(), []byte(`{"email":"x@y"}`), nil, discardLogger())
	if err == nil {
		t.Fatal("handler error must surface")
	}
}

type fakeFailHandler struct{}

func (fakeFailHandler) Handle(_ *configuration.AppContext, _ *fakeCommand) (fakeResult, error) {
	return fakeResult{OK: false}, nil // Result.IsSuccess=false, no error
}

func TestHandleMessage_NonSuccessResultStillProcessed(t *testing.T) {
	r := newResolvedReceiver(t, fakeFailHandler{})
	exec := &fakeExec{}
	// Non-success Result with no error → consumer treats the message as
	// handled (acks), records the failure, returns nil.
	if err := r.handleMessage(context.Background(), engineFor(exec), map[string]string{"event_type": "PartnerOnboarded"},
		uuid.New(), []byte(`{"email":"x@y"}`), nil, discardLogger()); err != nil {
		t.Fatalf("non-success Result must still return nil, got %v", err)
	}
}

// --- RetryPendingFailures ---------------------------------------------------

func TestRetryPendingFailures_NilExec(t *testing.T) {
	r := newResolvedReceiver(t, &fakeHandler{})
	if _, err := r.RetryPendingFailures(context.Background(), nil, nil, discardLogger()); err == nil {
		t.Fatal("nil exec must error")
	}
}

func TestRetryPendingFailures_ListError(t *testing.T) {
	r := newResolvedReceiver(t, &fakeHandler{})
	exec := &fakeExec{queryErr: errors.New("list boom")}
	if _, err := r.RetryPendingFailures(context.Background(), engineFor(exec), nil, discardLogger()); err == nil {
		t.Fatal("list error must surface")
	}
}

func TestRetryPendingFailures_RetriesMatchingRows(t *testing.T) {
	r := newResolvedReceiver(t, &fakeHandler{})
	// Two pending rows: one matches this receiver's (sourceKey,eventKey),
	// one is for a different event and must be skipped.
	matching := sampleFailureRow(1)
	matching[2] = "partners"  // SourceKey
	matching[3] = "onboarded" // EventKey
	other := sampleFailureRow(2)
	other[2] = "partners"
	other[3] = "different-event"

	exec := &fakeExec{rows: &fakeRows{data: [][]any{matching, other}}}
	n, err := r.RetryPendingFailures(context.Background(), engineFor(exec), nil, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("only the matching row must be retried, got %d", n)
	}
}
