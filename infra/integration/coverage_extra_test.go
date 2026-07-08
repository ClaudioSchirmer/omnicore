package integration

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/transport"

	"github.com/google/uuid"
)

// --- config.go nil-receiver + remaining Validate branches -------------------

func TestConfig_NilReceiverSafety(t *testing.T) {
	var c *Config
	if _, ok := c.LookupPublish("k"); ok {
		t.Error("nil Config.LookupPublish must miss")
	}
	if _, _, ok := c.LookupSubscribe("s", "e"); ok {
		t.Error("nil Config.LookupSubscribe must miss")
	}
	// ApplyDefaults / Validate must be no-op / nil on a nil receiver.
	c.ApplyDefaults("svc")
	if err := c.Validate(); err != nil {
		t.Errorf("nil Config.Validate must return nil, got %v", err)
	}
}

func TestConfig_LookupPublishMiss(t *testing.T) {
	c := &Config{Publishes: PublishConfig{Events: map[string]PublishEvent{}}}
	if _, ok := c.LookupPublish("absent"); ok {
		t.Error("missing key must report not-ok")
	}
}

func TestConfig_ApplyDefaultsNilSubscribes(t *testing.T) {
	// Subscribes nil → ApplyDefaults must allocate the map (the nil branch).
	c := &Config{}
	c.ApplyDefaults("orders")
	if c.Subscribes == nil {
		t.Fatal("ApplyDefaults must allocate a nil Subscribes map")
	}
}

func TestConfig_ValidateDefaultsWorkersNegative(t *testing.T) {
	c := &Config{Defaults: SubscriberDefaults{Workers: -1, StartFrom: "latest"}}
	err := c.Validate()
	if err == nil {
		t.Fatal("negative defaults.workers must fail validation")
	}
}

// --- consumer_pool.go: bucket overflow + Shutdown branches ------------------

func TestBucketOfMessage_NegativeSumStaysInRange(t *testing.T) {
	// A 21-byte 0xff key overflows the int accumulator to a negative value,
	// exercising the `if sum < 0 { sum = -sum }` branch.
	key := make([]byte, 21)
	for i := range key {
		key[i] = 0xff
	}
	const workers = 4
	got := bucketOfMessage(transport.Message{Key: key}, workers)
	if got < 0 || got >= workers {
		t.Fatalf("bucket %d out of range [0,%d) after overflow", got, workers)
	}
}

func TestConsumerPool_ShutdownNilReceiver(t *testing.T) {
	var p *ConsumerPool
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil pool Shutdown must return nil, got %v", err)
	}
}

func TestConsumerPool_ShutdownDrainTimeout(t *testing.T) {
	p := NewConsumerPool(NewRegistry(), &Config{}, nil, nil, nil, discardLogger())
	// Simulate a supervisor that never exits so workers.Wait() blocks and the
	// drain has to fall through to the drainCtx.Done() timeout branch.
	p.workers.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Shutdown(ctx); err == nil {
		t.Fatal("expected drain timeout error when the supervisor never exits")
	}
}

// --- consumer_pool.go Start: pure error branches (no Kafka dial) ------------

func TestConsumerPool_StartEmptyRegistryNoop(t *testing.T) {
	p := NewConsumerPool(NewRegistry(), &Config{}, nil, nil, nil, discardLogger())
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("empty registry Start must be a no-op, got %v", err)
	}
}

func TestConsumerPool_StartUnresolvedReceiverErrors(t *testing.T) {
	reg := NewRegistry()
	reg.From("missing").On("ev", fakeRequest{}, &fakeHandler{})
	// cfg does not declare the "missing" source → resolveAgainstYAML fails
	// before any consumer goroutine spawns.
	p := NewConsumerPool(reg, &Config{Subscribes: map[string]SubscribeEntry{}}, nil, nil, nil, discardLogger())
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("unresolved receiver must abort Start")
	}
}

func TestConsumerPool_StartDuplicateEventTypeErrors(t *testing.T) {
	reg := NewRegistry()
	reg.From("partners").
		On("ev1", fakeRequest{}, &fakeHandler{}).
		On("ev2", fakeRequest{}, &fakeHandler{})
	// Both event keys resolve to the SAME wire event_type on the same
	// (topic, consumerGroup) → groupReceivers aborts before spawning readers.
	cfg := &Config{Subscribes: map[string]SubscribeEntry{
		"partners": {
			Topic:         "partners.events",
			ConsumerGroup: "orders-int",
			Workers:       1,
			StartFrom:     "latest",
			Events: map[string]SubscribeEvent{
				"ev1": {EventType: "Same"},
				"ev2": {EventType: "Same"},
			},
		},
	}}
	p := NewConsumerPool(reg, cfg, nil, nil, nil, discardLogger())
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("duplicate event_type on one group must abort Start")
	}
}

// --- registry.go: toCommand pointer-receiver path ---------------------------

type ptrRequest struct {
	Email string `json:"email"`
}

// ToCommand on a POINTER receiver, so the request value type does NOT carry
// the method and toCommand must invoke it on the pointer-typed reqValue.
func (r *ptrRequest) ToCommand() *fakeCommand { return &fakeCommand{Email: r.Email} }

func TestHandleMessage_PointerReceiverToCommand(t *testing.T) {
	reg := NewRegistry()
	reg.From("partners").On("onboarded", ptrRequest{}, &fakeHandler{})
	r := reg.Receivers()[0]
	r.topic = "partners.events"
	r.wireEventType = "PartnerOnboarded"
	r.consumerGroup = "orders-int"
	r.workers = 1
	r.startFrom = "latest"

	exec := &fakeExec{}
	if err := r.handleMessage(context.Background(),
		engineFor(exec), map[string]string{"event_type": "PartnerOnboarded"},
		uuid.New(), []byte(`{"email":"x@y"}`), nil, discardLogger()); err != nil {
		t.Fatalf("pointer-receiver ToCommand path must succeed, got %v", err)
	}
}

// --- registry.go: handleMessage dedup-check + mark-processed failure --------

func TestHandleMessage_DedupCheckErrorFallsThrough(t *testing.T) {
	h := &fakeHandler{}
	r := newResolvedReceiver(t, h)
	// The dedup pre-check Query errors → IsAlreadyProcessed errs; handleMessage
	// logs and still attempts the handler (at-least-once).
	exec := &fakeExec{queryErr: errors.New("conn reset")}
	if err := r.handleMessage(context.Background(),
		engineFor(exec), map[string]string{"event_type": "PartnerOnboarded"},
		uuid.New(), []byte(`{"email":"x@y"}`), nil, discardLogger()); err != nil {
		t.Fatalf("dedup-check error must fall through, got %v", err)
	}
	if !h.called {
		t.Fatal("handler must still run after a dedup-check failure")
	}
}

func TestHandleMessage_MarkProcessedErrorIsBestEffort(t *testing.T) {
	h := &fakeHandler{}
	r := newResolvedReceiver(t, h)
	// The dedup pre-check finds no row (not processed) so the handler runs;
	// Exec (MarkProcessed) fails → the mark_processed_failed warn branch.
	exec := &fakeExec{execErr: errors.New("insert failed")}
	if err := r.handleMessage(context.Background(),
		engineFor(exec), map[string]string{"event_type": "PartnerOnboarded"},
		uuid.New(), []byte(`{"email":"x@y"}`), nil, discardLogger()); err != nil {
		t.Fatalf("MarkProcessed failure must not surface, got %v", err)
	}
	if !h.called {
		t.Fatal("handler must run before the MarkProcessed attempt")
	}
}

// --- registry.go: RetryPendingFailures ctx-cancel mid-loop ------------------

func TestRetryPendingFailures_ContextCancelStopsLoop(t *testing.T) {
	r := newResolvedReceiver(t, &fakeHandler{})
	row := sampleFailureRow(1)
	row[2] = "partners"  // SourceKey
	row[3] = "onboarded" // EventKey
	exec := &fakeExec{rows: &fakeRows{data: [][]any{row}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the loop runs
	n, err := r.RetryPendingFailures(ctx, engineFor(exec), nil, discardLogger())
	if err == nil {
		t.Fatal("cancelled context must surface ctx.Err() from the loop")
	}
	if n != 1 {
		t.Fatalf("the matching row is attempted once before the cancel check, got %d", n)
	}
}

// --- registry.go: planReceiver Handle arity branch --------------------------

type oneArgHandle struct{}

func (oneArgHandle) Handle(*configuration.AppContext) (fakeResult, error) {
	return fakeResult{}, nil
}

func TestPlanReceiver_HandleWrongArity(t *testing.T) {
	if _, err := planReceiver(fakeRequest{}, oneArgHandle{}); err == nil {
		t.Fatal("Handle taking the wrong number of params must error")
	}
}

// --- registry.go: On panic branches -----------------------------------------

func TestSourceBuilder_OnNilBuilderPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("On on a nil builder must panic")
		}
	}()
	var sb *SourceBuilder
	sb.On("ev", fakeRequest{}, &fakeHandler{})
}

func TestSourceBuilder_OnPlanErrorPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("On with a sample missing ToCommand must panic via planReceiver")
		}
	}()
	// noToCommand has no ToCommand method → planReceiver errors → On panics.
	NewRegistry().From("x").On("ev", noToCommand{}, &fakeHandler{})
}

// --- registry.go: isSuccessResult non-bool + no-return shapes ---------------

type strResult struct{}

func (strResult) IsSuccess() string { return "yes" }

type voidResult struct{}

func (voidResult) IsSuccess() {}

func TestIsSuccessResult_NonBoolAndVoid(t *testing.T) {
	// IsSuccess returning a non-bool → fall to the lenient default (success).
	if !isSuccessResult(reflect.ValueOf(strResult{})) {
		t.Error("non-bool IsSuccess must default to success")
	}
	// IsSuccess returning nothing → len(out)==0 branch → success.
	if !isSuccessResult(reflect.ValueOf(voidResult{})) {
		t.Error("void IsSuccess must default to success")
	}
}

// --- dispatch.go: writeIntegrationEvent WithTx branch (foreign handle) ------

// foreignTxHandle satisfies persistence.TxHandle (via the exported embed
// token) but carries no neutral Tx, so the canonical db.UnwrapTx panics.
// This drives the `tx != nil` branch of writeIntegrationEvent; the real
// in-TX Exec success path needs a live engine tx and is covered by
// integration tests, not here.
type foreignTxHandle struct {
	persistence.SealedTxHandle
}

func TestWriteIntegrationEvent_WithForeignTxPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("a foreign TxHandle must panic at UnwrapTx")
		}
	}()
	c := &client{}
	row := dispatchRow{
		EventID:   uuid.New(),
		EventType: "T",
		Version:   1,
		Payload:   []byte("{}"),
		ThreadID:  uuid.New(),
		Actor:     "anonymous",
	}
	_ = writeIntegrationEvent(context.Background(), c, foreignTxHandle{}, row)
}
