package infra

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"

	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

// defaultWorkerQueueDepth bounds in-flight messages per worker. With async
// commit (Phase 20.4), the at-least-once window after a crash equals roughly
// workers*queueDepth messages still in worker channels whose offsets may have
// already been async-committed. Kept small to limit that window while still
// absorbing short bursts from the Kafka reader.
const defaultWorkerQueueDepth = 4

// ensureTopicsTimeout caps how long SyncEngine waits for the broker to be
// reachable AND accept topic creation requests on startup. Tuned for
// docker-compose boot races where Debezium/Kafka may still be coming up when
// the service starts.
var ensureTopicsTimeout = 30 * time.Second

// defaultTopicNumPartitions / defaultTopicReplicationFactor are the values
// used when the SyncEngine has to create the topic itself. Production
// deployments typically pre-create topics via ops tooling with broker-aware
// partition counts — in that case CreateTopics returns TopicAlreadyExists and
// these values are ignored. Single-broker dev (docker-compose) needs RF=1.
const (
	defaultTopicNumPartitions     = 1
	defaultTopicReplicationFactor = 1
)

type SyncEngine struct {
	pg       *Postgres
	mongo    *MongoDB
	composer *Composer
	// index splits view lookup by source kind. SyncEngine reads byPGTable
	// (event.AggregateType → views whose root or PG embed match);
	// UpstreamSubscriber reads byMongoColl (subscription.Collection → views
	// embedding the upstream Mongo collection). The maps stay populated
	// during the lifetime of SyncEngine and are also handed to the
	// UpstreamSubscriber wiring at boot.
	index   viewIndex
	brokers []string
	groupID string
	topics  []string
	workers int
	// traceKafka gates the per-message consumer span (the tracing `kafka`
	// instrument toggle). bootstrap sets it via WithKafkaTracing; false (the
	// default) leaves the projection loop untraced and pays nothing.
	traceKafka bool
}

// WithKafkaTracing enables the consumer span on each processed message. bootstrap
// passes tracing.Instruments(SubKafka); off (the default) keeps the loop untraced.
func (s *SyncEngine) WithKafkaTracing(on bool) *SyncEngine {
	s.traceKafka = on
	return s
}

// kafkaEvent is reconstructed from a Kafka message produced by Debezium's
// Outbox Event Router (or any CDC tool that follows the same conventions):
//   - aggregate_id → message.Key
//   - aggregate_type, event_type → message.Headers
// The message Value is ignored — composer re-reads current state from Postgres.
type kafkaEvent struct {
	AggregateType string
	EventType     string
	AggregateID   string
	// Traceparent is the W3C trace context the producer stamped on the outbox
	// row, mapped to a Kafka header by Debezium's Outbox Event Router. Empty
	// when the producing write had tracing off. Used to LINK the projection
	// span back to the producing trace.
	Traceparent string
}

func extractEvent(msg kafka.Message) kafkaEvent {
	e := kafkaEvent{AggregateID: decodeAggregateID(msg.Key)}
	for _, h := range msg.Headers {
		switch h.Key {
		case "aggregate_type":
			e.AggregateType = string(h.Value)
		case "event_type":
			e.EventType = string(h.Value)
		case "traceparent":
			e.Traceparent = string(h.Value)
		}
	}
	return e
}

// decodeAggregateID normalizes the Kafka message Key to the canonical UUID
// string form expected by Postgres (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
// Debezium's Outbox Event Router emits aggregate_id either as a canonical
// string (StringConverter path) or as raw 16-byte binary UUID (some snapshot
// or replay paths use the native UUID type representation, ignoring the
// configured key converter). Without this normalization, the binary form
// would hit Postgres as a 16-byte string and fail with SQLSTATE 22P02
// ("invalid input syntax for type uuid"), breaking the read-side projection.
//
// Behavior:
//   - 16 bytes: parsed as binary UUID, formatted as canonical string
//   - Otherwise: returned verbatim (covers the string path and any non-UUID
//     aggregate IDs a future consumer might use)
func decodeAggregateID(key []byte) string {
	if len(key) == 16 {
		var u uuid.UUID
		copy(u[:], key)
		return u.String()
	}
	return string(key)
}

// NewSyncEngine wires the read-side projection consumer.
//
// workers controls how many goroutines process Kafka messages in parallel.
// Messages are routed to a worker via FNV-1a hash of the aggregate_id, so
// updates for the same aggregate always land on the same worker — preserving
// per-aggregate ordering. Across aggregates ordering is not promised, which
// matches Kafka's contract anyway. workers < 1 is clamped to 1.
func NewSyncEngine(pg *Postgres, mongo *MongoDB, brokers []string, groupID string, views []*ViewDefinition, workers int) *SyncEngine {
	topics := make([]string, 0, len(views))
	seen := map[string]bool{}
	for _, v := range views {
		t := topicFromTable(v.rootTable)
		if !seen[t] {
			topics = append(topics, t)
			seen[t] = true
		}
	}
	if workers < 1 {
		workers = 1
	}
	return &SyncEngine{
		pg:    pg,
		mongo: mongo,
		// NewComposerWithMongo so views embedding external FromSchema collections
		// resolve correctly through the composer during recompose.
		composer: NewComposerWithMongo(pg, mongo),
		index:    buildViewIndex(views),
		brokers:  brokers,
		groupID:  groupID,
		topics:   topics,
		workers:  workers,
	}
}

func (s *SyncEngine) Start(ctx context.Context) {
	go s.run(ctx)
}

func (s *SyncEngine) run(ctx context.Context) {
	// Topic ensurance is mandatory before the reader starts. kafka-go v0.4.51
	// with GroupTopics blocks forever in JoinGroup when a subscribed topic
	// doesn't exist in broker metadata at subscribe time — the coordinator
	// can't assign partitions and the reader doesn't refresh metadata to pick
	// the topic up later (e.g., when Debezium creates it by publishing the
	// first event). Pre-creating the topics here is the only way to give the
	// reader a non-empty assignment from the first JoinGroup.
	//
	// CreateTopics is idempotent at the kafka-go layer (TopicAlreadyExists is
	// silently treated as no-op), so calling it on every boot is safe even
	// when ops tooling or a previous run already provisioned the topics.
	if err := s.ensureTopics(ctx); err != nil {
		log.Printf("sync engine: ensure topics failed: %v", err)
		return
	}

	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     s.brokers,
		GroupID:     s.groupID,
		GroupTopics: s.topics,
		// CommitInterval > 0 switches kafka-go from commitLoopImmediate
		// (sync OffsetCommit RPC per message — caps throughput at ~9 msg/s
		// in local Docker because each commit roundtrip costs ~100 ms) to
		// commitLoopInterval (offsets batched and committed asynchronously
		// on a ticker). Safe under at-least-once: composer re-reads current
		// state from Postgres on each message and mongo.Upsert keyed by _id
		// is idempotent, so reprocessing the last <=1s window of messages
		// after a consumer crash converges to the same Mongo state.
		CommitInterval: time.Second,
	})
	defer r.Close()

	// Worker pool: each worker owns a channel; reader dispatches by FNV-1a
	// hash of aggregate_id. Same aggregate → same worker → ordering preserved.
	// Different aggregates parallelize. queueDepth bounded so the crash-loss
	// window stays small (see comment on defaultWorkerQueueDepth).
	queues := make([]chan kafkaEvent, s.workers)
	var wg sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		queues[i] = make(chan kafkaEvent, defaultWorkerQueueDepth)
		wg.Add(1)
		go func(q <-chan kafkaEvent) {
			defer wg.Done()
			for event := range q {
				if err := s.process(ctx, event); err != nil {
					log.Printf("sync engine: process error: %v", err)
				}
			}
		}(queues[i])
	}
	defer func() {
		for _, q := range queues {
			close(q)
		}
		wg.Wait()
	}()

	for {
		msg, err := r.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("sync engine: read error: %v", err)
			continue
		}
		event := extractEvent(msg)
		if event.AggregateID == "" || event.AggregateType == "" || event.EventType == "" {
			log.Printf("sync engine: incomplete metadata, skipping (topic=%s, key=%s, type=%q, eventType=%q)",
				msg.Topic, string(msg.Key), event.AggregateType, event.EventType)
			continue
		}
		// Blocking send: when a worker's queue is full, the reader stalls.
		// That's the desired backpressure — kafka-go's internal prefetch
		// buffer keeps absorbing while the reader waits, and the consumer
		// group heartbeat runs on a separate goroutine inside kafka-go so
		// the broker doesn't kick us out of the group.
		select {
		case queues[bucketOf(event.AggregateID, s.workers)] <- event:
		case <-ctx.Done():
			return
		}
	}
}

func (s *SyncEngine) process(ctx context.Context, event kafkaEvent) error {
	ctx, span := tracing.StartConsumerSpanIf(s.traceKafka, ctx,
		"github.com/ClaudioSchirmer/omnicore/infra/sync",
		"sync "+event.AggregateType, event.Traceparent)
	defer span.End()

	views, ok := s.index.byPGTable[event.AggregateType]
	if !ok {
		return nil
	}
	for _, view := range views {
		// DELETED always removes from the read side (hard delete, no flag
		// overrides it). ARCHIVED by default goes through the upsert branch
		// below — the composer keeps archived rows and lands the document
		// with deleted_at populated, so consumers that pass
		// IncludeArchived=true (e.g. ?includeArchived=true) can read it. Views that
		// opt in via ViewDefinition.DeleteOnArchive() instead remove the
		// document on ARCHIVED. An UNARCHIVED event always hits the upsert
		// branch regardless of the flag.
		if shouldDeleteFromView(event.EventType, view.deleteOnArchive) {
			if err := s.mongo.Delete(ctx, view.name, event.AggregateID); err != nil {
				return err
			}
			continue
		}
		doc, err := s.composer.Compose(ctx, view, event.AggregateID)
		if err != nil {
			return err
		}
		if doc == nil {
			continue
		}
		if err := s.mongo.Upsert(ctx, view.name, event.AggregateID, doc); err != nil {
			return err
		}
	}
	return nil
}

// shouldDeleteFromView is the routing decision for read-side events. DELETED
// is unconditional (hard delete = remove from Mongo regardless of the view's
// archive policy). ARCHIVED is conditional: by default the document survives
// in the projection (upsert path with deleted_at populated, symmetric with
// PostgreSQL) and the consumer reads it via the existing IncludeArchived
// flag; views that opt in via DeleteOnArchive() remove the document
// instead — the explicit hot-tier choice. Any other event type (INSERTED,
// UPDATED, UNARCHIVED) hits the upsert path.
func shouldDeleteFromView(eventType string, deleteOnArchive bool) bool {
	switch eventType {
	case "DELETED":
		return true
	case "ARCHIVED":
		return deleteOnArchive
	default:
		return false
	}
}

// bucketOf maps an aggregate_id to a worker index in [0, workers) via FNV-1a.
// Stable across runs (same id always picks same bucket) and well-distributed
// for UUID-shaped keys.
func bucketOf(aggregateID string, workers int) int {
	if workers <= 1 {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(aggregateID))
	return int(h.Sum32() % uint32(workers))
}

func topicFromTable(table string) string {
	return table + ".events"
}

// topicConfigsFor returns the CreateTopics config slice for the SyncEngine's
// declared topics. Extracted so tests can verify the shape without going
// through a Kafka connection.
func topicConfigsFor(topics []string) []kafka.TopicConfig {
	out := make([]kafka.TopicConfig, len(topics))
	for i, t := range topics {
		out[i] = kafka.TopicConfig{
			Topic:             t,
			NumPartitions:     defaultTopicNumPartitions,
			ReplicationFactor: defaultTopicReplicationFactor,
		}
	}
	return out
}

// ensureTopics dials any broker, finds the controller, and issues CreateTopics
// for every topic the SyncEngine intends to consume. Retries with linear
// backoff until ensureTopicsTimeout, after which the boot of the consumer
// goroutine is aborted with a logged error rather than left silently stuck.
//
// Idempotent at the kafka-go layer: pre-existing topics yield TopicAlreadyExists
// which is silently absorbed by Conn.CreateTopics (see kafka-go createtopics.go
// L415-L418), so calling this on every restart is safe.
func (s *SyncEngine) ensureTopics(ctx context.Context) error {
	if len(s.topics) == 0 {
		return nil
	}
	configs := topicConfigsFor(s.topics)
	deadline := time.Now().Add(ensureTopicsTimeout)
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("deadline exceeded")
			}
			return fmt.Errorf("sync engine: ensure topics %v: %w", s.topics, lastErr)
		}
		if err := createTopicsOnController(ctx, s.brokers, configs); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
}

// createTopicsOnController is the single-attempt body of ensureTopics: dial
// any broker, look up the controller, dial the controller, send CreateTopics.
// Closes both connections deterministically.
func createTopicsOnController(ctx context.Context, brokers []string, configs []kafka.TopicConfig) error {
	if len(brokers) == 0 {
		return errors.New("no brokers configured")
	}
	dialer := &kafka.Dialer{Timeout: 5 * time.Second, DualStack: true}
	conn, err := dialer.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("dial %s: %w", brokers[0], err)
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("lookup controller: %w", err)
	}
	controllerAddr := net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port))
	cConn, err := dialer.DialContext(ctx, "tcp", controllerAddr)
	if err != nil {
		return fmt.Errorf("dial controller %s: %w", controllerAddr, err)
	}
	defer cConn.Close()

	if err := cConn.CreateTopics(configs...); err != nil {
		return fmt.Errorf("create topics: %w", err)
	}
	return nil
}
