//go:build nats

package nats

import (
	"context"
	"errors"
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
	acks     int32
	naks     int32
	inflight int32
	data     []byte
	hdr      nats.Header
	subj     string
}

func (f *fakeMsg) Ack() error           { atomic.AddInt32(&f.acks, 1); return nil }
func (f *fakeMsg) Nak() error           { atomic.AddInt32(&f.naks, 1); return nil }
func (f *fakeMsg) InProgress() error    { atomic.AddInt32(&f.inflight, 1); return nil }
func (f *fakeMsg) Data() []byte         { return f.data }
func (f *fakeMsg) Headers() nats.Header { return f.hdr }
func (f *fakeMsg) Subject() string      { return f.subj }
func (f *fakeMsg) ackCount() int32      { return atomic.LoadInt32(&f.acks) }
func (f *fakeMsg) nakCount() int32      { return atomic.LoadInt32(&f.naks) }
func (f *fakeMsg) progressCount() int32 { return atomic.LoadInt32(&f.inflight) }

// newSub builds a subscription with no live consumer (cc == nil, which Close
// tolerates), so the completion machinery is exercised in isolation.
func newSub() *subscription {
	return &subscription{
		ch:      make(chan jetstream.Msg, 4),
		done:    make(chan struct{}),
		pending: map[*completion]struct{}{},
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

// --- Completion contract ------------------------------------------------
//
// These replace the previous delayed-ack tests. The timer-ack model they
// covered is gone: it acked a message ~commitInterval after HAND-OFF, i.e.
// before the consumer had processed it, so a crash in that window lost the
// message. Confirmation is now the consumer's explicit outcome.

// TestRead_DoesNotConfirmOnItsOwn is the crux of the whole change: taking a
// message off the stream must NOT confirm it. Before, Read scheduled an ack.
func TestRead_DoesNotConfirmOnItsOwn(t *testing.T) {
	sub := newSub()
	m := &fakeMsg{
		data: []byte("payload"),
		subj: subjectPrefix + ".users.events",
		hdr:  nats.Header{"aggregate_id": {`"k1"`}},
	}
	sub.ch <- m
	got, completion, err := sub.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got.Key) != "k1" || got.Topic != "users.events" {
		t.Fatalf("Read mapped wrong message: %+v", got)
	}
	if completion == nil {
		t.Fatal("Read must return a Completion")
	}
	if m.ackCount() != 0 || m.nakCount() != 0 {
		t.Fatalf("Read must not confirm the message, acks=%d naks=%d", m.ackCount(), m.nakCount())
	}
}

// TestCompletion_DoneAcksExactlyOnce: Done acks, and a second outcome is
// refused rather than silently double-acking.
func TestCompletion_DoneAcksExactlyOnce(t *testing.T) {
	sub := newSub()
	m := &fakeMsg{}
	sub.ch <- m
	_, completion, _ := sub.Read(context.Background())

	if err := completion.Done(context.Background()); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if m.ackCount() != 1 {
		t.Fatalf("Done must ack once, acks=%d", m.ackCount())
	}
	if err := completion.Done(context.Background()); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("second Done = %v, want ErrAlreadyCompleted", err)
	}
	if err := completion.Failed(context.Background()); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("Failed after Done = %v, want ErrAlreadyCompleted", err)
	}
	if m.ackCount() != 1 || m.nakCount() != 0 {
		t.Fatalf("first outcome must stand, acks=%d naks=%d", m.ackCount(), m.nakCount())
	}
	// A settled completion is no longer tracked, so Close has no heartbeat left.
	sub.mu.Lock()
	n := len(sub.pending)
	sub.mu.Unlock()
	if n != 0 {
		t.Fatalf("settled completion must be forgotten, pending=%d", n)
	}
}

// TestCompletion_FailedNaks: Failed asks for redelivery via Nak, which is
// immediate — withholding the ack instead would cost the full AckWait.
func TestCompletion_FailedNaks(t *testing.T) {
	sub := newSub()
	m := &fakeMsg{}
	sub.ch <- m
	_, completion, _ := sub.Read(context.Background())

	if err := completion.Failed(context.Background()); err != nil {
		t.Fatalf("Failed: %v", err)
	}
	if m.nakCount() != 1 {
		t.Fatalf("Failed must nak once, naks=%d", m.nakCount())
	}
	if m.ackCount() != 0 {
		t.Fatalf("Failed must never ack, acks=%d", m.ackCount())
	}
}

// TestCompletion_HeartbeatExtendsAckDeadline pins the JetStream-specific
// requirement Kafka has no analogue for: while an outcome is pending the
// message must keep extending its ack lease, or AckWait expires and JetStream
// redelivers a message that is still being processed — racing it against
// itself. Driven at a test interval; production uses ackHeartbeat.
func TestCompletion_HeartbeatExtendsAckDeadline(t *testing.T) {
	m := &fakeMsg{}
	c := &completion{msg: m, stop: make(chan struct{}), sub: newSub()}
	go c.heartbeat(time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for m.progressCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat did not extend the deadline within 2s, InProgress=%d", m.progressCount())
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.Done(context.Background()); err != nil {
		t.Fatalf("Done: %v", err)
	}
	// Settling stops the heartbeat: the count must go quiet.
	settled := m.progressCount()
	time.Sleep(20 * time.Millisecond)
	if got := m.progressCount(); got != settled {
		t.Fatalf("heartbeat kept running after the outcome (%d -> %d)", settled, got)
	}
}

// TestClose_ConfirmsNothing: Close must never ack. The old model flushed
// pending acks here, which confirmed work whose outcome nobody had reported.
// Dropping the heartbeat instead lets AckWait expire and JetStream redeliver —
// the safe direction.
func TestClose_ConfirmsNothing(t *testing.T) {
	sub := newSub()
	m := &fakeMsg{}
	sub.ch <- m
	_, _, _ = sub.Read(context.Background())

	sub.mu.Lock()
	pending := len(sub.pending)
	sub.mu.Unlock()
	if pending != 1 {
		t.Fatalf("an unsettled completion must be tracked, pending=%d", pending)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if m.ackCount() != 0 || m.nakCount() != 0 {
		t.Fatalf("Close must confirm nothing, acks=%d naks=%d", m.ackCount(), m.nakCount())
	}
	sub.mu.Lock()
	pending = len(sub.pending)
	sub.mu.Unlock()
	if pending != 0 {
		t.Fatalf("Close must stop every heartbeat, pending=%d", pending)
	}
	// Close is idempotent (doneOnce guards the done close).
	if err := sub.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestRead_HonorsContextCancel: Read must return ctx.Err() when the context is
// cancelled with no message available (the JetStream iterator alone can't do
// this — the channel indirection is what buys cancellation).
func TestRead_HonorsContextCancel(t *testing.T) {
	sub := newSub()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := sub.Read(ctx); err != context.Canceled {
		t.Fatalf("Read = %v, want context.Canceled", err)
	}
}

// TestRead_AfterCloseErrors: Read on a closed subscription returns an error
// rather than blocking forever.
func TestRead_AfterCloseErrors(t *testing.T) {
	sub := newSub()
	_ = sub.Close()
	if _, _, err := sub.Read(context.Background()); err == nil {
		t.Fatal("Read on a closed subscription must error")
	}
}
