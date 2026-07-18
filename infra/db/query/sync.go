package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
	"github.com/ClaudioSchirmer/omnicore/infra/transport"
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
	// eng is the neutral relational engine the composer reads through; the
	// rebuild control plane (advisory lock, omnicore_mongo_views registry) is
	// backend-neutral too — it serializes on eng.AcquireRebuildLock (PG
	// pg_advisory_lock, MySQL GET_LOCK) and reads/writes the registry via
	// eng.Querier()/eng.Dialect(), so it runs on any relational backend.
	eng   core.RelationalEngine
	mongo ReadModelStore
	// resolver maps a view name to the physical collection it currently
	// resolves to (its active slot). Shared process-wide so every read-model
	// component observes one consistent pointer view.
	resolver *ViewResolver
	composer *Composer
	// index splits view lookup by source kind. SyncEngine reads byPGTable
	// (event.AggregateType → views whose root or PG embed match);
	// UpstreamSubscriber reads byMongoColl (subscription.Collection → views
	// embedding the upstream Mongo collection). The maps stay populated
	// during the lifetime of SyncEngine and are also handed to the
	// UpstreamSubscriber wiring at boot.
	index viewIndex
	// sub is the transport port the projection loop opens its consumer
	// subscription through, and ensures topics exist through — the seam that
	// keeps this loop broker-neutral.
	sub     transport.Subscriber
	groupID string
	topics  []string
	workers int
	// traceKafka gates the per-message consumer span (the tracing `kafka`
	// instrument toggle). bootstrap sets it via WithKafkaTracing; false (the
	// default) leaves the projection loop untraced and pays nothing.
	traceKafka bool

	// done closes when the projection loop has FULLY exited — worker drain
	// (every in-flight compose+upsert finished) and reader Close() (the Kafka
	// LeaveGroup) included, in that dependency order. Shutdown waits on it so
	// the process never exits with an in-flight projection racing the store
	// closes, nor with a ghost member still holding the consumer-group slot.
	done      chan struct{}
	startOnce sync.Once
	started   atomic.Bool
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
//   - the outbox payload JSON → message.Value
//
// The message Value is never a STATE source — the composer re-reads current
// state from the relational backend. It is carried as a ROUTING HINT for
// exactly one case: a role DELETED under the separate-FK SharedBase model,
// where the row is gone and nothing is left to consult — the write side
// records the structural keys (the role PK + the shared-base FK) in the
// DELETED payload precisely so the base-rooted recompose can find its
// document (resolveBaseID).
type kafkaEvent struct {
	AggregateType string
	EventType     string
	AggregateID   string
	// Traceparent is the W3C trace context the producer stamped on the outbox
	// row, mapped to a Kafka header by Debezium's Outbox Event Router. Empty
	// when the producing write had tracing off. Used to LINK the projection
	// span back to the producing trace.
	Traceparent string
	// Payload is the raw outbox payload (message.Value) — a routing hint only
	// (see the type comment), parsed lazily and exclusively on the role-DELETED
	// branch of the base-rooted recompose.
	Payload []byte
}

func extractEvent(msg transport.Message) kafkaEvent {
	return kafkaEvent{
		AggregateID:   decodeAggregateID(msg.Key),
		Payload:       msg.Value,
		AggregateType: msg.Headers["aggregate_type"],
		EventType:     msg.Headers["event_type"],
		Traceparent:   msg.Headers["traceparent"],
	}
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
func NewSyncEngine(eng core.RelationalEngine, mongo ReadModelStore, resolver *ViewResolver, sub transport.Subscriber, groupID string, views []*ViewDefinition, workers int) *SyncEngine {
	topics := make([]string, 0, len(views))
	seen := map[string]bool{}
	addTopic := func(table string) {
		t := topicFromTable(table)
		if !seen[t] {
			topics = append(topics, t)
			seen[t] = true
		}
	}
	for _, v := range views {
		addTopic(v.rootTable)
		// Subscribe to a role view's SharedBase topic too, so a base change
		// (emitted as an aggregate_type=base outbox row) reaches the fan-out.
		if v.schema != nil {
			if base, _, ok := v.schema.SharedBaseRef(); ok {
				addTopic(base.Table())
			}
		}
		// A SharedBaseView subscribes to every ROLE table's topic too: role
		// ARCHIVE/UNARCHIVE emits only the role event (the base convergence is
		// SQL without its own outbox row), so without these topics a person
		// document would never learn a role's lifecycle change — mandatory even
		// when the service declares no per-role view.
		for _, r := range v.roles {
			addTopic(r.schema.Table())
		}
	}
	if workers < 1 {
		workers = 1
	}
	return &SyncEngine{
		eng:      eng,
		mongo:    mongo,
		resolver: resolver,
		// NewComposerWithMongo so views embedding external FromSchema collections
		// resolve correctly through the composer during recompose.
		composer: NewComposerWithMongo(eng, mongo, resolver),
		index:    buildViewIndex(views),
		sub:      sub,
		groupID:  groupID,
		topics:   topics,
		workers:  workers,
		done:     make(chan struct{}),
	}
}

// Start launches the projection loop. Idempotent — a second call is a no-op
// (guarding the done channel against a double close).
func (s *SyncEngine) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		s.started.Store(true)
		go func() {
			// done closes only after run() returned — i.e. after run's own
			// deferred chain completed: worker queues closed → wg.Wait()
			// (every in-flight compose+upsert FINISHED) → r.Close() (the
			// Kafka LeaveGroup went out). Coordination is by dependency,
			// never by timing.
			defer close(s.done)
			s.run(ctx)
		}()
	})
}

// Shutdown blocks until the projection loop has fully exited (see the done
// field for the exact dependency chain) or drainCtx expires — returning
// drainCtx.Err() on timeout so bootstrap's coordinated drain surfaces partial
// drains in its shutdown summary. Without this wait the process could exit
// while the LeaveGroup was still in flight, leaving a ghost member holding
// the consumer-group slot: the NEXT boot's JoinGroup then blocks until the
// session times the ghost out — the "first CDC event after boot is late"
// symptom. Nil-safe; an engine that never Started returns immediately.
func (s *SyncEngine) Shutdown(drainCtx context.Context) error {
	if s == nil || !s.started.Load() {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-drainCtx.Done():
		return drainCtx.Err()
	}
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

	// StartFrom earliest preserves the prior kafka-go default (an unset
	// StartOffset defaulted to FirstOffset): a fresh consumer group replays the
	// outbox topics from the beginning to build the projection.
	//
	// CommitInterval > 0 batches offset commits asynchronously on a ticker
	// instead of a sync OffsetCommit RPC per message (which caps throughput at
	// ~9 msg/s in local Docker because each commit roundtrip costs ~100 ms).
	// Safe under at-least-once: the composer re-reads current state from the
	// relational backend on each message and mongo.Upsert keyed by _id is
	// idempotent, so reprocessing the last <=1s window after a consumer crash
	// converges to the same Mongo state.
	sub, err := s.sub.Subscribe(ctx, transport.SubscribeConfig{
		Topics:         s.topics,
		GroupID:        s.groupID,
		StartFrom:      transport.StartFromEarliest,
		CommitInterval: time.Second,
	})
	if err != nil {
		log.Printf("sync engine: subscribe failed: %v", err)
		return
	}
	defer sub.Close()

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
		msg, err := sub.Read(ctx)
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

	// A SharedBase change fans out: recompose every role view's document that
	// references the changed identity. The base table is not a view root, so it
	// only appears in bySharedBase.
	if baseViews, ok := s.index.bySharedBase[event.AggregateType]; ok {
		if err := s.fanOutSharedBase(ctx, event, baseViews); err != nil {
			return err
		}
	}
	// The INVERSE direction: an event on a ROLE table recomposes the person
	// document of every base-rooted SharedBaseView declaring that role. All
	// event types route here — including ARCHIVED/UNARCHIVED (the base
	// convergence is SQL without its own outbox row, so the role event is the
	// only signal) and DELETED (the segment must flip to null, or the whole
	// document must go when the identity was purged).
	if routes, ok := s.index.byRoleTable[event.AggregateType]; ok {
		if err := s.recomposeBaseRooted(ctx, event, routes); err != nil {
			return err
		}
	}
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
			if err := s.mongo.Delete(ctx, s.resolver.Active(view.name), event.AggregateID); err != nil {
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
		if err := s.mongo.Upsert(ctx, s.resolver.Active(view.name), event.AggregateID, doc); err != nil {
			return err
		}
	}
	return nil
}

// fanOutSharedBase recomposes every role-view document that references a changed
// shared identity. For each role view embedding the base, it finds the role docs
// whose FK equals the base id (index-only via FindIDsByField on the role's link
// column) and recomposes each by its own id — so a shared-field change made
// through one role reaches the read models of the OTHER roles of that identity.
// A role row that has since vanished is removed from its view.
func (s *SyncEngine) fanOutSharedBase(ctx context.Context, event kafkaEvent, baseViews []*ViewDefinition) error {
	for _, view := range baseViews {
		_, fkCol, ok := view.schema.SharedBaseRef()
		if !ok {
			continue
		}
		roleIDs, err := s.mongo.FindIDsByField(ctx, s.resolver.Active(view.name), fkCol, event.AggregateID)
		if err != nil {
			return err
		}
		for _, roleID := range roleIDs {
			doc, err := s.composer.Compose(ctx, view, roleID)
			if err != nil {
				return err
			}
			if doc == nil {
				if err := s.mongo.Delete(ctx, s.resolver.Active(view.name), roleID); err != nil {
					return err
				}
				continue
			}
			if err := s.mongo.Upsert(ctx, s.resolver.Active(view.name), roleID, doc); err != nil {
				return err
			}
		}
	}
	return nil
}

// recomposeBaseRooted recomposes the person document of each base-rooted view
// routed from a role event. The full recompose re-reads everything from the
// relational source (active-first role selection included), so INSERTED /
// UPDATED / ARCHIVED / UNARCHIVED / DELETED all converge through the same
// upsert; a nil composition (the identity row is gone — orphan purge — or
// archived under DeleteOnArchive) removes the document instead. An
// unresolvable base id (a malformed DELETED payload — not an expected state)
// logs and skips rather than failing the whole event.
func (s *SyncEngine) recomposeBaseRooted(ctx context.Context, event kafkaEvent, routes []roleRoute) error {
	for _, rt := range routes {
		baseID, err := s.resolveBaseID(ctx, event, rt.role)
		if err != nil {
			return err
		}
		if baseID == "" {
			log.Printf("sync engine: role event %s on %s: shared-base id unresolvable — skipping view %s",
				event.EventType, event.AggregateType, rt.view.name)
			continue
		}
		doc, err := s.composer.Compose(ctx, rt.view, baseID)
		if err != nil {
			return err
		}
		if doc == nil {
			if err := s.mongo.Delete(ctx, s.resolver.Active(rt.view.name), baseID); err != nil {
				return err
			}
			continue
		}
		if err := s.mongo.Upsert(ctx, s.resolver.Active(rt.view.name), baseID, doc); err != nil {
			return err
		}
	}
	return nil
}

// resolveBaseID resolves the shared-base id a role event refers to, per the
// role's link model:
//
//   - shared-PK (fk == role PK): the event's aggregate_id IS the base id — no
//     access at all.
//   - separate-FK, event ≠ DELETED: the row exists — consult the source (the
//     same consult-always rule every other read follows). A row that vanished
//     between the event and its processing yields "" (skip); the DELETED event
//     that removed it follows on the same partition (same key → same worker,
//     ordered) carrying the payload keys, so the document still converges.
//   - separate-FK DELETED: nothing is left to consult — read the FK from the
//     event payload, which the write side records (structural keys, flat map)
//     for exactly this moment. No fallback: the payload dispatch is guaranteed
//     by the current write side; a missing key is a malformed event, surfaced
//     by the caller's log.
func (s *SyncEngine) resolveBaseID(ctx context.Context, event kafkaEvent, r roleDef) (string, error) {
	_, fkCol, ok := r.schema.SharedBaseRef()
	if !ok {
		return "", nil
	}
	if fkCol == r.schema.PKColumn() {
		return event.AggregateID, nil
	}
	if event.EventType == "DELETED" {
		return baseIDFromDeletePayload(event.Payload, fkCol), nil
	}
	row, err := s.composer.fetchRow(ctx, r.schema, r.schema.Table(), r.schema.PKColumn(), event.AggregateID, "", true)
	if err != nil || row == nil {
		return "", err
	}
	fk := row[fkCol]
	if fk == nil {
		return "", nil
	}
	return fmt.Sprintf("%v", fk), nil
}

// baseIDFromDeletePayload reads the shared-base FK from a role DELETED outbox
// payload — the flat structural-keys map the write side records
// ({pkCol: roleID, fkCol: baseID}). Returns "" on a missing/malformed payload.
func baseIDFromDeletePayload(payload []byte, fkCol string) string {
	if len(payload) == 0 {
		return ""
	}
	var keys map[string]any
	if err := json.Unmarshal(payload, &keys); err != nil {
		return ""
	}
	if v, ok := keys[fkCol].(string); ok {
		return v
	}
	return ""
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

// topicSpecsFor returns the EnsureTopics spec slice for the SyncEngine's
// declared topics. Extracted so tests can verify the shape without going
// through a broker connection.
func topicSpecsFor(topics []string) []transport.TopicSpec {
	out := make([]transport.TopicSpec, len(topics))
	for i, t := range topics {
		out[i] = transport.TopicSpec{
			Name:              t,
			NumPartitions:     defaultTopicNumPartitions,
			ReplicationFactor: defaultTopicReplicationFactor,
		}
	}
	return out
}

// ensureTopics asks the transport to provision every topic the SyncEngine
// intends to consume. Retries with linear backoff until ensureTopicsTimeout,
// after which the boot of the consumer goroutine is aborted with a logged error
// rather than left silently stuck.
//
// Idempotent at the transport layer: a pre-existing topic is absorbed as a
// no-op by the adapter's EnsureTopics, so calling this on every restart is safe.
func (s *SyncEngine) ensureTopics(ctx context.Context) error {
	if len(s.topics) == 0 {
		return nil
	}
	specs := topicSpecsFor(s.topics)
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
		if err := s.sub.EnsureTopics(ctx, specs); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
}
