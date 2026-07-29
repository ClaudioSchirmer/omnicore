package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The insert-once loss shape: an aggregate written exactly once, whose ONLY
// event hits a failure. There is no later write to reconverge it, so each
// recovery mechanism must bring the document back on its own. One test per
// mechanism — the in-process retry here and the ledger replay below; the sweep
// leg lives in reconcile_test.go (TestReconcile_DetectsMissingDocument).

// TestInsertOnce_TransientFailureHealsViaRetry: the write path fails twice
// (transient Mongo outage) and heals; the retry loop must land the document
// and confirm the event, without ever touching the ledger.
func TestInsertOnce_TransientFailureHealsViaRetry(t *testing.T) {
	coll := &fakeColl{failWrites: 2}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "g", []*ViewDefinition{view}, 1)

	c := &recordingCompletion{}
	s.processToOutcome(context.Background(), queuedEvent{event: convRoleEvent(), completion: c})

	if c.done != 1 || c.failed != 0 {
		t.Fatalf("a healed event must confirm exactly once, done=%d failed=%d", c.done, c.failed)
	}
	if len(coll.updates) == 0 {
		t.Fatal("the only event of the aggregate must reach the projection once the outage heals")
	}
	h := s.ProjectionHealth()
	if got := h.Counters[MetricProjectionRetried]; got != 2 {
		t.Errorf("retried = %d, want 2 (one per failed attempt)", got)
	}
	if h.Counters[MetricProjectionParked] != 0 {
		t.Errorf("a within-budget recovery must not park, parked = %d", h.Counters[MetricProjectionParked])
	}
}

// TestProcessAttempt_WedgedWriteAbortsWithTheContext: a write that never
// returns (quorum loss, black-hole partition) must end WITH the context rather
// than stall the worker forever. The attempt deadline is context.WithTimeout —
// what this pins is that the context actually reaches the store operation and
// that a context-ended event is handed back, not confirmed and not parked.
func TestProcessAttempt_WedgedWriteAbortsWithTheContext(t *testing.T) {
	coll := &fakeColl{blockWrites: true}
	store := newFakeMongo(coll)
	view := convRoleView()
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, nil, "g", []*ViewDefinition{view}, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	c := &recordingCompletion{}
	go func() {
		s.processToOutcome(ctx, queuedEvent{event: convRoleEvent(), completion: c})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged write stalled processToOutcome — the context is not reaching the store operation")
	}
	if c.failed != 1 || c.done != 0 {
		t.Fatalf("a context-ended event must be handed back exactly once, done=%d failed=%d", c.done, c.failed)
	}
	if got := s.ProjectionHealth().Counters[MetricProjectionParked]; got != 0 {
		t.Errorf("a shutdown/deadline abort must not park, parked = %d", got)
	}
}

// parkedLedgerQuerier scripts the relational seam for the replay test: the
// park INSERT is captured, and the pending-failures SELECT answers with the
// captured record — the same round trip the live ledger performs.
type parkedLedgerQuerier struct {
	fakeQuerier
	parked   [][]any  // captured park-insert args
	resolves []string // captured resolve statements
}

func newParkedLedgerQuerier() *parkedLedgerQuerier {
	q := &parkedLedgerQuerier{}
	q.execFn = func(sql string, args []any) error {
		switch {
		case strings.Contains(sql, "INSERT"):
			q.parked = append(q.parked, args)
		case strings.Contains(sql, "resolved_at"):
			q.resolves = append(q.resolves, sql)
		}
		return nil
	}
	q.queryFn = func(sql string, _ []any) (core.Rows, error) {
		if !strings.Contains(sql, projectionFailureTable) || len(q.parked) == 0 {
			return &fakeRows{}, nil
		}
		// One pending row, scanned in the SELECT's column order (see
		// selectProjectionFailures). The park bound projectionFailureInsertCols
		// in order: id, kind, group, topic, aggregate_type, event_type,
		// aggregate_id, stage, local_id, traceparent, payload, error.
		rec := q.parked[len(q.parked)-1]
		asPtr := func(v any) *string {
			if v == nil {
				return nil
			}
			s := v.(string)
			return &s
		}
		return &fakeRows{
			rows: 1,
			scan: func(_ int, dest []any) error {
				*(dest[0].(*string)) = rec[0].(string)   // id
				*(dest[1].(*string)) = rec[1].(string)   // kind
				*(dest[2].(*string)) = rec[2].(string)   // consumer_group
				*(dest[3].(*string)) = rec[3].(string)   // topic
				*(dest[4].(*string)) = rec[4].(string)   // aggregate_type
				*(dest[5].(**string)) = asPtr(rec[5])    // event_type
				*(dest[6].(*string)) = rec[6].(string)   // aggregate_id
				*(dest[7].(**string)) = asPtr(rec[7])    // stage
				*(dest[8].(**string)) = asPtr(rec[8])    // local_id
				*(dest[9].(**string)) = asPtr(rec[9])    // traceparent
				*(dest[10].(**string)) = asPtr(rec[10])  // payload
				*(dest[11].(*string)) = rec[11].(string) // error
				*(dest[12].(*int)) = 1                   // attempt
				return nil
			},
		}, nil
	}
	return q
}

// TestInsertOnce_ExhaustedEventComesBackViaLedgerReplay: every in-process
// attempt fails, the event parks, the stream advances — and the replay driver
// must then deliver the document and resolve the row. This is the "advance is
// deferred, not forgotten" contract, exercised end to end at the unit level.
func TestInsertOnce_ExhaustedEventComesBackViaLedgerReplay(t *testing.T) {
	q := newParkedLedgerQuerier()
	coll := &fakeColl{failWrites: processRetries} // outage outlives the whole budget
	store := newFakeMongo(coll)
	view := convRoleView()
	eng := newFakeEngine(q)
	// The composer reads through the SAME querier: route QueryMaps to the
	// standard convergence fixture rows.
	q.queryMapsFn = convComposerRows
	identityResolver.lease = 0
	s := NewSyncEngine(eng, store, identityResolver, nil, "g", []*ViewDefinition{view}, 1)

	c := &recordingCompletion{}
	s.processToOutcome(context.Background(), queuedEvent{event: convRoleEvent(), completion: c})

	if c.done != 1 {
		t.Fatalf("a parked event still advances the stream, done=%d failed=%d", c.done, c.failed)
	}
	if len(q.parked) != 1 {
		t.Fatalf("expected exactly one park insert, got %d", len(q.parked))
	}
	if len(coll.updates) != 0 {
		t.Fatal("precondition broken: no document may exist before the replay")
	}

	// The outage heals (failWrites exhausted by the failed attempts); replay.
	if err := s.RetryPendingProjectionFailures(context.Background()); err != nil {
		t.Fatalf("RetryPendingProjectionFailures: %v", err)
	}
	if len(coll.updates) == 0 {
		t.Fatal("the replay must project the aggregate's only event — otherwise the park was a loss")
	}
	if len(q.resolves) != 1 {
		t.Fatalf("a successful replay must resolve the ledger row exactly once, got %d", len(q.resolves))
	}
	if got := s.ProjectionHealth().Counters[MetricProjectionReplayed]; got != 1 {
		t.Errorf("replayed = %d, want 1", got)
	}
}

// The two tiny helpers the retry loop leans on — trivial, but a drifted
// errString would blank every ledger row's error column.
func TestErrStringAndMinInt(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Errorf("errString(nil) = %q", got)
	}
	if got := errString(errFake); got != errFake.Error() {
		t.Errorf("errString = %q", got)
	}
	if minInt(2, 7) != 2 || minInt(7, 2) != 2 || minInt(3, 3) != 3 {
		t.Error("minInt drifted")
	}
}
