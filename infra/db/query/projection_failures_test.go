package query

import (
	"context"
	"strings"
	"testing"
)

// The ledger helpers reach the relational backend through the engine's neutral
// Querier/Dialect seam, so a scriptable fake stands in for a live database. The
// DDL itself is validated by applying the embedded 0003 migration on each
// dialect; what these pin is the shape of the statements built on top of it —
// above all the argument order, which no compiler checks.

func TestRecordProjectionFailure_RequiredFields(t *testing.T) {
	eng := newScriptEngine(nil, nil)
	cases := []struct {
		name string
		rec  ProjectionFailureRecord
	}{
		{"no consumer group", ProjectionFailureRecord{Kind: ProjectionFailureKindEvent, AggregateType: "users", AggregateID: "u1", Payload: []byte("{}")}},
		{"no aggregate type", ProjectionFailureRecord{Kind: ProjectionFailureKindEvent, ConsumerGroup: "g", AggregateID: "u1", Payload: []byte("{}")}},
		{"no aggregate id", ProjectionFailureRecord{Kind: ProjectionFailureKindEvent, ConsumerGroup: "g", AggregateType: "users", Payload: []byte("{}")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := RecordProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), c.rec); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

// TestRecordProjectionFailure_ArgumentOrder is the assertion that matters: the
// insert columns and the bound arguments are two separate lists, and swapping
// two of them compiles, runs, and silently stores an event under the wrong
// identity — which would then be replayed as the wrong aggregate.
func TestRecordProjectionFailure_ArgumentOrder(t *testing.T) {
	eng := newScriptEngine(nil, nil)
	var gotSQL string
	var gotArgs []any
	eng.q.(*fakeQuerier).execFn = func(sql string, args []any) error {
		gotSQL, gotArgs = sql, args
		return nil
	}

	rec := ProjectionFailureRecord{
		Kind:          ProjectionFailureKindEvent,
		ConsumerGroup: "svc-sync",
		Topic:         "users.events",
		AggregateType: "users",
		EventType:     "UPDATED",
		AggregateID:   "u1",
		Traceparent:   "00-abc-def-01",
		Payload:       []byte(`{"_ids":{"id":"u1"}}`),
		Error:         "boom",
	}
	if err := RecordProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), rec); err != nil {
		t.Fatalf("RecordProjectionFailure: %v", err)
	}
	if !strings.Contains(gotSQL, projectionFailureTable) {
		t.Errorf("statement must target %s, got %q", projectionFailureTable, gotSQL)
	}
	// args[0] is the Go-minted surrogate id; the rest follow
	// projectionFailureInsertCols exactly. Empty nullable columns bind as nil
	// (stage/local_id on an event row).
	if len(gotArgs) != len(projectionFailureInsertCols) {
		t.Fatalf("bound %d args for %d insert columns", len(gotArgs), len(projectionFailureInsertCols))
	}
	want := []any{string(rec.Kind), rec.ConsumerGroup, rec.Topic, rec.AggregateType, rec.EventType,
		rec.AggregateID, nil, nil, rec.Traceparent, string(rec.Payload), rec.Error}
	for i, w := range want {
		if gotArgs[i+1] != w {
			t.Errorf("arg %d (%s) = %v, want %v", i+1, projectionFailureInsertCols[i+1], gotArgs[i+1], w)
		}
	}
}

// TestRecordProjectionFailure_RippleShape pins the ripple-kind row: no payload
// (replay reads current state — storing one is rejected), stage and local_id
// bound, event_type NULL.
func TestRecordProjectionFailure_RippleShape(t *testing.T) {
	eng := newScriptEngine(nil, nil)
	var gotArgs []any
	eng.q.(*fakeQuerier).execFn = func(_ string, args []any) error { gotArgs = args; return nil }

	rec := ProjectionFailureRecord{
		Kind:          ProjectionFailureKindRipple,
		ConsumerGroup: "svc-sync",
		Topic:         "view:products",
		AggregateType: "sales",
		AggregateID:   "p1",
		Stage:         ProjectionFailureStageUpsert,
		LocalID:       "s1",
		Error:         "boom",
	}
	if err := RecordProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), rec); err != nil {
		t.Fatalf("RecordProjectionFailure: %v", err)
	}
	want := []any{"ripple", "svc-sync", "view:products", "sales", nil, "p1", "upsert", "s1", nil, nil, "boom"}
	for i, w := range want {
		if gotArgs[i+1] != w {
			t.Errorf("arg %d (%s) = %v, want %v", i+1, projectionFailureInsertCols[i+1], gotArgs[i+1], w)
		}
	}

	rec.Payload = []byte(`{"stale":"state"}`)
	if err := RecordProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), rec); err == nil {
		t.Fatal("a ripple row carrying a payload must be rejected — replaying stale state is the defect")
	}
	if err := RecordProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), ProjectionFailureRecord{
		Kind: ProjectionFailureKindEvent, ConsumerGroup: "g", AggregateType: "users", AggregateID: "u1",
	}); err == nil {
		t.Fatal("an event row WITHOUT a payload must be rejected — it could never be replayed")
	}
	if err := RecordProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), ProjectionFailureRecord{
		ConsumerGroup: "g", AggregateType: "users", AggregateID: "u1", Payload: []byte("{}"),
	}); err == nil {
		t.Fatal("a row without a kind must be rejected")
	}
}

// TestRecordProjectionFailure_RefreshesPayloadOnConflict pins the semantic that
// makes the ledger a live-state mirror rather than a log: a newer park for the
// same aggregate must OVERWRITE the stored payload. Keeping the old one would
// replay stale state over fresh state.
func TestRecordProjectionFailure_RefreshesPayloadOnConflict(t *testing.T) {
	eng := newScriptEngine(nil, nil)
	var gotSQL string
	eng.q.(*fakeQuerier).execFn = func(sql string, _ []any) error { gotSQL = sql; return nil }
	if err := RecordProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), ProjectionFailureRecord{
		Kind: ProjectionFailureKindEvent, ConsumerGroup: "g", Topic: "t",
		AggregateType: "users", AggregateID: "u1", Payload: []byte("{}"),
	}); err != nil {
		t.Fatalf("RecordProjectionFailure: %v", err)
	}
	for _, col := range []string{"payload", "event_type", "stage", "local_id", "attempt", "resolved_at"} {
		if !strings.Contains(gotSQL, col) {
			t.Errorf("the conflict clause must refresh %q, statement was %q", col, gotSQL)
		}
	}
}

func TestResolveProjectionFailure_RequiredFields(t *testing.T) {
	eng := newScriptEngine(nil, nil)
	if err := ResolveProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), "", ProjectionFailureKindEvent, "t", "users", "u1"); err == nil {
		t.Error("empty consumer_group must be rejected")
	}
	if err := ResolveProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), "g", ProjectionFailureKindEvent, "t", "", "u1"); err == nil {
		t.Error("empty aggregate_type must be rejected")
	}
	if err := ResolveProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), "g", ProjectionFailureKindEvent, "t", "users", ""); err == nil {
		t.Error("empty aggregate_id must be rejected")
	}
}

// TestResolveProjectionFailure_OnlyTouchesPending: resolving must not reopen or
// re-stamp an already-resolved row.
func TestResolveProjectionFailure_OnlyTouchesPending(t *testing.T) {
	eng := newScriptEngine(nil, nil)
	var gotSQL string
	eng.q.(*fakeQuerier).execFn = func(sql string, _ []any) error { gotSQL = sql; return nil }
	if err := ResolveProjectionFailure(context.Background(), eng.Querier(), eng.Dialect(), "g", ProjectionFailureKindEvent, "t", "users", "u1"); err != nil {
		t.Fatalf("ResolveProjectionFailure: %v", err)
	}
	if !strings.Contains(gotSQL, "resolved_at IS NULL") {
		t.Errorf("resolve must be scoped to pending rows, got %q", gotSQL)
	}
}

func TestListPendingProjectionFailures_RequiresConsumerGroup(t *testing.T) {
	eng := newScriptEngine(nil, nil)
	if _, err := ListPendingProjectionFailures(context.Background(), eng.Querier(), eng.Dialect(), ""); err == nil {
		t.Error("empty consumer_group must be rejected")
	}
}

// TestParkEvent_WithoutEngineIsSafe: the wiring-less test shape has no
// relational engine. Parking must degrade to the log line rather than panic —
// the park is best-effort by contract, and the sweep is the backstop.
func TestParkEvent_WithoutEngineIsSafe(t *testing.T) {
	s := &SyncEngine{groupID: "g"}
	s.parkEvent(context.Background(), kafkaEvent{
		AggregateType: "users", EventType: "UPDATED", AggregateID: "u1",
	}, context.Canceled)
}
