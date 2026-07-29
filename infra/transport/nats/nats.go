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
	// consumeBuffer so in-flight processing (a message is unacked from Read
	// until its Completion settles) never trips JetStream's flow control and
	// pauses delivery mid-burst.
	maxAckPending = 2048
	// ackHeartbeat is how often a delivered-but-unsettled message extends its
	// ack deadline via InProgress. Half of defaultAckWait, so two consecutive
	// missed beats are needed before JetStream considers the message abandoned.
	//
	// This exists because AckWait is a LEASE, and the consumer's retry budget
	// can legitimately outlive it: a message retried with backoff would be
	// redelivered by JetStream while the first attempt is still running,
	// racing itself. Kafka has no analogue — it has no per-message lease, and
	// kafka-go's session heartbeat covers the group-level equivalent.
	ackHeartbeat = defaultAckWait / 2
)

// ErrAlreadyCompleted is returned by a second call to Done/Failed on the same
// Completion. The port's contract is exactly-one-outcome-per-message, so a
// double settle is a consumer bug; it is surfaced rather than absorbed, and the
// first outcome stands.
var ErrAlreadyCompleted = errors.New("nats transport: message already completed")

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

// deliverPolicyFor maps the neutral StartFrom to a JetStream DeliverPolicy for a
// FRESH durable (no stored ack state); on restart the durable resumes from its
// last ack regardless of this. It mirrors the transport contract and the kafka
// adapter: StartFromEarliest replays the retained log (DeliverAll), while
// everything else — StartFromLatest AND the empty default — begins at new
// messages (DeliverNew). Aligning the empty case to "latest" is what keeps a
// caller that omits StartFrom behaving identically on both transports (kafka-go's
// unset StartOffset defaults to LastOffset); see SubscribeConfig.StartFrom.
func deliverPolicyFor(startFrom string) jetstream.DeliverPolicy {
	if startFrom == transport.StartFromEarliest {
		return jetstream.DeliverAllPolicy
	}
	return jetstream.DeliverNewPolicy
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
	cons, err := s.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:        cfg.GroupID,
		FilterSubjects: subjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  deliverPolicyFor(cfg.StartFrom),
		AckWait:        defaultAckWait,
		MaxDeliver:     -1,
		MaxAckPending:  maxAckPending,
	})
	if err != nil {
		return nil, fmt.Errorf("nats transport: consumer %q: %w", cfg.GroupID, err)
	}
	// cfg.CommitInterval is deliberately unused here. It bounds how often an
	// adapter FLUSHES completed positions, which is a Kafka concern: JetStream
	// confirms per message, so an outcome is reported the instant it is known
	// and there is nothing to batch.
	sub := &subscription{
		ch:      make(chan jetstream.Msg, consumeBuffer),
		done:    make(chan struct{}),
		pending: map[*completion]struct{}{},
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
	cc   jetstream.ConsumeContext
	ch   chan jetstream.Msg
	done chan struct{}

	mu       sync.Mutex
	closed   bool
	pending  map[*completion]struct{}
	doneOnce sync.Once
}

// Read returns the next message, honoring ctx (unlike the raw JetStream
// iterator), together with the Completion that reports its outcome. The message
// stays UNACKED from here until that outcome is settled, so a crash mid-
// processing leaves it for redelivery — the at-least-once property the whole
// read side's correctness argument rests on.
func (s *subscription) Read(ctx context.Context) (transport.Message, transport.Completion, error) {
	select {
	case m := <-s.ch:
		return toMessage(m), s.track(m), nil
	case <-ctx.Done():
		return transport.Message{}, nil, ctx.Err()
	case <-s.done:
		return transport.Message{}, nil, errors.New("nats transport: subscription closed")
	}
}

// track builds the message's completion handle and registers it so Close can
// stop its heartbeat. A subscription already closed yields an unregistered
// handle: it still settles correctly, it simply has no heartbeat to stop.
func (s *subscription) track(m jetstream.Msg) *completion {
	c := &completion{msg: m, stop: make(chan struct{}), sub: s}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return c
	}
	s.pending[c] = struct{}{}
	go c.heartbeat(ackHeartbeat)
	return c
}

func (s *subscription) forget(c *completion) {
	s.mu.Lock()
	delete(s.pending, c)
	s.mu.Unlock()
}

// completion is the JetStream outcome handle. Confirmation is per message here,
// so the mapping is direct: Done acks, Failed NAKs. Nak is deliberate over
// simply withholding the ack — a nak redelivers immediately, where silence
// costs the full AckWait (30s) before JetStream gives up on the delivery.
type completion struct {
	msg  jetstream.Msg
	sub  *subscription
	stop chan struct{}
	once sync.Once
}

// heartbeat extends the message's ack deadline every `every` while its outcome
// is pending. See ackHeartbeat for why this is mandatory on JetStream and
// absent on Kafka.
func (c *completion) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = c.msg.InProgress()
		case <-c.stop:
			return
		}
	}
}

func (c *completion) Done(context.Context) error   { return c.settle(c.msg.Ack) }
func (c *completion) Failed(context.Context) error { return c.settle(c.msg.Nak) }

// settle stops the heartbeat and reports the outcome exactly once. A second
// call answers ErrAlreadyCompleted and changes nothing.
func (c *completion) settle(report func() error) error {
	err := ErrAlreadyCompleted
	c.once.Do(func() {
		close(c.stop)
		c.sub.forget(c)
		err = report()
	})
	return err
}

// Close stops consuming and stops every pending heartbeat. It deliberately acks
// NOTHING: an outcome is the consumer's to report, and acking here would
// confirm work that may never have happened. The framework drains its workers —
// settling every in-flight Completion — BEFORE closing the subscription, so a
// pending handle at this point is a leak; dropping its heartbeat lets AckWait
// expire and JetStream redeliver, which is the safe direction. Messages still
// in the internal channel (never Read) stay unacked and redeliver, as they must.
func (s *subscription) Close() error {
	s.doneOnce.Do(func() { close(s.done) })
	if s.cc != nil {
		s.cc.Stop()
	}
	s.mu.Lock()
	s.closed = true
	for c := range s.pending {
		c.once.Do(func() { close(c.stop) })
		delete(s.pending, c)
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
