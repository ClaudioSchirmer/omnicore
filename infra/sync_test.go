package infra

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

// TestDecodeAggregateID_CanonicalString covers the happy path used by the
// StringConverter Debezium config — the key arrives as the canonical 36-char
// UUID string and must round-trip unchanged.
func TestDecodeAggregateID_CanonicalString(t *testing.T) {
	want := "785442b6-3da5-435f-a47b-aab5c702713f"
	got := decodeAggregateID([]byte(want))
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// TestDecodeAggregateID_BinaryUUID covers the path where Debezium emits the
// aggregate_id as the raw 16-byte UUID. Without normalization, Postgres
// rejects the value with SQLSTATE 22P02 because it sees a 16-byte string
// instead of a canonical UUID literal.
func TestDecodeAggregateID_BinaryUUID(t *testing.T) {
	u := uuid.MustParse("785442b6-3da5-435f-a47b-aab5c702713f")
	got := decodeAggregateID(u[:])
	want := u.String()
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// TestDecodeAggregateID_NonUUID covers the escape hatch for any future
// consumer that uses non-UUID aggregate IDs (e.g. opaque string keys).
// Lengths other than 16 fall through to a plain string conversion.
func TestDecodeAggregateID_NonUUID(t *testing.T) {
	cases := []string{"a", "some-non-uuid-key", "1234567890", ""}
	for _, c := range cases {
		if got := decodeAggregateID([]byte(c)); got != c {
			t.Fatalf("expected %q to pass through, got %q", c, got)
		}
	}
}

// TestBucketOf_DeterministicSameInput guarantees the per-aggregate ordering
// contract of the worker pool — the same aggregate_id MUST always land in the
// same bucket, otherwise concurrent workers could reorder updates for one
// aggregate and the Mongo view would diverge from the SQL state.
func TestBucketOf_DeterministicSameInput(t *testing.T) {
	id := "0a6e8189-5c92-4d22-900c-738674dbb081"
	want := bucketOf(id, 8)
	for i := 0; i < 100; i++ {
		if got := bucketOf(id, 8); got != want {
			t.Fatalf("bucketOf(%q, 8) drifted: got %d want %d", id, got, want)
		}
	}
}

// TestBucketOf_RangeBounded verifies the bucket always falls in [0, workers).
// Out-of-range would panic the worker dispatch.
func TestBucketOf_RangeBounded(t *testing.T) {
	ids := []string{
		"0a6e8189-5c92-4d22-900c-738674dbb081",
		"785442b6-3da5-435f-a47b-aab5c702713f",
		"f0e1d2c3-b4a5-6789-0abc-def123456789",
		"opaque-non-uuid-key",
		"",
	}
	for _, n := range []int{1, 2, 4, 8, 16} {
		for _, id := range ids {
			b := bucketOf(id, n)
			if b < 0 || b >= n {
				t.Fatalf("bucketOf(%q, %d) = %d out of range", id, n, b)
			}
		}
	}
}

// TestBucketOf_SingleWorkerShortcircuits documents the workers<=1 fast path:
// no hashing needed, every event goes to worker 0. Matches the pre-pool
// behavior so users that explicitly set syncWorkers: 1 in yaml get a
// zero-overhead serial consumer.
func TestBucketOf_SingleWorkerShortcircuits(t *testing.T) {
	for _, id := range []string{"a", "b", "c", "0a6e8189-5c92-4d22-900c-738674dbb081"} {
		if got := bucketOf(id, 1); got != 0 {
			t.Fatalf("bucketOf(%q, 1) = %d, want 0", id, got)
		}
		if got := bucketOf(id, 0); got != 0 {
			t.Fatalf("bucketOf(%q, 0) = %d, want 0 (clamp)", id, got)
		}
	}
}

// TestBucketOf_DistributesAcrossWorkers spot-checks that random UUIDs spread
// across the worker space — not statistically rigorous, just a smoke test
// that the hash isn't pathologically collapsing to one bucket.
func TestBucketOf_DistributesAcrossWorkers(t *testing.T) {
	const workers = 4
	seen := make(map[int]bool, workers)
	ids := []string{
		"0a6e8189-5c92-4d22-900c-738674dbb081",
		"785442b6-3da5-435f-a47b-aab5c702713f",
		"f0e1d2c3-b4a5-6789-0abc-def123456789",
		"11111111-2222-3333-4444-555555555555",
		"deadbeef-cafe-babe-feed-c0ffeec0ffee",
		"abcdef01-2345-6789-abcd-ef0123456789",
		"99999999-8888-7777-6666-555555555555",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	for _, id := range ids {
		seen[bucketOf(id, workers)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("hash collapsed: %d UUIDs all hashed to %v", len(ids), seen)
	}
}

// TestNewSyncEngine_WorkersFloor clamps workers<1 to 1 so callers that pass
// a missing yaml value (zero) don't silently end up with a deadlocked
// channel-less consumer.
func TestNewSyncEngine_WorkersFloor(t *testing.T) {
	for _, in := range []int{-1, 0} {
		s := NewSyncEngine(nil, nil, []string{"localhost:9092"}, "g", nil, in)
		if s.workers != 1 {
			t.Fatalf("NewSyncEngine workers=%d clamped to %d, want 1", in, s.workers)
		}
	}
	if s := NewSyncEngine(nil, nil, []string{"localhost:9092"}, "g", nil, 5); s.workers != 5 {
		t.Fatalf("NewSyncEngine workers=5 stored as %d", s.workers)
	}
}

// TestExtractEvent_BinaryKeyHeaders verifies the full path: a Kafka message
// with binary UUID key + Debezium-style headers produces a complete
// kafkaEvent with the canonical aggregate_id.
func TestExtractEvent_BinaryKeyHeaders(t *testing.T) {
	u := uuid.MustParse("0a6e8189-5c92-4d22-900c-738674dbb081")
	msg := kafka.Message{
		Key: u[:],
		Headers: []kafka.Header{
			{Key: "aggregate_type", Value: []byte("users")},
			{Key: "event_type", Value: []byte("INSERTED")},
		},
	}
	event := extractEvent(msg)
	if event.AggregateID != u.String() {
		t.Fatalf("expected aggregate_id %q, got %q", u.String(), event.AggregateID)
	}
	if event.AggregateType != "users" {
		t.Fatalf("expected aggregate_type %q, got %q", "users", event.AggregateType)
	}
	if event.EventType != "INSERTED" {
		t.Fatalf("expected event_type %q, got %q", "INSERTED", event.EventType)
	}
}

// TestTopicConfigsFor_Shape locks the topic creation shape (1 partition, RF=1)
// used by SyncEngine.ensureTopics. These defaults are deliberate: single-broker
// dev (docker-compose) requires RF=1; production deployments pre-create topics
// via ops tooling, so CreateTopics returns TopicAlreadyExists and the values
// here are ignored. Changing either default needs explicit broker-aware
// rationale.
func TestTopicConfigsFor_Shape(t *testing.T) {
	topics := []string{"users.events", "orders.events"}
	configs := topicConfigsFor(topics)
	if len(configs) != len(topics) {
		t.Fatalf("expected %d configs, got %d", len(topics), len(configs))
	}
	for i, c := range configs {
		if c.Topic != topics[i] {
			t.Errorf("config[%d].Topic = %q, want %q", i, c.Topic, topics[i])
		}
		if c.NumPartitions != defaultTopicNumPartitions {
			t.Errorf("config[%d].NumPartitions = %d, want %d", i, c.NumPartitions, defaultTopicNumPartitions)
		}
		if c.ReplicationFactor != defaultTopicReplicationFactor {
			t.Errorf("config[%d].ReplicationFactor = %d, want %d", i, c.ReplicationFactor, defaultTopicReplicationFactor)
		}
	}
}

// TestTopicConfigsFor_EmptyInput is the degenerate case — a service without
// any read-side views passes an empty topic slice and gets an empty config
// slice back, which ensureTopics then short-circuits on.
func TestTopicConfigsFor_EmptyInput(t *testing.T) {
	if got := topicConfigsFor(nil); len(got) != 0 {
		t.Fatalf("expected empty configs for nil input, got %d", len(got))
	}
	if got := topicConfigsFor([]string{}); len(got) != 0 {
		t.Fatalf("expected empty configs for empty input, got %d", len(got))
	}
}

// TestEnsureTopics_NoTopicsShortCircuits documents that a SyncEngine with no
// declared topics returns nil without dialing — write-only services (no
// ReadableFeature) must not require a reachable Kafka broker to boot.
func TestEnsureTopics_NoTopicsShortCircuits(t *testing.T) {
	s := &SyncEngine{topics: nil, brokers: []string{"127.0.0.1:1"}} // intentionally unreachable
	if err := s.ensureTopics(context.Background()); err != nil {
		t.Fatalf("expected nil error for empty topics, got %v", err)
	}
	s.topics = []string{}
	if err := s.ensureTopics(context.Background()); err != nil {
		t.Fatalf("expected nil error for empty topics slice, got %v", err)
	}
}

// TestEnsureTopics_NoBrokersFailsFast documents the operator-facing error
// when ensureTopics is called with no brokers configured — a config bug that
// should surface immediately, not via a 30s retry loop.
func TestEnsureTopics_NoBrokersFailsFast(t *testing.T) {
	// Shrink the timeout so the test fails fast even though the retry loop
	// would otherwise burn the full 30s budget.
	prev := ensureTopicsTimeout
	ensureTopicsTimeout = 50 * time.Millisecond
	defer func() { ensureTopicsTimeout = prev }()

	s := &SyncEngine{topics: []string{"users.events"}, brokers: nil}
	err := s.ensureTopics(context.Background())
	if err == nil {
		t.Fatal("expected error when no brokers are configured")
	}
}

// TestShouldDeleteFromView_DefaultKeep locks the canonical default for views
// that did NOT opt in to DeleteOnArchive: DELETED removes the document (hard
// delete is unconditional), every other event type — including ARCHIVED —
// hits the upsert path so the Mongo projection mirrors PostgreSQL
// symmetrically (archived rows survive with deleted_at populated; consumers
// read them via IncludeArchived=true, e.g. ?archived=true). UNARCHIVED and
// INSERTED/UPDATED stay on the upsert branch as before.
func TestShouldDeleteFromView_DefaultKeep(t *testing.T) {
	cases := []struct {
		eventType string
		want      bool
	}{
		{"DELETED", true},
		{"ARCHIVED", false},
		{"INSERTED", false},
		{"UPDATED", false},
		{"UNARCHIVED", false},
		{"SOMETHING_ELSE", false},
		{"", false},
	}
	for _, c := range cases {
		if got := shouldDeleteFromView(c.eventType, false); got != c.want {
			t.Errorf("shouldDeleteFromView(%q, false) = %v, want %v", c.eventType, got, c.want)
		}
	}
}

// TestShouldDeleteFromView_DeleteOnArchive is the opt-in hot-tier routing: a
// view that called .DeleteOnArchive() removes the document from Mongo on
// ARCHIVED (the explicit choice of keeping only active data on the read
// side). DELETED stays unconditional; INSERTED/UPDATED/UNARCHIVED stay on
// the upsert branch — the flag only flips ARCHIVED's routing.
func TestShouldDeleteFromView_DeleteOnArchive(t *testing.T) {
	cases := []struct {
		eventType string
		want      bool
	}{
		{"DELETED", true},
		{"ARCHIVED", true},
		{"INSERTED", false},
		{"UPDATED", false},
		{"UNARCHIVED", false},
		{"SOMETHING_ELSE", false},
		{"", false},
	}
	for _, c := range cases {
		if got := shouldDeleteFromView(c.eventType, true); got != c.want {
			t.Errorf("shouldDeleteFromView(%q, true) = %v, want %v", c.eventType, got, c.want)
		}
	}
}

// TestEnsureTopics_HonorsContextCancellation proves the retry loop unblocks
// when the caller cancels the context — important for clean shutdown when
// Kafka is permanently unreachable.
func TestEnsureTopics_HonorsContextCancellation(t *testing.T) {
	// Long timeout so cancellation is the only way to exit.
	prev := ensureTopicsTimeout
	ensureTopicsTimeout = 5 * time.Minute
	defer func() { ensureTopicsTimeout = prev }()

	// Unreachable address — every dial attempt fails, exercising the retry
	// loop. Use TEST-NET-1 (RFC 5737) which is reserved for documentation
	// and routed nowhere, so dial fails predictably without resolving DNS.
	s := &SyncEngine{topics: []string{"users.events"}, brokers: []string{"192.0.2.1:9092"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ensureTopics(ctx) }()

	// Give the goroutine a moment to enter the retry loop, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ensureTopics did not return after ctx cancellation")
	}
}
