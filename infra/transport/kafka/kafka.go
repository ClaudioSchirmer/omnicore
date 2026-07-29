//go:build kafka

// Package kafka is the Kafka/Redpanda adapter for the transport seam. It is the
// ONLY place in the framework that imports segmentio/kafka-go: a build without
// the `kafka` tag links neither this package nor the client. Because Redpanda
// speaks the Kafka wire protocol, this single adapter backs both brokers — the
// choice between them is deployment configuration (the broker address), not code.
//
// It self-registers under the name "kafka" in init(), mirroring how each
// relational engine registers its dialect; bootstrap's tag-gated transport
// binding resolves it via transport.NewSubscriber.
//
// # Why the low-level consumer-group API
//
// This adapter drives kafka.ConsumerGroup + kafka.Generation directly instead of
// the convenience kafka.Reader. The reason is the transport port's Completion
// contract, which must survive a rebalance:
//
//   - Reader exposes no generation. A completed-offset tracker layered on top of
//     it would hold in-flight state across a rebalance without knowing one
//     happened, and could then commit an offset for a partition another consumer
//     had meanwhile processed — skipping those messages permanently.
//   - Generation.CommitOffsets is GENERATION-SCOPED. A commit issued from a
//     revoked generation carries its stale generation id and the group
//     coordinator REJECTS it. The broker itself enforces "a revoked partition
//     never has its offset committed afterwards"; this adapter does not have to
//     get that right by timing.
//
// Each generation therefore owns its own tracker, and a Completion belonging to
// a generation that has ended is dropped rather than applied.
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

func init() {
	transport.RegisterSubscriber("kafka", New)
}

const (
	// dialTimeout bounds the controller dial used by EnsureTopics.
	dialTimeout = 5 * time.Second
	// defaultCommitInterval is how often completed offsets are flushed when the
	// caller leaves SubscribeConfig.CommitInterval unset.
	defaultCommitInterval = time.Second
	// fetchBuffer bounds messages buffered across all partition readers of the
	// current generation before Read consumes them. It only smooths the hand-off;
	// a buffered message is already delivered and unconfirmed, so this is not a
	// durability knob.
	fetchBuffer = 256
)

// ErrAlreadyCompleted is returned by a second call to Done/Failed on the same
// Completion. The port's contract is exactly-one-outcome-per-message, so a
// double settle is a consumer bug; it is surfaced rather than absorbed, and the
// first outcome stands.
var ErrAlreadyCompleted = errors.New("kafka transport: message already completed")

// ErrGenerationEnded is returned when a Completion is settled after its
// generation was revoked. The outcome is intentionally DROPPED: the partition
// belongs to another consumer now, and the message will be redelivered from the
// last offset this group committed while it still owned the partition.
var ErrGenerationEnded = errors.New("kafka transport: generation ended before the message was completed")

// New builds the Kafka subscriber from the neutral transport config. Endpoints is
// the bootstrap-servers list (Kafka or Redpanda); a nil/empty list is accepted
// here and surfaces as a dial error at Subscribe/EnsureTopics time.
func New(cfg transport.Config) (transport.Subscriber, error) {
	return &subscriber{brokers: cfg.Endpoints}, nil
}

type subscriber struct {
	brokers []string
}

// Subscribe joins the consumer group and starts the generation loop. Messages
// from every assigned partition of the current generation funnel into one
// channel that Read drains.
func (s *subscriber) Subscribe(ctx context.Context, cfg transport.SubscribeConfig) (transport.Subscription, error) {
	startOffset := kafka.LastOffset
	if cfg.StartFrom == transport.StartFromEarliest {
		startOffset = kafka.FirstOffset
	}
	commitInterval := cfg.CommitInterval
	if commitInterval <= 0 {
		commitInterval = defaultCommitInterval
	}

	group, err := kafka.NewConsumerGroup(kafka.ConsumerGroupConfig{
		ID:          cfg.GroupID,
		Brokers:     s.brokers,
		Topics:      cfg.Topics,
		StartOffset: startOffset,
	})
	if err != nil {
		return nil, fmt.Errorf("kafka transport: consumer group %q: %w", cfg.GroupID, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	sub := &subscription{
		group:          group,
		brokers:        s.brokers,
		commitInterval: commitInterval,
		msgs:           make(chan fetched, fetchBuffer),
		done:           make(chan struct{}),
		cancel:         cancel,
	}
	go sub.run(runCtx)
	return sub, nil
}

// EnsureTopics dials any broker, finds the controller, and issues CreateTopics
// for every requested topic. Idempotent at the kafka-go layer: a pre-existing
// topic yields TopicAlreadyExists, which Conn.CreateTopics absorbs — so calling
// this on every boot is safe even when ops tooling pre-provisioned the topics.
// A nil/empty spec list is a no-op.
func (s *subscriber) EnsureTopics(ctx context.Context, topics []transport.TopicSpec) error {
	if len(topics) == 0 {
		return nil
	}
	if len(s.brokers) == 0 {
		return errors.New("kafka transport: no brokers configured")
	}
	configs := make([]kafka.TopicConfig, len(topics))
	for i, t := range topics {
		configs[i] = kafka.TopicConfig{
			Topic:             t.Name,
			NumPartitions:     t.NumPartitions,
			ReplicationFactor: t.ReplicationFactor,
		}
	}

	dialer := &kafka.Dialer{Timeout: dialTimeout, DualStack: true}
	conn, err := dialer.DialContext(ctx, "tcp", s.brokers[0])
	if err != nil {
		return fmt.Errorf("kafka transport: dial %s: %w", s.brokers[0], err)
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("kafka transport: lookup controller: %w", err)
	}
	controllerAddr := net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port))
	cConn, err := dialer.DialContext(ctx, "tcp", controllerAddr)
	if err != nil {
		return fmt.Errorf("kafka transport: dial controller %s: %w", controllerAddr, err)
	}
	defer cConn.Close()

	if err := cConn.CreateTopics(configs...); err != nil {
		return fmt.Errorf("kafka transport: create topics: %w", err)
	}
	return nil
}

// partKey identifies one assigned partition within a generation.
type partKey struct {
	topic     string
	partition int
}

// fetched is one delivered message plus the coordinates its outcome applies to.
type fetched struct {
	msg kafka.Message
	trk *genTracker
	key partKey
}

type subscription struct {
	group          *kafka.ConsumerGroup
	brokers        []string
	commitInterval time.Duration

	msgs   chan fetched
	done   chan struct{}
	cancel context.CancelFunc

	closeOnce sync.Once
}

// run is the generation loop. group.Next blocks until this member is assigned a
// generation, and blocks again on the next iteration until the current
// generation ends — so one pass of the loop is exactly one generation's life.
func (s *subscription) run(ctx context.Context) {
	defer close(s.done)
	for {
		gen, err := s.group.Next(ctx)
		if err != nil {
			return // ctx cancelled or the group was closed
		}
		trk := newGenTracker(gen)
		for topic, assignments := range gen.Assignments {
			for _, a := range assignments {
				topic, a := topic, a
				gen.Start(func(gctx context.Context) {
					s.readPartition(gctx, trk, topic, a)
				})
			}
		}
		// The commit loop runs under the generation too, so the generation does
		// not finish closing until the final flush has been attempted.
		gen.Start(func(gctx context.Context) {
			trk.commitLoop(gctx, s.commitInterval)
		})
	}
}

// readPartition streams one assigned partition. The reader carries NO GroupID —
// group membership is owned by the ConsumerGroup above — so ReadMessage here
// never commits anything on its own; offsets move only through the tracker.
func (s *subscription) readPartition(ctx context.Context, trk *genTracker, topic string, a kafka.PartitionAssignment) {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   s.brokers,
		Topic:     topic,
		Partition: a.ID,
	})
	defer r.Close()
	if err := r.SetOffset(a.Offset); err != nil {
		return
	}
	key := partKey{topic: topic, partition: a.ID}
	for {
		msg, err := r.ReadMessage(ctx)
		if err != nil {
			return // generation ended or subscription closed
		}
		trk.observe(key, msg.Offset)
		select {
		case s.msgs <- fetched{msg: msg, trk: trk, key: key}:
		case <-ctx.Done():
			return
		}
	}
}

// Read returns the next message together with the Completion that reports its
// outcome. The offset does not move until that outcome says Done.
func (s *subscription) Read(ctx context.Context) (transport.Message, transport.Completion, error) {
	select {
	case f := <-s.msgs:
		return toMessage(f.msg), &completion{trk: f.trk, key: f.key, offset: f.msg.Offset}, nil
	case <-ctx.Done():
		return transport.Message{}, nil, ctx.Err()
	case <-s.done:
		return transport.Message{}, nil, errors.New("kafka transport: subscription closed")
	}
}

// Close stops the generation loop, waits for it to unwind (so no partition
// reader outlives this call), and leaves the group.
func (s *subscription) Close() error {
	s.closeOnce.Do(func() { s.cancel() })
	<-s.done
	return s.group.Close()
}

// completion is the Kafka outcome handle.
//
// Done releases the offset to the tracker's contiguous-prefix computation.
// Failed does the OPPOSITE of releasing it: the offset is simply never
// completed, so the prefix stops at this message by construction and the group's
// committed offset never advances past it. That is the whole mechanism — Kafka
// has no negative acknowledgment and no in-session redelivery, so "not
// processed" can only be expressed as "not committed", and the message returns
// on the next rebalance or restart.
type completion struct {
	trk    *genTracker
	key    partKey
	offset int64
	once   sync.Once
}

func (c *completion) Done(context.Context) error {
	err := ErrAlreadyCompleted
	c.once.Do(func() { err = c.trk.complete(c.key, c.offset) })
	return err
}

func (c *completion) Failed(context.Context) error {
	err := ErrAlreadyCompleted
	c.once.Do(func() { err = nil })
	return err
}

// genTracker computes, per partition of ONE generation, the contiguous prefix of
// completed offsets that is safe to commit.
//
// A contiguous prefix is required because the consumer dispatches messages to
// workers by hash of the aggregate id, so one partition completes OUT OF ORDER
// by design. Committing the highest completed offset would silently confirm the
// gaps below it; committing the prefix confirms only what is genuinely finished.
type genTracker struct {
	gen *kafka.Generation

	mu     sync.Mutex
	closed bool
	dirty  bool
	parts  map[partKey]*partState
}

// partState tracks one partition. next is the lowest offset not yet completed —
// which is exactly the value Kafka expects as a committed offset ("the next
// offset to consume", cf. kafka-go's makeCommit, which commits msg.Offset+1).
type partState struct {
	next      int64
	started   bool
	completed map[int64]struct{}
}

func newGenTracker(gen *kafka.Generation) *genTracker {
	return &genTracker{gen: gen, parts: map[partKey]*partState{}}
}

// observe registers a delivered offset. Messages arrive in offset order within a
// partition, so the first one seen establishes the commit floor for this
// generation.
func (t *genTracker) observe(key partKey, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.parts[key]
	if st == nil {
		st = &partState{completed: map[int64]struct{}{}}
		t.parts[key] = st
	}
	if !st.started {
		st.next = offset
		st.started = true
	}
}

// complete releases one offset and advances the contiguous prefix as far as the
// completed set allows. A completion arriving after the generation ended is
// dropped — the partition is no longer ours.
func (t *genTracker) complete(key partKey, offset int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrGenerationEnded
	}
	st := t.parts[key]
	if st == nil || offset < st.next {
		return nil // unknown partition, or already covered by the prefix
	}
	st.completed[offset] = struct{}{}
	for {
		if _, ok := st.completed[st.next]; !ok {
			break
		}
		delete(st.completed, st.next)
		st.next++
		t.dirty = true
	}
	return nil
}

// commitLoop flushes the completed prefix on the interval and once more when the
// generation ends, then closes the tracker so late completions are dropped.
func (t *genTracker) commitLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.flush()
		case <-ctx.Done():
			// Final flush of work that genuinely finished before the generation
			// ended, so a rebalance does not force needless reprocessing. If the
			// generation is already revoked the coordinator rejects the commit on
			// its generation id, which is the safety property this adapter relies
			// on rather than on timing.
			t.flush()
			t.close()
			return
		}
	}
}

func (t *genTracker) flush() {
	t.mu.Lock()
	if t.closed || !t.dirty {
		t.mu.Unlock()
		return
	}
	offsets := make(map[string]map[int]int64, len(t.parts))
	for key, st := range t.parts {
		if !st.started {
			continue
		}
		if offsets[key.topic] == nil {
			offsets[key.topic] = map[int]int64{}
		}
		offsets[key.topic][key.partition] = st.next
	}
	t.dirty = false
	t.mu.Unlock()
	// Best-effort: a rejected commit (stale generation) leaves the offsets where
	// they were, so the messages redeliver. That is the safe direction.
	_ = t.gen.CommitOffsets(offsets)
}

func (t *genTracker) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

// toMessage translates a kafka-go message into the neutral envelope.
func toMessage(msg kafka.Message) transport.Message {
	return transport.Message{
		Topic:   msg.Topic,
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: flattenHeaders(msg.Headers),
	}
}

// flattenHeaders converts kafka-go's ordered header slice into the neutral map,
// LAST occurrence winning on a duplicate key — kafka-go preserves producer
// order, so this matches the on-the-wire intent. Each value is JSON-unwrapped so
// the adapter is decoupled from the relay's header format: Debezium Server's
// Kafka sink serializes a string as a quoted JSON scalar (`"users"`), while the
// classic SimpleHeaderConverter (Kafka Connect) writes it bare — unwrap
// normalizes both to the bare value the consumers expect. Idempotent for bare
// strings, so it never regresses the Connect/Redpanda path.
func flattenHeaders(headers []kafka.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		out[h.Key] = unwrap(string(h.Value))
	}
	return out
}

// unwrap decodes a JSON-encoded scalar string to its bare value, falling back to
// trimming surrounding quotes; a value that is not JSON (a bare string) passes
// through unchanged.
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
