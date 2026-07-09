//go:build nats

package nats

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// fakeMsg is a hermetic jetstream.Msg: it needs no broker. Embedding the
// interface satisfies the full method set; only the methods the adapter touches
// are overridden (any un-overridden call would panic, flagging an unexpected
// dependency). Ack is counted atomically so tests can assert exactly-once.
type fakeMsg struct {
	jetstream.Msg
	acks int32
	data []byte
	hdr  nats.Header
	subj string
}

func (f *fakeMsg) Ack() error           { atomic.AddInt32(&f.acks, 1); return nil }
func (f *fakeMsg) Data() []byte         { return f.data }
func (f *fakeMsg) Headers() nats.Header { return f.hdr }
func (f *fakeMsg) Subject() string      { return f.subj }
func (f *fakeMsg) ackCount() int32      { return atomic.LoadInt32(&f.acks) }

// newSub builds a subscription with no live consumer (cc == nil, which Close
// tolerates), so the delayed-ack machinery is exercised in isolation.
func newSub(commit time.Duration) *subscription {
	return &subscription{
		ch:             make(chan jetstream.Msg, 4),
		done:           make(chan struct{}),
		commitInterval: commit,
		pending:        map[*time.Timer]jetstream.Msg{},
	}
}

func TestUnwrap(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"users"`, "users"}, // JSON-quoted string (Debezium json header format)
		{"users", "users"},   // bare (not valid JSON) → trimmed passthrough
		{`""`, ""},           // empty JSON string
		{"", ""},             // empty
		{`"00-abc-def-01"`, "00-abc-def-01"},
	}
	for _, c := range cases {
		if got := unwrap(c.in); got != c.want {
			t.Errorf("unwrap(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFlattenHeadersAndHeaderString(t *testing.T) {
	h := nats.Header{
		"aggregate_type": {`"users"`},
		"event_type":     {`"INSERTED"`},
		"traceparent":    {`"00-abc-def-01"`},
		"aggregate_id":   {`"f5aaa9e7-17e1-41d3-8f9d-056d2d30f4a3"`},
	}
	out := flattenHeaders(h)
	if out["aggregate_type"] != "users" {
		t.Errorf("aggregate_type = %q, want users", out["aggregate_type"])
	}
	if out["event_type"] != "INSERTED" {
		t.Errorf("event_type = %q, want INSERTED", out["event_type"])
	}
	if out["aggregate_id"] != "f5aaa9e7-17e1-41d3-8f9d-056d2d30f4a3" {
		t.Errorf("aggregate_id = %q", out["aggregate_id"])
	}
	if headerString(h, "traceparent") != "00-abc-def-01" {
		t.Errorf("headerString(traceparent) = %q", headerString(h, "traceparent"))
	}
	if headerString(h, "absent") != "" {
		t.Errorf("absent header must be empty, got %q", headerString(h, "absent"))
	}
}

// TestDeliverPolicyFor pins the StartFrom → DeliverPolicy contract, including the
// empty default: an omitted StartFrom must mean "latest" (DeliverNew) so a caller
// that leaves it unset behaves the same on NATS as on Kafka (whose unset
// StartOffset is LastOffset). Only StartFromEarliest replays the log.
func TestDeliverPolicyFor(t *testing.T) {
	cases := []struct {
		in   string
		want jetstream.DeliverPolicy
	}{
		{transport.StartFromEarliest, jetstream.DeliverAllPolicy},
		{transport.StartFromLatest, jetstream.DeliverNewPolicy},
		{"", jetstream.DeliverNewPolicy},        // empty default = latest (contract)
		{"garbage", jetstream.DeliverNewPolicy}, // unknown ≠ earliest → latest
	}
	for _, c := range cases {
		if got := deliverPolicyFor(c.in); got != c.want {
			t.Errorf("deliverPolicyFor(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestToMessage checks the JetStream→neutral mapping: Key comes from the
// aggregate_id header (NATS has no Kafka key), Topic is the subject with the
// stream prefix stripped, headers are JSON-unwrapped.
func TestToMessage(t *testing.T) {
	m := &fakeMsg{
		data: []byte(`{"id":1}`),
		subj: subjectPrefix + ".users.events",
		hdr: nats.Header{
			"aggregate_id":   {`"abc-123"`},
			"aggregate_type": {`"users"`},
			"event_type":     {`"INSERTED"`},
		},
	}
	msg := toMessage(m)
	if msg.Topic != "users.events" {
		t.Errorf("Topic = %q, want users.events (prefix stripped)", msg.Topic)
	}
	if string(msg.Key) != "abc-123" {
		t.Errorf("Key = %q, want abc-123 (from aggregate_id header)", msg.Key)
	}
	if string(msg.Value) != `{"id":1}` {
		t.Errorf("Value = %q", msg.Value)
	}
	if msg.Headers["event_type"] != "INSERTED" {
		t.Errorf("Headers[event_type] = %q, want INSERTED", msg.Headers["event_type"])
	}
}

// TestScheduleAck_ImmediateWhenZeroInterval: a zero commit interval acks inline
// (mirrors kafka-go committing synchronously) — nothing is left pending.
func TestScheduleAck_ImmediateWhenZeroInterval(t *testing.T) {
	sub := newSub(0)
	m := &fakeMsg{}
	sub.scheduleAck(m)
	if m.ackCount() != 1 {
		t.Fatalf("zero interval must ack immediately, acks=%d", m.ackCount())
	}
	if len(sub.pending) != 0 {
		t.Fatalf("zero interval must leave nothing pending, pending=%d", len(sub.pending))
	}
}

// TestScheduleAck_WhenClosedAcksInline: after Close set closed, a late
// scheduleAck must ack immediately rather than register a timer that would
// outlive the subscription.
func TestScheduleAck_WhenClosedAcksInline(t *testing.T) {
	sub := newSub(time.Hour)
	sub.closed = true
	m := &fakeMsg{}
	sub.scheduleAck(m)
	if m.ackCount() != 1 {
		t.Fatalf("closed subscription must ack inline, acks=%d", m.ackCount())
	}
	if len(sub.pending) != 0 {
		t.Fatalf("closed subscription must not register a timer, pending=%d", len(sub.pending))
	}
}

// TestCloseFlushesPendingAcksExactlyOnce: with a long interval the timer never
// fires on its own, so Close's t.Stop() path deterministically flushes the
// pending ack — exactly once, and pending is cleared. This is the graceful-drain
// contract (workers finished before Close, so acking avoids needless redelivery).
func TestCloseFlushesPendingAcksExactlyOnce(t *testing.T) {
	sub := newSub(time.Hour)
	m := &fakeMsg{}
	sub.scheduleAck(m)
	if m.ackCount() != 0 {
		t.Fatalf("deferred ack must not fire before the interval, acks=%d", m.ackCount())
	}
	if len(sub.pending) != 1 {
		t.Fatalf("deferred ack must be tracked as pending, pending=%d", len(sub.pending))
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if m.ackCount() != 1 {
		t.Fatalf("Close must flush the pending ack exactly once, acks=%d", m.ackCount())
	}
	if len(sub.pending) != 0 {
		t.Fatalf("Close must clear pending, pending=%d", len(sub.pending))
	}
	// Close is idempotent (doneOnce guards the done close); a second call must
	// not panic or double-ack.
	if err := sub.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if m.ackCount() != 1 {
		t.Fatalf("second Close must not re-ack, acks=%d", m.ackCount())
	}
}

// TestTimerFiresAcksOnce_NoDoubleAckOnClose: once the timer fires and acks, a
// following Close (whose t.Stop() returns false) must not ack again — the crux
// of the timer/Close race the mutex guards.
func TestTimerFiresAcksOnce_NoDoubleAckOnClose(t *testing.T) {
	sub := newSub(20 * time.Millisecond)
	m := &fakeMsg{}
	sub.scheduleAck(m)

	deadline := time.Now().Add(2 * time.Second)
	for m.ackCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timer did not fire within 2s")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if m.ackCount() != 1 {
		t.Fatalf("timer must ack exactly once, acks=%d", m.ackCount())
	}
	// The fired timer must have removed itself from pending.
	sub.mu.Lock()
	n := len(sub.pending)
	sub.mu.Unlock()
	if n != 0 {
		t.Fatalf("fired timer must remove itself from pending, pending=%d", n)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if m.ackCount() != 1 {
		t.Fatalf("Close after timer fired must not double-ack, acks=%d", m.ackCount())
	}
}

// TestRead_DeliversAndSchedulesAck: a message on the channel is returned mapped
// and acked (zero interval → inline ack).
func TestRead_DeliversAndSchedulesAck(t *testing.T) {
	sub := newSub(0)
	m := &fakeMsg{
		data: []byte("payload"),
		subj: subjectPrefix + ".users.events",
		hdr:  nats.Header{"aggregate_id": {`"k1"`}},
	}
	sub.ch <- m
	got, err := sub.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got.Key) != "k1" || got.Topic != "users.events" {
		t.Fatalf("Read mapped wrong message: %+v", got)
	}
	if m.ackCount() != 1 {
		t.Fatalf("Read with zero interval must ack, acks=%d", m.ackCount())
	}
}

// TestRead_HonorsContextCancel: Read must return ctx.Err() when the context is
// cancelled with no message available (the JetStream iterator alone can't do
// this — the channel indirection is what buys cancellation).
func TestRead_HonorsContextCancel(t *testing.T) {
	sub := newSub(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sub.Read(ctx); err != context.Canceled {
		t.Fatalf("Read = %v, want context.Canceled", err)
	}
}

// TestRead_AfterCloseErrors: Read on a closed subscription returns an error
// rather than blocking forever.
func TestRead_AfterCloseErrors(t *testing.T) {
	sub := newSub(0)
	_ = sub.Close()
	if _, err := sub.Read(context.Background()); err == nil {
		t.Fatal("Read on a closed subscription must error")
	}
}
