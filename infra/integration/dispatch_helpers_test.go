package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"

	"github.com/google/uuid"
)

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		declared int
		want     int
	}{
		{0, 1},  // zero-value → framework default 1
		{-3, 1}, // negative → default 1
		{1, 1},
		{7, 7},
	}
	for _, tc := range cases {
		if got := resolveVersion(tc.declared); got != tc.want {
			t.Errorf("resolveVersion(%d) = %d, want %d", tc.declared, got, tc.want)
		}
	}
}

func TestResolveActorReadsIdentitySubject(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{Subject: "user-42"})
	if got := resolveActor(ctx); got != "user-42" {
		t.Fatalf("resolveActor with identity = %q, want user-42", got)
	}
}

func TestResolveCorrelationCausationOverride(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	// ctx carries its own values; the override must win.
	ctx.SetCorrelationID(uuid.New())
	ctx.SetCausationID(uuid.New())

	corr := uuid.New()
	caus := uuid.New()
	if got := resolveCorrelation(ctx, &dispatchOpts{hasCorrelation: true, correlation: corr}); got != corr {
		t.Fatalf("resolveCorrelation override = %v, want %v", got, corr)
	}
	if got := resolveCausation(ctx, &dispatchOpts{hasCausation: true, causation: caus}); got != caus {
		t.Fatalf("resolveCausation override = %v, want %v", got, caus)
	}
}

func TestWithAggregateIDSetsFlag(t *testing.T) {
	o := &dispatchOpts{}
	WithAggregateID(domain.NewID("abc"))(o)
	if !o.hasAggregate {
		t.Fatal("WithAggregateID must set hasAggregate")
	}
	if o.aggregateID.String() != "abc" {
		t.Fatalf("aggregateID = %q, want abc", o.aggregateID.String())
	}
}

func TestWithTxStoresHandle(t *testing.T) {
	o := &dispatchOpts{}
	// TxHandle is a sealed marker; nil is a valid value and exercises the
	// option closure without needing a live pgx.Tx.
	WithTx(nil)(o)
	if o.tx != nil {
		t.Fatal("WithTx(nil) should leave tx nil")
	}
}

func TestNullableString(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Errorf("nullableString(\"\") = %v, want nil", got)
	}
	if got := nullableString("x"); got != "x" {
		t.Errorf("nullableString(\"x\") = %v, want x", got)
	}
}

func TestNullableUUID(t *testing.T) {
	if got := nullableUUID(uuid.Nil); got != nil {
		t.Errorf("nullableUUID(Nil) = %v, want nil", got)
	}
	u := uuid.New()
	if got := nullableUUID(u); got != u {
		t.Errorf("nullableUUID(%v) = %v, want same", u, got)
	}
}

func TestMaybeAggregateUUID(t *testing.T) {
	valid := uuid.New()
	cases := []struct {
		name string
		row  dispatchRow
		want uuid.UUID
	}{
		{"no-aggregate", dispatchRow{HasAggregate: false, AggregateID: domain.NewID(valid.String())}, uuid.Nil},
		{"valid-uuid", dispatchRow{HasAggregate: true, AggregateID: domain.NewID(valid.String())}, valid},
		{"unparsable", dispatchRow{HasAggregate: true, AggregateID: domain.NewID("not-a-uuid")}, uuid.Nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maybeAggregateUUID(tc.row); got != tc.want {
				t.Fatalf("maybeAggregateUUID = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmitDispatchEcho(t *testing.T) {
	// nil logger is a no-op (must not panic).
	emitDispatchEcho(nil, "k", PublishEvent{EventType: "T"}, dispatchRow{})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// Standalone event (no aggregate): no aggregate attrs.
	emitDispatchEcho(logger, "standaloneKey", PublishEvent{EventType: "Standalone"},
		dispatchRow{EventID: uuid.New(), ThreadID: uuid.New(), Actor: "anonymous", Version: 1})
	if !strings.Contains(buf.String(), "integration.event.emitted") {
		t.Fatal("expected emitted log line")
	}
	if strings.Contains(buf.String(), "aggregate_type") {
		t.Fatal("standalone event must not log aggregate_type")
	}

	// Aggregate-bound event: aggregate attrs present.
	buf.Reset()
	emitDispatchEcho(logger, "aggKey", PublishEvent{EventType: "Agg", Aggregate: "User"},
		dispatchRow{EventID: uuid.New(), ThreadID: uuid.New(), Actor: "u-1", Version: 2,
			AggregateID: domain.NewID(uuid.New().String())})
	if !strings.Contains(buf.String(), "aggregate_type") {
		t.Fatal("aggregate-bound event must log aggregate_type")
	}
}

func TestWriteIntegrationEventRejectsNilPool(t *testing.T) {
	// Standalone path (tx == nil) with a client carrying no Postgres pool
	// must surface a loud error. Building the args first also exercises the
	// nullableString / nullableUUID / maybeAggregateUUID helpers in situ.
	c := &client{}
	row := dispatchRow{
		EventID:   uuid.New(),
		EventType: "T",
		Version:   1,
		Payload:   []byte("{}"),
		ThreadID:  uuid.New(),
		Actor:     "anonymous",
	}
	err := writeIntegrationEvent(context.Background(), c, nil, row)
	if err == nil || !strings.Contains(err.Error(), "Postgres pool") {
		t.Fatalf("expected nil-pool error, got %v", err)
	}
}

func TestDispatchNilCtx(t *testing.T) {
	if err := Dispatch(nil, "k", nil); err == nil || !strings.Contains(err.Error(), "ctx is required") {
		t.Fatalf("expected ctx-required error, got %v", err)
	}
}

func TestDispatchMarshalError(t *testing.T) {
	Reset()
	defer Reset()
	Configure(&Config{
		Publishes: PublishConfig{Events: map[string]PublishEvent{
			"k": {EventType: "T"}, // no aggregate → no WithAggregateID needed
		}},
	}, nil, nil)

	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	// A channel cannot be JSON-marshalled → marshal error branch.
	err := Dispatch(ctx, "k", make(chan int))
	if err == nil || !strings.Contains(err.Error(), "marshal payload") {
		t.Fatalf("expected marshal error, got %v", err)
	}
}

func TestDispatchReachesWriteWithNilPool(t *testing.T) {
	Reset()
	defer Reset()
	Configure(&Config{
		Publishes: PublishConfig{Events: map[string]PublishEvent{
			"k": {EventType: "T"},
		}},
	}, nil, nil) // pg nil → standalone write fails loudly

	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "k", map[string]any{"ok": true})
	if err == nil || !strings.Contains(err.Error(), "write eventKey=") {
		t.Fatalf("expected write error wrapper, got %v", err)
	}
}

// Guard: the dispatchRow payload an aggregate event carries round-trips as
// valid JSON (the marshal happens inside Dispatch; this anchors the contract
// the helpers above assume).
func TestDispatchRowPayloadIsValidJSON(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"email": "x@y"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["email"] != "x@y" {
		t.Fatalf("payload round-trip drift: %#v", back)
	}
}
