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

// shadowWriteRetries / shadowWriteBackoff bound dual-apply's per-write retry on
// the shadow slot during a rebuild. On exhaustion the rebuild is aborted (the
// shadow flag is cleared cluster-wide) rather than failing the live path.
const (
	shadowWriteRetries = 3
	shadowWriteBackoff = 50 * time.Millisecond
)

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
// The payload carries the event's STRUCTURAL IDENTITY in its "_ids" block
// (aggregate PK, shared-base id + revision, purge flag), so routing decisions
// — which shared identity to fan out for, which person document a role
// DELETED belongs to — read the payload and touch no database. Entity-rooted
// views project their document straight from the payload; SharedBaseView and
// embed views recompose through the composer.
type kafkaEvent struct {
	AggregateType string
	EventType     string
	AggregateID   string
	// Traceparent is the W3C trace context the producer stamped on the outbox
	// row, mapped to a Kafka header by Debezium's Outbox Event Router. Empty
	// when the producing write had tracing off. Used to LINK the projection
	// span back to the producing trace.
	Traceparent string
	// Payload is the raw outbox payload (message.Value), parsed ONCE per event
	// at the top of process (decodeRawPayload) and coerced per view from that
	// shared map.
	Payload []byte
}

// payloadIDs is the "_ids" block of an outbox payload — the structural
// identity the write side stamps on every event.
type payloadIDs struct {
	ID           string `json:"id"`
	Revision     int64  `json:"revision"`
	BaseID       string `json:"base_id"`
	BaseRevision int64  `json:"base_revision"`
	BasePurged   bool   `json:"base_purged"`
	// CreatedAt is the row's created_at instant (RFC 3339) — the incarnation
	// discriminator of the document tombstone: a DETERMINISTIC id reborn under
	// the same natural key restarts its revision, and only the created_at instant
	// tells the new life from a zombie of the dead one.
	CreatedAt string `json:"created_at"`
}

// createdAtMillis renders the created_at instant at the millisecond grain the document
// store keeps for datetimes (BSON) — the tombstone stores and compares at that
// grain. 0 when the payload carries no created_at (schema without CreatedAt).
func (p payloadIDs) createdAtMillis() int64 {
	if p.CreatedAt == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, p.CreatedAt)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// parsePayloadIDs extracts the "_ids" block. ok=false on an empty/malformed
// payload or one without the block.
func parsePayloadIDs(payload []byte) (payloadIDs, bool) {
	if len(payload) == 0 {
		return payloadIDs{}, false
	}
	var envelope struct {
		IDs *payloadIDs `json:"_ids"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.IDs == nil {
		return payloadIDs{}, false
	}
	return *envelope.IDs, true
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
		addTopic(v.RootTable())
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

	// Provision the projection-state registry (tombstone TTL index). Failure
	// degrades to tombstones that never expire — never a projection stop.
	if err := s.mongo.EnsureProjectionState(ctx); err != nil {
		log.Printf("sync engine: projection-state registry provisioning failed (tombstones will not expire): %v", err)
	}

	// StartFrom earliest preserves the prior kafka-go default (an unset
	// StartOffset defaulted to FirstOffset): a fresh consumer group replays the
	// outbox topics from the beginning to build the projection.
	//
	// CommitInterval > 0 batches offset commits asynchronously on a ticker
	// instead of a sync OffsetCommit RPC per message (which caps throughput at
	// ~9 msg/s in local Docker because each commit roundtrip costs ~100 ms).
	// Safe under at-least-once: each event projects from its self-sufficient
	// payload and the Mongo write keyed by _id is idempotent (the revision
	// guards reject a stale replay), so reprocessing the last <=1s window after
	// a consumer crash converges to the same Mongo state.
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
		// G3 activation fence: before applying this message, ensure the pointer
		// cache is within the lease. If it is stale and cannot be re-read, stop
		// consuming rather than apply with a stale view of which rebuilds are
		// active — a dual-apply that missed the shadow would leave it permanently
		// gapped at the flip. Refreshing here also guarantees that once a driver
		// has waited one lease after enabling dual-apply, every pod observed it.
		if ferr := s.resolver.EnsureFresh(ctx); ferr != nil {
			log.Printf("sync engine: dual-apply fence — cannot refresh view pointers, stopping consumption: %v", ferr)
			return
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

// applyUpsert writes the composed document to the view's active slot and, when a
// rebuild is in flight for the view, also to the shadow slot (dual-apply). The
// active write keeps the steady-state semantics — its error fails the event so
// at-least-once redelivery reconverges. A shadow-write failure is handled by
// dualApply (bounded retry → abort) and never fails the live path.
func (s *SyncEngine) applyUpsert(ctx context.Context, viewName, id string, doc Document) error {
	if err := s.mongo.Upsert(ctx, s.resolver.Active(viewName), id, doc); err != nil {
		return err
	}
	if shadow, on := s.resolver.ShadowActive(viewName); on {
		s.dualApply(ctx, viewName, func() error { return s.mongo.Upsert(ctx, shadow, id, doc) })
	}
	return nil
}

// applyConsultUpsert writes a consult-composed document as ONE revision-guarded
// pipeline (consultGuardedStages): own data behind the document's _revision,
// shared-base data behind _base_revision (a SharedBaseView document is a single
// base-revision scope), embed segments only on document creation (the
// recompose-ripple owns them on an existing document — the fieldOwnershipStages
// rule, now composed WITH the guards). A consult that read the relational
// earlier but reaches the store later than a fresher writer is a no-op instead
// of a lost-update: the composed document's data is always at least as fresh as
// its watermark (the composer reads the root/base row FIRST), so the guard can
// only suppress stale writes, never fresh ones. After the write, the document
// tombstone handshake runs for entity-rooted views (a consult racing a DELETED
// must not leave a resurrected document behind).
func (s *SyncEngine) applyConsultUpsert(ctx context.Context, view *ViewDefinition, id string, doc Document) error {
	stages := consultGuardedStages(view, doc)
	if len(stages) == 0 {
		return nil
	}
	if err := s.applyProjection(ctx, view.name, id, stages, true); err != nil {
		return err
	}
	if len(view.embeds) > 0 {
		repairDanglingOneToOne(ctx, s.mongo, s.resolver, s.eng, view, id, doc)
	}
	if !view.isSharedBaseView {
		if err := s.checkTombstone(ctx, view.name, id, watermarkOf(doc[docRevisionField])); err != nil {
			return err
		}
	}
	return nil
}

// applyProjection runs the payload-direct pipeline on the view's active slot
// and, during a rebuild, on the shadow slot too — the same dual-apply
// discipline as applyUpsert, so the blue-green window misses nothing.
//
// upsert decides what a MISSING document means. The writer's OWN document
// passes true — the projection must materialize a document the first event
// creates. The shared-base fan-out passes false: it targets ids another
// writer's FindIDsByField snapshot produced, so a missing document there means
// a role deleted concurrently — upserting would resurrect a skeleton carrying
// only base fields (no PK, no FK, no deleted_at: invisible to every future
// fan-out and reconciliation, yet listed as an active row) that only a full
// rebuild could remove.
func (s *SyncEngine) applyProjection(ctx context.Context, viewName, id string, stages []Document, upsert bool) error {
	if err := s.mongo.ApplyProjection(ctx, s.resolver.Active(viewName), id, stages, upsert); err != nil {
		return err
	}
	if shadow, on := s.resolver.ShadowActive(viewName); on {
		s.dualApply(ctx, viewName, func() error { return s.mongo.ApplyProjection(ctx, shadow, id, stages, upsert) })
	}
	return nil
}

// applyDelete removes the document from the active slot and, during a rebuild,
// from the shadow slot too, with the same dual-apply discipline as applyUpsert.
//
// rev > 0 is the deleted row's LAST revision (the DELETED payload's
// _ids.revision): the delete becomes the full tombstone handshake — record the
// tombstone FIRST, then delete GUARDED (a document a fresher writer already
// advanced past rev survives; the zombie upsert that resurrects one after this
// delete finds the tombstone on its own post-write check and removes itself —
// one of the two sides always fires, by store write order). rev == 0 is a
// consult-observed vanish (the composed row is gone): the delete stays
// unconditional — the row's own DELETED event carries the durable tombstone.
func (s *SyncEngine) applyDelete(ctx context.Context, viewName, id string, rev, createdAtMs int64) error {
	if rev > 0 {
		if err := s.stampTombstone(ctx, viewName, id, rev, createdAtMs); err != nil {
			return err
		}
		if err := s.mongo.DeleteGuarded(ctx, s.resolver.Active(viewName), id, rev, createdAtMs); err != nil {
			return err
		}
		if shadow, on := s.resolver.ShadowActive(viewName); on {
			s.dualApply(ctx, viewName, func() error { return s.mongo.DeleteGuarded(ctx, shadow, id, rev, createdAtMs) })
		}
		return nil
	}
	if err := s.mongo.Delete(ctx, s.resolver.Active(viewName), id); err != nil {
		return err
	}
	if shadow, on := s.resolver.ShadowActive(viewName); on {
		s.dualApply(ctx, viewName, func() error { return s.mongo.Delete(ctx, shadow, id) })
	}
	return nil
}

// dualApply runs a shadow-slot write with a bounded retry. On exhaustion it
// aborts the rebuild — clearing the shadow flag cluster-wide — instead of
// failing the live path: the active write already succeeded and the offset
// advances, so a shadow that cannot be kept current is abandoned, not flipped.
func (s *SyncEngine) dualApply(ctx context.Context, viewName string, write func() error) {
	dualApplyShadow(ctx, s.eng, s.resolver, viewName, write)
}

// dualApplyShadow is the shared shadow-write discipline behind SyncEngine's
// dual-apply, also used by the UpstreamSubscriber's recompose-ripple: EVERY
// writer of a view document must reach the shadow slot during a rebuild
// window, or the flipped collection silently misses the writes that landed
// only on the retiring active slot. Bounded retry; on exhaustion the rebuild
// is aborted cluster-wide rather than failing the live path.
func dualApplyShadow(ctx context.Context, eng core.RelationalEngine, resolver *ViewResolver, viewName string, write func() error) {
	var err error
	for attempt := 0; attempt < shadowWriteRetries; attempt++ {
		if err = write(); err == nil {
			return
		}
		if attempt < shadowWriteRetries-1 {
			select {
			case <-time.After(shadowWriteBackoff):
			case <-ctx.Done():
				return
			}
		}
	}
	log.Printf("sync engine: shadow write failed for view %q after %d attempts, aborting rebuild: %v",
		viewName, shadowWriteRetries, err)
	if aerr := abortSlotRebuild(ctx, eng.Querier(), eng.Dialect(), viewName); aerr != nil {
		log.Printf("sync engine: abort rebuild %q failed: %v", viewName, aerr)
		return
	}
	if rerr := resolver.Refresh(ctx); rerr != nil {
		log.Printf("sync engine: resolver refresh after abort %q failed: %v", viewName, rerr)
	}
}

func (s *SyncEngine) process(ctx context.Context, event kafkaEvent) error {
	ctx, span := tracing.StartConsumerSpanIf(s.traceKafka, ctx,
		"github.com/ClaudioSchirmer/omnicore/infra/sync",
		"sync "+event.AggregateType, event.Traceparent)
	defer span.End()

	// The payload is parsed ONCE per event; every consumer below (the payload
	// fan-out, the per-view payload-direct projection) coerces its typed input
	// over this shared map — never a second unmarshal of the same bytes. A
	// payload that does not decode (corrupt or truncated) is skipped: every
	// framework event carries a well-formed payload with its _ids block.
	raw, ok := decodeRawPayload(event.Payload)
	if !ok {
		log.Printf("sync engine: undecodable payload on %s %s (id=%s), skipping",
			event.AggregateType, event.EventType, event.AggregateID)
		return nil
	}

	ids := rawPayloadIDs(raw)

	// A SharedBase change fans out: recompose every role view's document that
	// references the changed identity. Two triggers converge here:
	//   - a BASE-table event (the purge DELETED): the event's aggregate_id IS
	//     the base id;
	//   - a ROLE-table event: the write side stamps the base id in
	//     _ids.base_id, so the fan-out rides the role event itself — there is
	//     no empty base-table row.
	// Both may fire across one identity's writes — the recompose is idempotent,
	// so a duplicate is harmless.
	if baseViews, ok := s.index.bySharedBase[event.AggregateType]; ok {
		if err := s.fanOutSharedBase(ctx, event.AggregateID, baseViews); err != nil {
			return err
		}
		// The purge destroyed the identity: drop its registry base-revision record.
		// (A zombie role event may recreate it later — inert garbage, no role
		// document exists to pull from it; the TTL-less base-revision record is bounded
		// by the purge volume.)
		if event.EventType == "DELETED" && ids.BasePurged {
			if err := s.dropBaseRevision(ctx, event.AggregateType, event.AggregateID); err != nil {
				return err
			}
		}
	}
	roleBaseTable, isRole := s.index.baseOfRole[event.AggregateType]
	if isRole && ids.BaseID != "" {
		// HANDSHAKE, push side: the base revision is stamped into the registry
		// BEFORE the fan-out probes for target documents. If the probe misses a
		// document still being born, that document's writer — whose own write
		// necessarily enters the store after our probe, hence after this stamp —
		// finds the newer base revision on its post-write pull check and repairs.
		if err := s.stampBaseRevision(ctx, roleBaseTable, ids.BaseID, ids.BaseRevision); err != nil {
			return err
		}
		if baseViews, ok := s.index.bySharedBase[roleBaseTable]; ok {
			if err := s.fanOutSharedBasePayload(ctx, raw, ids.BaseID, baseViews); err != nil {
				return err
			}
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
		// overrides it). ARCHIVED by default goes through the projection branch
		// below — the document survives with deleted_at populated, so consumers
		// that pass IncludeArchived=true can read it. Views that opt in via
		// ViewDefinition.DeleteOnArchive() instead remove the document on
		// ARCHIVED. An UNARCHIVED event always hits the projection branch.
		// The event's own revision (the row's last) rides along as the
		// tombstone — the guard that stops a zombie upsert from resurrecting
		// the document after it is gone.
		if shouldDeleteFromView(event.EventType, view.deleteOnArchive) {
			if err := s.applyDelete(ctx, view.name, event.AggregateID, ids.Revision, ids.createdAtMillis()); err != nil {
				return err
			}
			continue
		}
		// PAYLOAD-DIRECT projection — the day-to-day path: the payload IS the
		// state, applied as one atomic pipeline (typed decode + revision-guarded
		// base fields + surgical child edits). No relational read. The
		// post-write tombstone check closes the delete race (a zombie write
		// after DELETED removes itself).
		//
		// The composer (consult) remains for exactly two cases:
		//   - SharedBaseView documents (base-rooted; the archived-remnant
		//     segment pick is cross-row — recomposeBaseRooted owns them);
		//   - views with external EMBEDS (the embed enrichment on first
		//     composition still reads the local Mongo mirror).
		if !view.isSharedBaseView && len(view.embeds) == 0 {
			stages := buildProjectionStages(view.schema, coercePayloadEvent(view.schema, raw))
			if len(stages) == 0 {
				continue
			}
			if err := s.applyProjection(ctx, view.name, event.AggregateID, stages, true); err != nil {
				return err
			}
			if err := s.checkTombstone(ctx, view.name, event.AggregateID, ids.Revision); err != nil {
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
		if err := s.applyConsultUpsert(ctx, view, event.AggregateID, doc); err != nil {
			return err
		}
	}
	// HANDSHAKE, pull side: a document this event just MATERIALIZED
	// (INSERTED, or UNARCHIVED re-creating under DeleteOnArchive) may carry
	// base state older than a fan-out that could not find it (the fan-out's
	// FindIDsByField snapshot predates the document). The registry's base revision
	// record proves it: when a newer base revision was pushed, repair by consult —
	// the composed closure is read fresh from the relational and applied
	// guarded, so the repair can never regress anything. The store's write
	// order makes the two sides meet: if the fan-out's probe missed this
	// document, this pull necessarily sees that fan-out's base-revision stamp.
	if isRole && ids.BaseID != "" && ids.BaseRevision > 0 &&
		(event.EventType == "INSERTED" || event.EventType == "UNARCHIVED") {
		stamped, err := s.stampedBaseRevision(ctx, roleBaseTable, ids.BaseID)
		if err != nil {
			return err
		}
		if stamped > ids.BaseRevision {
			for _, view := range views {
				doc, err := s.composer.Compose(ctx, view, event.AggregateID)
				if err != nil {
					return err
				}
				if doc == nil {
					continue
				}
				if err := s.applyConsultUpsert(ctx, view, event.AggregateID, doc); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// fanOutSharedBase recomposes every role-view document that references a changed
// shared identity (baseID — from a base event's aggregate_id or a role event's
// _ids.base_id). For each role view embedding the base, it finds the role docs
// whose FK equals the base id (index-only via FindIDsByField on the role's link
// column) and recomposes each by its own id — so a shared-field change made
// through one role reaches the read models of the OTHER roles of that identity.
// A role row that has since vanished is removed from its view.
func (s *SyncEngine) fanOutSharedBase(ctx context.Context, baseID string, baseViews []*ViewDefinition) error {
	for _, view := range baseViews {
		_, fkCol, ok := view.schema.SharedBaseRef()
		if !ok {
			continue
		}
		roleIDs, err := s.mongo.FindIDsByField(ctx, s.resolver.Active(view.name), fkCol, baseID)
		if err != nil {
			return err
		}
		if len(roleIDs) == 0 {
			continue
		}
		// Recompose all affected role docs in one set-based pass — collapses the
		// relational N+1 (one closure re-read per role id) into one round trip per
		// related table. The Mongo write stays per-doc through applyUpsert /
		// applyDelete so the dual-apply-to-shadow discipline (blue-green) and the
		// per-id at-least-once semantics are unchanged. A role id absent from the
		// composed set is a vanished row (hard-deleted / archived under
		// DeleteOnArchive) — its doc is removed, exactly as the nil Compose did.
		composed, err := s.composer.ComposeBatch(ctx, view, roleIDs)
		if err != nil {
			return err
		}
		pkCol := schemaPK(view.schema)
		present := make(map[string]struct{}, len(composed))
		for _, doc := range composed {
			id := fmt.Sprintf("%v", doc[pkCol])
			present[id] = struct{}{}
			if err := s.applyConsultUpsert(ctx, view, id, doc); err != nil {
				return err
			}
		}
		for _, roleID := range roleIDs {
			if _, ok := present[roleID]; ok {
				continue
			}
			if err := s.applyDelete(ctx, view.name, roleID, 0, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// fanOutSharedBasePayload is the payload-direct shared-identity fan-out: on a
// role event it applies the revision-guarded base fields + surgical
// base-children edits to every role document referencing the
// identity — no relational read. The writer's own document receives the same
// guarded stages on top of its full projection: idempotent by construction.
// Vanished roles need no reconciliation here — their own DELETED events remove
// their documents — but that removal must not be UNDONE by this write: the
// target ids come from a FindIDsByField snapshot, so a document missing at
// write time is a role deleted concurrently (the only way a snapshotted id can
// be absent), and the projection applies with upsert=false — a no-op instead
// of resurrecting a base-fields-only skeleton no future event could ever
// clean.
//
// A role document BORN concurrently is the one target this push can lose: the
// snapshot cannot see a document whose INSERTED/UNARCHIVED projection has not
// landed yet, and that document's own payload carries the base state of ITS
// commit — possibly older than this event's. The registry handshake closes it:
// the caller stamped this event's base revision BEFORE this probe, so the
// late-born document's writer — whose store write necessarily follows the
// probe that missed it — finds the newer base revision on its post-write pull check
// and repairs by consult. The base-table trigger (fanOutSharedBase) handles a
// base event (the purge DELETED) instead.
//
// raw is the event's payload parsed once by the caller (decodeRawPayload);
// each view coerces its typed input over that shared map — no re-parse per view.
func (s *SyncEngine) fanOutSharedBasePayload(ctx context.Context, raw map[string]any, baseID string, baseViews []*ViewDefinition) error {
	for _, view := range baseViews {
		_, fkCol, ok := view.schema.SharedBaseRef()
		if !ok {
			continue
		}
		stages := buildFanOutStages(view.schema, coercePayloadEvent(view.schema, raw))
		if len(stages) == 0 {
			continue
		}
		roleIDs, err := s.mongo.FindIDsByField(ctx, s.resolver.Active(view.name), fkCol, baseID)
		if err != nil {
			return err
		}
		for _, rid := range roleIDs {
			if err := s.applyProjection(ctx, view.name, rid, stages, false); err != nil {
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
	// The base id is a property of the event (its payload's _ids.base_id), not
	// of the route — resolve it once for every base-rooted view the role feeds.
	baseID := resolveBaseID(event)
	if baseID == "" {
		log.Printf("sync engine: role event %s on %s: shared-base id unresolvable — skipping",
			event.EventType, event.AggregateType)
		return nil
	}
	for _, rt := range routes {
		doc, err := s.composer.Compose(ctx, rt.view, baseID)
		if err != nil {
			return err
		}
		if doc == nil {
			if err := s.applyDelete(ctx, rt.view.name, baseID, 0, 0); err != nil {
				return err
			}
			continue
		}
		if err := s.applyConsultUpsert(ctx, rt.view, baseID, doc); err != nil {
			return err
		}
	}
	return nil
}

// resolveBaseID resolves the shared-base id a role event refers to, read
// straight from the payload's _ids.base_id — the write side stamps it on every
// role event, so the resolution touches no database. "" when the event carries
// no base id.
func resolveBaseID(event kafkaEvent) string {
	ids, _ := parsePayloadIDs(event.Payload)
	return ids.BaseID
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
