package query

import (
	"context"
	"fmt"
	"testing"
)

// first returns the index of the first occurrence of op in the trace, or a
// large sentinel when absent — so an absent op never compares as "earliest".
func first(ops []string, op string) int {
	for i, o := range ops {
		if o == op {
			return i
		}
	}
	return len(ops) + 1
}

// These cover the delivery contract and the obligation isolation that replaced
// the old fire-and-forget projection loop.

// convRoleEvent is the role-table event the convergence fixtures compose from.
func convRoleEvent() kafkaEvent {
	return kafkaEvent{
		AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":2,"base_id":"p1","base_revision":3}}`),
	}
}

// TestProcess_FanOutFailureDoesNotDiscardOwnProjection is the incident's
// regression test.
//
// A blue-green flip retired the collection the shared-base fan-out was probing;
// the server killed the query with QueryPlanKilled. Because the probe ran BEFORE
// the writer's own projection and the first error aborted the event, the
// aggregate's own document was never written — and with no redelivery it was
// gone for good. One user out of 100200.
//
// The probe failing must now cost only the probe.
func TestProcess_FanOutFailureDoesNotDiscardOwnProjection(t *testing.T) {
	// findErr models the retired-collection kill: FindIDsByField (the fan-out
	// probe) fails, while writes to the collection still work.
	coll := &fakeColl{findErr: errFake}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	err := s.process(context.Background(), convRoleEvent())
	if err == nil {
		t.Fatal("the fan-out probe failure must still fail the event so it is retried")
	}
	if len(coll.updates) == 0 {
		t.Fatal("the event's OWN document must be projected even when the fan-out probe fails — " +
			"this is the exact loss the rebuild_scale RED exhibited")
	}
}

// TestProcess_OwnProjectionRunsBeforeTheProbe pins the ORDER, not just the
// isolation. Isolation alone would still leave the own write exposed to any
// future failure mode of the probe; ordering removes the exposure entirely.
func TestProcess_OwnProjectionRunsBeforeTheProbe(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	if err := s.process(context.Background(), convRoleEvent()); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.updates) == 0 {
		t.Fatal("expected the own projection to run")
	}
	if coll.idFinds == 0 {
		t.Fatal("expected the fan-out probe to run")
	}
	// Assert via the recorded operation sequence, never by timing.
	if first(coll.ops, "write") > first(coll.ops, "idfind") {
		t.Errorf("the event's own document must be projected BEFORE the shared-base fan-out probe, ops=%v", coll.ops)
	}
}

// TestProcess_ReplayConvergesToTheSameState is the property message-granular
// retry rests on. Every obligation is idempotent (guarded pipelines, guarded
// deletes, advance-only registry stamps), so replaying a whole message is safe
// and no per-obligation replay machinery is needed. If this breaks, the retry
// design breaks with it.
func TestProcess_ReplayConvergesToTheSameState(t *testing.T) {
	run := func(times int) []map[string]any {
		coll := &fakeColl{}
		store := newFakeMongo(coll)
		view := convRoleView()
		s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)
		for i := 0; i < times; i++ {
			if err := s.process(context.Background(), convRoleEvent()); err != nil {
				t.Fatalf("process (pass %d): %v", i+1, err)
			}
		}
		return coll.updates
	}
	once := run(1)
	thrice := run(3)
	if len(once) == 0 {
		t.Fatal("expected at least one write")
	}
	// The same pipeline is applied each pass — replay adds repetitions, never a
	// DIFFERENT instruction. Comparing the last write of each run pins that.
	last := func(u []map[string]any) string { return fmt.Sprint(u[len(u)-1]) }
	if last(once) != last(thrice) {
		t.Errorf("replay produced a different write:\n first: %v\n replayed: %v", last(once), last(thrice))
	}
}

// TestProcessToOutcome_ConfirmsOnSuccess: a processed event is confirmed exactly
// once, which is what lets the stream advance.
func TestProcessToOutcome_ConfirmsOnSuccess(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	c := &recordingCompletion{}
	s.processToOutcome(context.Background(), queuedEvent{event: convRoleEvent(), completion: c})
	if c.done != 1 || c.failed != 0 {
		t.Fatalf("a processed event must report Done exactly once, done=%d failed=%d", c.done, c.failed)
	}
}

// TestProcessToOutcome_HandsBackOnShutdown: an event interrupted by shutdown
// must be HANDED BACK, not confirmed and not recorded as a defect — it never got
// a fair attempt, and the next boot should receive it. Confirming it here is
// precisely the silent loss this program removes.
func TestProcessToOutcome_HandsBackOnShutdown(t *testing.T) {
	coll := &fakeColl{updateErr: errFake} // force the retry path
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "", []*ViewDefinition{view}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutting down

	c := &recordingCompletion{}
	s.processToOutcome(ctx, queuedEvent{event: convRoleEvent(), completion: c})
	if c.failed != 1 || c.done != 0 {
		t.Fatalf("a shutdown-interrupted event must report Failed exactly once, done=%d failed=%d", c.done, c.failed)
	}
}

// TestProjectionHealth_TracksOutcomesAndLiveness pins the signals an operator
// alarms on. The counters matter, but LastProcessed matters more: a projection
// loop that has stopped emits no errors, so staleness is the only observable
// that distinguishes "healthy and idle" from "dead".
func TestProjectionHealth_TracksOutcomesAndLiveness(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "g", []*ViewDefinition{view}, 1)

	if h := s.ProjectionHealth(); !h.LastProcessed.IsZero() {
		t.Fatal("a fresh engine must report no processed event yet")
	}

	s.processToOutcome(context.Background(), queuedEvent{event: convRoleEvent(), completion: &recordingCompletion{}})

	h := s.ProjectionHealth()
	if h.Counters[MetricProjectionProcessed] != 1 {
		t.Errorf("processed = %d, want 1", h.Counters[MetricProjectionProcessed])
	}
	if h.LastProcessed.IsZero() {
		t.Error("a successful outcome must stamp the liveness clock — staleness is the alarm")
	}
	if h.Counters[MetricProjectionParked] != 0 {
		t.Errorf("nothing should have parked, got %d", h.Counters[MetricProjectionParked])
	}
}

// TestProjectionHealth_CountsParkedEvents: an exhausted event must be visible as
// a park, because from that moment convergence depends on the replay driver or
// the sweep rather than on the stream.
func TestProjectionHealth_CountsParkedEvents(t *testing.T) {
	coll := &fakeColl{updateErr: errFake}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "g", []*ViewDefinition{view}, 1)

	c := &recordingCompletion{}
	s.processToOutcome(context.Background(), queuedEvent{event: convRoleEvent(), completion: c})

	h := s.ProjectionHealth()
	if h.Counters[MetricProjectionParked] != 1 {
		t.Errorf("parked = %d, want 1", h.Counters[MetricProjectionParked])
	}
	if h.Counters[MetricProjectionRetried] == 0 {
		t.Error("the retry attempts must be counted")
	}
	if c.done != 1 {
		t.Errorf("a parked event still advances the stream, done=%d", c.done)
	}
}

// TestProjectionHealth_NilEngineIsSafe: the accessor is used from probe and log
// paths, so it must never panic on a zero value.
func TestProjectionHealth_NilEngineIsSafe(t *testing.T) {
	var s *SyncEngine
	if h := s.ProjectionHealth(); h.Counters != nil {
		t.Errorf("nil engine must report an empty health, got %v", h.Counters)
	}
}
