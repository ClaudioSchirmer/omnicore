//go:build kafka

package kafka

import (
	"context"
	"errors"
	"testing"

	kafka "github.com/segmentio/kafka-go"
)

// The tracker is the correctness core of the Kafka delivery contract: it decides
// which offsets are safe to confirm. These tests drive it directly — the
// generation loop around it needs a live broker and is exercised by the QA
// suites, but the prefix arithmetic must be provable without one.

func key() partKey { return partKey{topic: "users.events", partition: 0} }

// nextOf reads the offset the tracker would commit for the partition.
func nextOf(t *testing.T, trk *genTracker, k partKey) int64 {
	t.Helper()
	trk.mu.Lock()
	defer trk.mu.Unlock()
	st := trk.parts[k]
	if st == nil {
		t.Fatalf("partition %v not tracked", k)
	}
	return st.next
}

// TestTracker_CommitsOnlyTheContiguousPrefix is the property the whole design
// turns on. The consumer dispatches by hash of the aggregate id, so one
// partition completes OUT OF ORDER. Confirming the highest completed offset
// would silently confirm the gaps below it — which is data loss wearing the
// shape of progress.
func TestTracker_CommitsOnlyTheContiguousPrefix(t *testing.T) {
	trk := newGenTracker(nil)
	k := key()
	for _, off := range []int64{10, 11, 12, 13} {
		trk.observe(k, off)
	}
	if got := nextOf(t, trk, k); got != 10 {
		t.Fatalf("floor after observe = %d, want the first delivered offset 10", got)
	}

	// 12 finishes first. Nothing may be confirmed: 10 and 11 are still in flight.
	if err := trk.complete(k, 12); err != nil {
		t.Fatalf("complete(12): %v", err)
	}
	if got := nextOf(t, trk, k); got != 10 {
		t.Fatalf("out-of-order completion must not move the commit point, got %d want 10", got)
	}

	// 10 finishes: the prefix advances to 11 only — 11 is still in flight, so 12
	// stays held even though it is done.
	if err := trk.complete(k, 10); err != nil {
		t.Fatalf("complete(10): %v", err)
	}
	if got := nextOf(t, trk, k); got != 11 {
		t.Fatalf("prefix after 10 = %d, want 11 (11 still in flight)", got)
	}

	// 11 finishes: the prefix now jumps past the already-completed 12.
	if err := trk.complete(k, 11); err != nil {
		t.Fatalf("complete(11): %v", err)
	}
	if got := nextOf(t, trk, k); got != 13 {
		t.Fatalf("prefix after 11 = %d, want 13 (10,11,12 all done)", got)
	}
}

// TestTracker_FailedOffsetHoldsTheLine: Kafka has no negative acknowledgment, so
// "not processed" is expressed as "never completed". The prefix must stop at
// that offset no matter how many later messages succeed, so a restart or
// rebalance redelivers it.
func TestTracker_FailedOffsetHoldsTheLine(t *testing.T) {
	trk := newGenTracker(nil)
	k := key()
	for _, off := range []int64{5, 6, 7} {
		trk.observe(k, off)
	}
	// 5 is never completed (its Completion reported Failed); 6 and 7 succeed.
	if err := trk.complete(k, 6); err != nil {
		t.Fatalf("complete(6): %v", err)
	}
	if err := trk.complete(k, 7); err != nil {
		t.Fatalf("complete(7): %v", err)
	}
	if got := nextOf(t, trk, k); got != 5 {
		t.Fatalf("commit point = %d, want 5 — a failed offset must never be committed past", got)
	}
}

// TestTracker_DropsCompletionsAfterTheGenerationEnded is the rebalance property
// the low-level consumer-group API was chosen for. Once the generation is over
// the partition may belong to another consumer; applying a late completion here
// could advance an offset past messages that consumer is still processing.
func TestTracker_DropsCompletionsAfterTheGenerationEnded(t *testing.T) {
	trk := newGenTracker(nil)
	k := key()
	trk.observe(k, 100)
	trk.close()

	err := trk.complete(k, 100)
	if !errors.Is(err, ErrGenerationEnded) {
		t.Fatalf("complete after the generation ended = %v, want ErrGenerationEnded", err)
	}
	if got := nextOf(t, trk, k); got != 100 {
		t.Fatalf("a dropped completion must not move the commit point, got %d want 100", got)
	}
}

// TestTracker_IgnoresOffsetsAlreadyCovered: a redelivered message whose offset
// the prefix already passed must not rewind or corrupt the state.
func TestTracker_IgnoresOffsetsAlreadyCovered(t *testing.T) {
	trk := newGenTracker(nil)
	k := key()
	trk.observe(k, 20)
	trk.observe(k, 21)
	if err := trk.complete(k, 20); err != nil {
		t.Fatalf("complete(20): %v", err)
	}
	if got := nextOf(t, trk, k); got != 21 {
		t.Fatalf("prefix = %d, want 21", got)
	}
	// A duplicate of an offset already behind the prefix is a no-op.
	if err := trk.complete(k, 20); err != nil {
		t.Fatalf("duplicate complete(20): %v", err)
	}
	if got := nextOf(t, trk, k); got != 21 {
		t.Fatalf("duplicate must not move the prefix, got %d want 21", got)
	}
}

// TestTracker_PartitionsAreIndependent: a stalled partition must not hold back
// another partition's progress.
func TestTracker_PartitionsAreIndependent(t *testing.T) {
	trk := newGenTracker(nil)
	p0 := partKey{topic: "users.events", partition: 0}
	p1 := partKey{topic: "users.events", partition: 1}
	trk.observe(p0, 1)
	trk.observe(p1, 50)

	// p0 stalls (never completed); p1 proceeds.
	if err := trk.complete(p1, 50); err != nil {
		t.Fatalf("complete(p1,50): %v", err)
	}
	if got := nextOf(t, trk, p0); got != 1 {
		t.Fatalf("stalled partition moved: %d", got)
	}
	if got := nextOf(t, trk, p1); got != 51 {
		t.Fatalf("independent partition = %d, want 51", got)
	}
}

// TestTracker_UnknownPartitionIsANoop: a completion for a partition this
// generation never observed is ignored rather than panicking.
func TestTracker_UnknownPartitionIsANoop(t *testing.T) {
	trk := newGenTracker(nil)
	if err := trk.complete(partKey{topic: "ghost", partition: 9}, 1); err != nil {
		t.Fatalf("unknown partition must be a no-op, got %v", err)
	}
}

// The Completion handle over the tracker — Done releases the offset, Failed
// deliberately never does, and both are once-only. This is the outcome surface
// the SyncEngine holds; the generation loop feeding it needs a live broker and
// is exercised by the transport QA suite.

func TestCompletion_DoneReleasesTheOffset(t *testing.T) {
	trk := newGenTracker(nil)
	k := key()
	trk.observe(k, 5)
	c := &completion{trk: trk, key: k, offset: 5}
	if err := c.Done(context.Background()); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if got := nextOf(t, trk, k); got != 6 {
		t.Fatalf("prefix after Done = %d, want 6", got)
	}
	if err := c.Done(context.Background()); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("second Done must report ErrAlreadyCompleted, got %v", err)
	}
}

func TestCompletion_FailedNeverReleases(t *testing.T) {
	trk := newGenTracker(nil)
	k := key()
	trk.observe(k, 5)
	c := &completion{trk: trk, key: k, offset: 5}
	if err := c.Failed(context.Background()); err != nil {
		t.Fatalf("Failed: %v", err)
	}
	if got := nextOf(t, trk, k); got != 5 {
		t.Fatalf("a failed offset must hold the commit point at %d, got %d", 5, got)
	}
	// The outcome is settled: a later Done must not resurrect the release.
	if err := c.Done(context.Background()); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("Done after Failed must report ErrAlreadyCompleted, got %v", err)
	}
	if got := nextOf(t, trk, k); got != 5 {
		t.Fatalf("Done after Failed must not move the prefix, got %d", got)
	}
}

// TestToMessage pins the neutral-envelope translation, including the header
// flattening the relay format depends on.
func TestToMessage(t *testing.T) {
	m := toMessage(kafka.Message{
		Topic: "users.events",
		Key:   []byte("k1"),
		Value: []byte(`{"a":1}`),
		Headers: []kafka.Header{
			{Key: "aggregate_type", Value: []byte(`"users"`)},
			{Key: "event_type", Value: []byte("UPDATED")},
		},
	})
	if m.Topic != "users.events" || string(m.Key) != "k1" || string(m.Value) != `{"a":1}` {
		t.Fatalf("envelope fields mangled: %+v", m)
	}
	if m.Headers["aggregate_type"] != "users" || m.Headers["event_type"] != "UPDATED" {
		t.Fatalf("headers not normalized: %v", m.Headers)
	}
}
