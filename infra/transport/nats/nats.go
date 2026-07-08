//go:build nats

// Package nats is the NATS JetStream adapter for the transport seam. It is the
// ONLY place in the framework that imports the NATS client: a build without the
// `nats` tag links neither this package nor nats.go. It self-registers under the
// name "nats" in init(), mirroring the kafka adapter and the relational engines.
//
// The upstream relay (Debezium Server's NATS JetStream sink) publishes each
// outbox event to subject "<subjectPrefix>.<aggregate_type>.events" carrying the
// framework's headers (aggregate_type, event_type, traceparent, aggregate_id).
// This adapter reads them through a durable pull consumer and maps each into the
// neutral transport.Message — Key from the aggregate_id header (NATS has no Kafka
// key), Value from the payload, Headers from the NATS headers (JSON-unwrapped) —
// so the framework's consumers stay identical to the Kafka path.
//
// Durability matches Kafka: a FILE-backed stream (survives a broker restart),
// durable consumers (ack state survives a consumer restart, resuming from the
// last ack), explicit ack with redelivery of unacked messages, and limits
// retention so the read side can replay from the earliest offset.
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

const (
	// subjectPrefix is the leading token of every event subject. It MUST match
	// the Debezium sink's route.topic.replacement prefix. A stable prefix is
	// mandatory: NATS rejects a stream whose subjects begin with a wildcard.
	subjectPrefix = "omnicore"
	// streamName is the single JetStream stream every event subject lands in.
	streamName = "OMNICORE_EVENTS"
	// defaultAckWait bounds how long a delivered-but-unacked message waits
	// before JetStream redelivers it — the at-least-once redelivery window.
	defaultAckWait = 30 * time.Second
	// consumeBuffer bounds in-flight messages pulled ahead of Read. Sized to
	// absorb write bursts (the kafka-go reader has a large internal prefetch); a
	// tiny buffer would stall the JetStream Consume callback and throttle
	// delivery, letting a burst's projection fall behind the write path.
	consumeBuffer = 256
	// maxAckPending caps delivered-but-unacked messages. Kept well above
	// consumeBuffer so the delayed ack (mirroring the Kafka commit interval)
	// never trips JetStream's flow control and pauses delivery mid-burst.
	maxAckPending = 2048
)

func init() {
	transport.RegisterSubscriber("nats", New)
}

// New connects to NATS and opens the JetStream context. RetryOnFailedConnect
// lets boot tolerate NATS still coming up (docker-compose races), mirroring the
// SyncEngine's ensureTopics retry.
func New(cfg transport.Config) (transport.Subscriber, error) {
	url := nats.DefaultURL
	if len(cfg.Endpoints) > 0 {
		url = strings.Join(cfg.Endpoints, ",")
	}
	nc, err := nats.Connect(url,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats transport: connect %q: %w", url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats transport: jetstream: %w", err)
	}
	return &subscriber{nc: nc, js: js}, nil
}

type subscriber struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// EnsureTopics ensures the single events stream exists, file-backed for
// durability, capturing every "<prefix>.>" subject. Create-if-absent and
// idempotent: an already-present stream (e.g. created by the relay) is left
// as-is, and a concurrent create is absorbed.
func (s *subscriber) EnsureTopics(ctx context.Context, _ []transport.TopicSpec) error {
	if _, err := s.js.Stream(ctx, streamName); err == nil {
		return nil
	} else if !errors.Is(err, jetstream.ErrStreamNotFound) {
		return fmt.Errorf("nats transport: lookup stream %q: %w", streamName, err)
	}
	_, err := s.js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subjectPrefix + ".>"},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
	})
	if err != nil && !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		return fmt.Errorf("nats transport: create stream %q: %w", streamName, err)
	}
	return nil
}

// Subscribe opens a durable pull consumer filtered to the configured topics
// (mapped to their prefixed subjects) and starts consuming into an internal
// channel so Read can honor the caller's context. GroupID is the durable name —
// multiple instances sharing it share the work (the consumer-group analogue).
func (s *subscriber) Subscribe(ctx context.Context, cfg transport.SubscribeConfig) (transport.Subscription, error) {
	subjects := make([]string, len(cfg.Topics))
	for i, t := range cfg.Topics {
		subjects[i] = subjectPrefix + "." + t
	}
	deliver := jetstream.DeliverAllPolicy
	if cfg.StartFrom == transport.StartFromLatest {
		deliver = jetstream.DeliverNewPolicy
	}
	cons, err := s.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:        cfg.GroupID,
		FilterSubjects: subjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  deliver,
		AckWait:        defaultAckWait,
		MaxDeliver:     -1,
		MaxAckPending:  maxAckPending,
	})
	if err != nil {
		return nil, fmt.Errorf("nats transport: consumer %q: %w", cfg.GroupID, err)
	}
	sub := &subscription{
		ch:             make(chan jetstream.Msg, consumeBuffer),
		done:           make(chan struct{}),
		commitInterval: cfg.CommitInterval,
		pending:        map[*time.Timer]jetstream.Msg{},
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		select {
		case sub.ch <- m:
		case <-sub.done:
		}
	})
	if err != nil {
		return nil, fmt.Errorf("nats transport: consume %q: %w", cfg.GroupID, err)
	}
	sub.cc = cc
	return sub, nil
}

type subscription struct {
	cc             jetstream.ConsumeContext
	ch             chan jetstream.Msg
	done           chan struct{}
	commitInterval time.Duration

	mu       sync.Mutex
	closed   bool
	pending  map[*time.Timer]jetstream.Msg
	doneOnce sync.Once
}

// Read returns the next message, honoring ctx (unlike the raw JetStream
// iterator). The message is scheduled for a delayed ack that mirrors the Kafka
// path's async offset commit — ack fires ~commitInterval after the message is
// handed off, so a crash within that window leaves it unacked and JetStream
// redelivers it (at-least-once; idempotent handlers + the dedup table absorb it).
func (s *subscription) Read(ctx context.Context) (transport.Message, error) {
	select {
	case m := <-s.ch:
		s.scheduleAck(m)
		return toMessage(m), nil
	case <-ctx.Done():
		return transport.Message{}, ctx.Err()
	case <-s.done:
		return transport.Message{}, errors.New("nats transport: subscription closed")
	}
}

// scheduleAck acks the message after commitInterval (or immediately when the
// interval is zero), mirroring kafka-go's CommitInterval batching. Pending
// timers are tracked so Close can flush them.
func (s *subscription) scheduleAck(m jetstream.Msg) {
	if s.commitInterval <= 0 {
		_ = m.Ack()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = m.Ack()
		return
	}
	var t *time.Timer
	t = time.AfterFunc(s.commitInterval, func() {
		_ = m.Ack()
		s.mu.Lock()
		delete(s.pending, t)
		s.mu.Unlock()
	})
	s.pending[t] = m
}

// Close stops consuming and flushes pending acks. On graceful shutdown the
// framework drains its workers BEFORE closing the subscription, so every pending
// message is already processed — acking them here avoids needless redelivery on
// the next boot. Messages still in the internal channel (never Read) stay
// unacked and redeliver, as they must.
func (s *subscription) Close() error {
	s.doneOnce.Do(func() { close(s.done) })
	if s.cc != nil {
		s.cc.Stop()
	}
	s.mu.Lock()
	s.closed = true
	for t, m := range s.pending {
		if t.Stop() {
			_ = m.Ack()
		}
		delete(s.pending, t)
	}
	s.mu.Unlock()
	return nil
}

// toMessage maps a JetStream message into the neutral envelope: Key from the
// aggregate_id header (NATS has no Kafka key), Value from the payload, Headers
// from the NATS headers, and Topic from the subject with the prefix stripped.
func toMessage(m jetstream.Msg) transport.Message {
	h := m.Headers()
	return transport.Message{
		Topic:   strings.TrimPrefix(m.Subject(), subjectPrefix+"."),
		Key:     []byte(headerString(h, "aggregate_id")),
		Value:   m.Data(),
		Headers: flattenHeaders(h),
	}
}

// flattenHeaders converts NATS headers to the neutral map, JSON-unwrapping each
// value: the Debezium JSON header format serializes a string as a quoted JSON
// scalar (`"users"`), so the raw header bytes carry the quotes — decode them off
// so consumers see bare values, matching the Kafka SimpleHeaderConverter path.
func flattenHeaders(h nats.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k := range h {
		out[k] = unwrap(h.Get(k))
	}
	return out
}

func headerString(h nats.Header, key string) string { return unwrap(h.Get(key)) }

func unwrap(raw string) string {
	if raw == "" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err == nil {
		return s
	}
	return strings.Trim(raw, `"`)
}
