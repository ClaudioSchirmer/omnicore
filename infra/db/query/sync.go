package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// defaultWorkerQueueDepth bounds in-flight messages per worker. Every queued
// message carries its own transport.Completion and the transport confirms
// NOTHING until that outcome is reported, so this is a throughput knob, not a
// loss window: a crash leaves every queued message unconfirmed and it
// redelivers. Kept small so a crash costs little reprocessing.
const defaultWorkerQueueDepth = 4

// Retry policy for one event. Attempts are bounded so a permanently broken
// aggregate cannot occupy a worker forever; the backoff is exponential with
// jitter so a transient outage (a flip mid-operation, a brief Mongo blip) is
// absorbed without a thundering herd across workers.
const (
	processRetries     = 5
	processBackoffBase = 100 * time.Millisecond
	processBackoffMax  = 5 * time.Second
	// processAttemptTimeout bounds ONE attempt end to end. Without it, a Mongo
	// write during a quorum loss (majority concern, no primary) or a black-hole
	// network partition blocks forever: no error, no retry, no park — the worker
	// simply stops, which is a silent I4 violation the retry budget cannot see.
	// A normal attempt is milliseconds; this is three orders of magnitude above
	// that, so it only ever fires on a genuinely wedged dependency.
	processAttemptTimeout = 30 * time.Second
)

// shadowWriteRetries / shadowWriteBackoff bound dual-apply's per-write retry on
// the shadow slot during a rebuild — the fast in-place absorption of a blip. On
// exhaustion the error is RETURNED, so the event fails and is retried as a
// whole; abandoning the rebuild is a separate, evidence-based decision (see
// shadowAbortThreshold).
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
	// parkedRetryOff / parkedRetryEvery hold the mongo.parkedRetry knob
	// (ConfigureParkedRetry): the replay driver runs by default on
	// projectionRetryInterval; an operator may slow it or turn it off. The
	// zero values mean "default on, default cadence", so the wiring-less test
	// shape and every existing constructor keep today's behavior.
	parkedRetryOff   bool
	parkedRetryEvery time.Duration
	// rippleReplayers maps a subscription topic to the closure that re-runs its
	// ripple for one source id — how the retry driver replays kind=ripple rows
	// whose source is an upstream mirror. Registered at wiring time
	// (WithViewChaining), read only by the parked-retry loop.
	rippleReplayers map[string]func(ctx context.Context, sourceID string)
	// viewSignal fans a projection write out to the views that MATERIALIZE this
	// one via a query.JoinView embed. Nil when no view embeds a view (the default
	// shape) — every call on it is then a no-op on a nil receiver.
	viewSignal *viewEmbedSignal
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

	// metrics carries the projection path's counters and liveness clocks. See
	// ProjectionHealth: the clocks matter more than the counters, because a loop
	// that has stopped emits no errors at all.
	metrics *projectionMetrics
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
// (aggregate ID, shared-base id + revision, purge flag), so routing decisions
// — which shared identity to fan out for, which person document a role
// DELETED belongs to — read the payload and touch no database. Entity-rooted
// views project their document straight from the payload; SharedBaseView and
// embed views recompose through the composer.
type kafkaEvent struct {
	AggregateType string
	EventType     string
	AggregateID   string
	// Topic is the source topic the message arrived on, carried so a parked
	// event records where it came from (omnicore_projection_failures.topic).
	Topic string
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
		Topic:         msg.Topic,
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
	// NewComposerWithMongo so views embedding external JoinUpstream collections
	// (and local JoinView sources) resolve correctly through the composer.
	composer := NewComposerWithMongo(eng, mongo, resolver)
	return &SyncEngine{
		eng:        eng,
		mongo:      mongo,
		resolver:   resolver,
		composer:   composer,
		index:      buildViewIndex(views),
		sub:        sub,
		groupID:    groupID,
		topics:     topics,
		workers:    workers,
		viewSignal: newViewEmbedSignal(eng, mongo, composer, resolver, views, groupID, nil, newUpstreamMetrics()),
		done:       make(chan struct{}),
		metrics:    newProjectionMetrics(),
	}
}

// Start launches the projection loop. Idempotent — a second call is a no-op
// (guarding the done channel against a double close).
// ConfigureParkedRetry adjusts the parked-events replay driver before Start:
// off, or on a custom cadence (0 keeps the framework default). This is the
// `mongo.parkedRetry` yaml block's seam. Turning the driver off means a parked
// event is replayed only by the reconcile sweep (when enabled) or by a manual
// RetryPendingProjectionFailures call — the ledger then holds dead letters, not
// deferred work.
func (s *SyncEngine) ConfigureParkedRetry(enabled bool, every time.Duration) {
	s.parkedRetryOff = !enabled
	if every > 0 {
		s.parkedRetryEvery = every
	}
}

func (s *SyncEngine) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		s.started.Store(true)
		if !s.parkedRetryOff {
			go s.retryParkedLoop(ctx)
		}
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
	// Provision the projection-state registry (tombstone TTL index). Failure
	// degrades to tombstones that never expire — never a projection stop.
	if s.mongo != nil {
		if err := s.mongo.EnsureProjectionState(ctx); err != nil {
			slog.WarnContext(ctx, "projection.state_registry.provision_failed",
				slog.String("detail", "tombstones will not expire"),
				slog.String("err", err.Error()))
		}
	}

	// SUPERVISION. Every fatal path below used to `return`, and startOnce made
	// that terminal for the life of the process: one failed topic ensure, one
	// failed Subscribe, and this pod served reads forever from a projection that
	// had silently stopped advancing — while /readyz kept answering 200, because
	// readiness deliberately excludes consumer health. A consumer that never runs
	// never errors, so the only honest signal is a session that keeps trying and
	// says so each time it fails.
	for attempt := 0; ctx.Err() == nil; attempt++ {
		if attempt > 0 {
			if !sleepBackoff(ctx, minInt(attempt, sessionBackoffCap)) {
				return
			}
		}
		if err := s.ensureTopics(ctx); err != nil {
			slog.ErrorContext(ctx, "projection.topics.ensure_failed",
				slog.String("consumerGroup", s.groupID),
				slog.Int("attempt", attempt+1),
				slog.String("err", err.Error()))
			continue
		}
		if err := s.consume(ctx); err != nil {
			s.metrics.inc(MetricProjectionSessionRestart)
			slog.ErrorContext(ctx, "projection.session.ended",
				slog.String("consumerGroup", s.groupID),
				slog.Int("attempt", attempt+1),
				slog.String("err", err.Error()))
			continue
		}
		return // clean exit: the context ended
	}
}

// sessionBackoffCap bounds the supervisor's exponential backoff so a long
// broker outage settles at a steady retry cadence instead of drifting to hours.
const sessionBackoffCap = 6

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// consume runs ONE consumer session: subscribe, spin the worker pool, and read
// until the context ends or the session must be torn down. A returned error asks
// the supervisor for a fresh session; nil means a clean, context-driven exit.
func (s *SyncEngine) consume(ctx context.Context) error {

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
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Close()

	// Worker pool: each worker owns a channel; reader dispatches by FNV-1a
	// hash of aggregate_id. Same aggregate → same worker → ordering preserved.
	// Different aggregates parallelize.
	queues := make([]chan queuedEvent, s.workers)
	var wg sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		queues[i] = make(chan queuedEvent, defaultWorkerQueueDepth)
		wg.Add(1)
		go func(q <-chan queuedEvent) {
			defer wg.Done()
			for qe := range q {
				s.processToOutcome(ctx, qe)
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
		msg, completion, err := sub.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		// G3 activation fence: before applying this message, ensure the pointer
		// cache is within the lease. If it is stale and cannot be re-read, hand
		// the message BACK to the transport and stop consuming rather than apply
		// with a stale view of which rebuilds are active — a dual-apply that
		// missed the shadow would leave it permanently gapped at the flip.
		// Refreshing here also guarantees that once a driver has waited one lease
		// after enabling dual-apply, every pod observed it.
		if ferr := s.resolver.EnsureFresh(ctx); ferr != nil {
			// Applying with a stale view of which rebuilds are active could leave a
			// shadow permanently gapped at the flip, so the message is handed back
			// rather than processed. The SESSION ends and the supervisor opens a new
			// one after a backoff — the loop must not die here, which is what it
			// used to do.
			s.metrics.inc(MetricProjectionHandedBack)
			_ = completion.Failed(ctx)
			return fmt.Errorf("dual-apply fence: cannot refresh view pointers: %w", ferr)
		}
		event := extractEvent(msg)
		if event.AggregateID == "" || event.AggregateType == "" || event.EventType == "" {
			// Terminal by nature: no retry can add metadata the producer never
			// wrote. Completing it is what lets the stream advance past it.
			slog.WarnContext(ctx, "projection.event.incomplete_metadata",
				slog.String("topic", msg.Topic),
				slog.String("key", string(msg.Key)),
				slog.String("aggregateType", event.AggregateType),
				slog.String("eventType", event.EventType))
			_ = completion.Done(ctx)
			continue
		}
		// Blocking send: when a worker's queue is full, the reader stalls.
		// That's the desired backpressure — the adapter keeps absorbing while
		// the reader waits, and the consumer-group heartbeat runs on its own
		// goroutine inside the client so the broker doesn't kick us out.
		select {
		case queues[bucketOf(event.AggregateID, s.workers)] <- queuedEvent{event: event, completion: completion}:
		case <-ctx.Done():
			_ = completion.Failed(ctx)
			return nil
		}
	}
}

// queuedEvent is one dispatched message: the decoded event plus the handle that
// reports its outcome back to the transport. The completion travels WITH the
// event through the worker queues, so the confirmation is issued by whoever
// actually finished the work — not by the reader that merely saw the message.
type queuedEvent struct {
	event      kafkaEvent
	completion transport.Completion
}

// processToOutcome drives one event to a terminal outcome and reports it. This
// is the function that makes the read side's stated contract true: every
// correctness argument in this package ("at-least-once redelivery reconverges",
// the revision guards, the tombstone handshake) assumes a failed event comes
// back, and before this existed it never did — the error was logged and the
// offset advanced regardless.
func (s *SyncEngine) processToOutcome(ctx context.Context, qe queuedEvent) {
	var lastErr error
	for attempt := 0; attempt < processRetries; attempt++ {
		if attempt > 0 {
			if !sleepBackoff(ctx, attempt) {
				// Shutting down mid-retry: hand the event back so the next boot
				// picks it up, rather than recording it as a defect.
				_ = qe.completion.Failed(ctx)
				return
			}
			// Re-read the pointer cache before retrying. The previous attempt may
			// have failed BECAUSE a flip moved the slot underneath it, and every
			// write resolves its collection per call — so a refreshed cache is
			// exactly what makes the retry land on the NEW active slot instead of
			// the retired one.
			if err := s.resolver.Refresh(ctx); err != nil {
				lastErr = err
				continue
			}
		}
		if err := s.processBounded(ctx, qe.event); err != nil {
			lastErr = err
			s.metrics.inc(MetricProjectionRetried)
			// One WARN per failed attempt: a lagging projection must be visible
			// in the log stream while it is happening, not only after parking —
			// the counter alone cannot say WHICH aggregate is stuck, and this
			// path is exceptional enough that the volume is trivial.
			slog.WarnContext(ctx, "projection.retry",
				slog.String("consumerGroup", s.groupID),
				slog.String("topic", qe.event.Topic),
				slog.String("aggregateType", qe.event.AggregateType),
				slog.String("eventType", qe.event.EventType),
				slog.String("aggregateId", qe.event.AggregateID),
				slog.Int("attempt", attempt+1),
				slog.Int("retryBudget", processRetries),
				slog.String("err", err.Error()))
			continue
		}
		s.metrics.processed(time.Now())
		_ = qe.completion.Done(ctx)
		return
	}
	if ctx.Err() != nil {
		s.metrics.inc(MetricProjectionHandedBack)
		_ = qe.completion.Failed(ctx)
		return
	}
	// Retry budget exhausted. PARK the event and advance.
	//
	// This is the step that makes "advance" honest. The event's payload is the
	// aggregate's full state, so recording it in the ledger defers the work
	// instead of discarding it: a retry driver replays it later, and the
	// reconciliation sweep repairs it regardless. Holding the message instead
	// would stall a whole Kafka partition over one broken document.
	//
	// If the park itself fails there is nothing further this loop can do — it
	// says so at Error level and relies on the sweep, which is deliberately
	// independent of this table.
	s.parkEvent(ctx, qe.event, lastErr)
	_ = qe.completion.Done(ctx)
}

// processBounded runs one processing attempt under processAttemptTimeout. The
// child context keeps shutdown propagation (cancelling the parent cancels the
// attempt); the deadline only adds an upper bound, so a wedged dependency turns
// into an error the retry/park path can act on instead of an indefinite stall.
// Callers deciding shutdown-vs-exhaustion must consult the PARENT context, which
// this deadline never touches.
func (s *SyncEngine) processBounded(ctx context.Context, event kafkaEvent) error {
	actx, cancel := context.WithTimeout(ctx, processAttemptTimeout)
	defer cancel()
	return s.process(actx, event)
}

// projectionRetryInterval is how often the parking ledger is swept for replay
// when mongo.parkedRetry does not name a cadence. Parked events are already the
// exceptional path, so this is deliberately slow: the sweep exists to recover
// from an outage that has since healed, not to be a second retry loop competing
// with the first. Deployments that want a tighter recovery latency (dev, QA)
// lower it via mongo.parkedRetry.intervalMinutes.
const projectionRetryInterval = 10 * time.Minute

// registerRippleReplayer teaches the retry driver how to replay ripple rows
// whose source is `topic` — an upstream subscriber registers its topic here
// (WithViewChaining); view sources ("view:<name>") dispatch to the view signal
// and need no registration. Called at wiring time, before Start.
func (s *SyncEngine) registerRippleReplayer(topic string, fn func(ctx context.Context, sourceID string)) {
	if s.rippleReplayers == nil {
		s.rippleReplayers = map[string]func(ctx context.Context, sourceID string){}
	}
	s.rippleReplayers[topic] = fn
}

// RetryPendingProjectionFailures replays every pending ledger row for this
// consumer group — BOTH kinds — and resolves the ones that now succeed. This is
// what turns "advance" into "deferred" rather than "forgotten".
//
//   - kind=event: re-process the stored payload through the normal projection
//     path. Safe against a concurrently-processed event for the same aggregate:
//     every write is revision-guarded, so a replay carrying an older payload is
//     rejected by the guard rather than regressing the document. Resolution is
//     stamped HERE on success.
//   - kind=ripple: re-run the segment recompose for the (source, source id)
//     pair from CURRENT state — "view:<name>" sources through the view signal,
//     subscription topics through the replayer the subscriber registered.
//     Resolution is stamped by the RIPPLER itself on a clean pass (the same
//     path a live event resolves through); rows are deduplicated per source id
//     because one ripple refreshes every dependent view at once.
//
// Multi-pod note: every pod sweeps its own group's ledger, so a row may be
// replayed by more than one pod in the same interval. That is redundant work,
// never a correctness problem (the writes are idempotent and guarded), and the
// volume is bounded by how many keys are actually stuck — which in a healthy
// service is zero.
func (s *SyncEngine) RetryPendingProjectionFailures(ctx context.Context) error {
	if s.eng == nil || s.groupID == "" {
		return nil
	}
	pending, err := ListPendingProjectionFailures(ctx, s.eng.Querier(), s.eng.Dialect(), s.groupID)
	if err != nil {
		return fmt.Errorf("retry parked projections: %w", err)
	}
	rippled := map[string]bool{} // topic + "\x00" + source id → already replayed this sweep
	for _, rec := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rec.Kind == ProjectionFailureKindRipple {
			key := rec.Topic + "\x00" + rec.AggregateID
			if rippled[key] {
				continue
			}
			rippled[key] = true
			s.replayRipple(ctx, rec)
			continue
		}
		event := kafkaEvent{
			AggregateType: rec.AggregateType,
			EventType:     rec.EventType,
			AggregateID:   rec.AggregateID,
			Topic:         rec.Topic,
			Traceparent:   rec.Traceparent,
			Payload:       rec.Payload,
		}
		if perr := s.processBounded(ctx, event); perr != nil {
			slog.WarnContext(ctx, "projection.parked_event.replay_failed",
				slog.String("consumerGroup", s.groupID),
				slog.String("aggregateType", rec.AggregateType),
				slog.String("aggregateId", rec.AggregateID),
				slog.Int("attempt", rec.Attempt),
				slog.String("err", perr.Error()))
			continue
		}
		if rerr := ResolveProjectionFailure(ctx, s.eng.Querier(), s.eng.Dialect(),
			s.groupID, ProjectionFailureKindEvent, rec.Topic, rec.AggregateType, rec.AggregateID); rerr != nil {
			slog.WarnContext(ctx, "projection.parked_event.resolve_failed",
				slog.String("aggregateId", rec.AggregateID), slog.String("err", rerr.Error()))
			continue
		}
		s.metrics.inc(MetricProjectionReplayed)
		slog.InfoContext(ctx, "projection.parked_event.replayed",
			slog.String("consumerGroup", s.groupID),
			slog.String("aggregateType", rec.AggregateType),
			slog.String("aggregateId", rec.AggregateID),
			slog.Int("attempts", rec.Attempt))
	}
	return nil
}

// replayRipple re-runs the fan-out for one ripple row's (source, source id)
// pair. Success is observed by the RIPPLER resolving the rows; here only the
// dispatch is decided.
func (s *SyncEngine) replayRipple(ctx context.Context, rec ProjectionFailureRecord) {
	const viewLabelPrefix = "view:"
	if strings.HasPrefix(rec.Topic, viewLabelPrefix) {
		src := strings.TrimPrefix(rec.Topic, viewLabelPrefix)
		if s.viewSignal == nil || !s.viewSignal.Tracks(src) {
			// The topology changed under the row: the source view is no longer
			// embedded anywhere. There is no segment left to repair, so the row
			// is moot — resolve it rather than pend it forever.
			slog.WarnContext(ctx, "projection.parked_ripple.source_untracked",
				slog.String("source", rec.Topic), slog.String("view", rec.AggregateType))
			_ = ResolveProjectionFailure(ctx, s.eng.Querier(), s.eng.Dialect(),
				s.groupID, ProjectionFailureKindRipple, rec.Topic, rec.AggregateType, rec.AggregateID)
			return
		}
		slog.InfoContext(ctx, "projection.parked_ripple.replaying",
			slog.String("source", rec.Topic), slog.String("sourceId", rec.AggregateID))
		s.viewSignal.replay(ctx, src, rec.AggregateID)
		return
	}
	fn := s.rippleReplayers[rec.Topic]
	if fn == nil {
		// No subscriber registered for this topic (subscription removed, or the
		// degenerate boot). Same moot-row reasoning as the untracked view.
		slog.WarnContext(ctx, "projection.parked_ripple.source_untracked",
			slog.String("source", rec.Topic), slog.String("view", rec.AggregateType))
		_ = ResolveProjectionFailure(ctx, s.eng.Querier(), s.eng.Dialect(),
			s.groupID, ProjectionFailureKindRipple, rec.Topic, rec.AggregateType, rec.AggregateID)
		return
	}
	slog.InfoContext(ctx, "projection.parked_ripple.replaying",
		slog.String("source", rec.Topic), slog.String("sourceId", rec.AggregateID))
	fn(ctx, rec.AggregateID)
}

// retryParkedLoop sweeps the parking ledger on projectionRetryInterval until the
// context ends.
func (s *SyncEngine) retryParkedLoop(ctx context.Context) {
	every := s.parkedRetryEvery
	if every <= 0 {
		every = projectionRetryInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := s.RetryPendingProjectionFailures(ctx); err != nil {
				slog.WarnContext(ctx, "projection.parked_sweep.failed",
					slog.String("consumerGroup", s.groupID), slog.String("err", err.Error()))
				continue
			}
			s.metrics.swept(time.Now())
		case <-ctx.Done():
			return
		}
	}
}

// parkEvent records an exhausted event in the projection failure ledger.
func (s *SyncEngine) parkEvent(ctx context.Context, event kafkaEvent, cause error) {
	slog.ErrorContext(ctx, "projection.event.parked",
		slog.String("consumerGroup", s.groupID),
		slog.String("topic", event.Topic),
		slog.String("aggregateType", event.AggregateType),
		slog.String("eventType", event.EventType),
		slog.String("aggregateId", event.AggregateID),
		slog.Int("attempts", processRetries),
		slog.String("err", errString(cause)))
	s.metrics.inc(MetricProjectionParked)
	if s.eng == nil {
		return // the wiring-less test shape: nothing to park into
	}
	if err := RecordProjectionFailure(ctx, s.eng.Querier(), s.eng.Dialect(), ProjectionFailureRecord{
		Kind:          ProjectionFailureKindEvent,
		ConsumerGroup: s.groupID,
		Topic:         event.Topic,
		AggregateType: event.AggregateType,
		EventType:     event.EventType,
		AggregateID:   event.AggregateID,
		Traceparent:   event.Traceparent,
		Payload:       event.Payload,
		Error:         errString(cause),
	}); err != nil {
		s.metrics.inc(MetricProjectionParkFailed)
		slog.ErrorContext(ctx, "projection.park.failed",
			slog.String("consumerGroup", s.groupID),
			slog.String("aggregateType", event.AggregateType),
			slog.String("aggregateId", event.AggregateID),
			slog.String("err", err.Error()))
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sleepBackoff waits the exponential-with-jitter delay before attempt n,
// reporting false when the context ended first.
func sleepBackoff(ctx context.Context, attempt int) bool {
	d := processBackoffBase << (attempt - 1)
	if d > processBackoffMax {
		d = processBackoffMax
	}
	// Full jitter: spreads retries of concurrently-failing workers instead of
	// re-synchronising them on the same instant.
	d = time.Duration(rand.Int63n(int64(d)) + int64(d)/2)
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
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
		repairDanglingOneToOne(ctx, s.mongo, s.resolver, s.eng, view, id, doc, s.viewSignal.Written)
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
// only base fields (no ID, no ParentID, no deleted_at: invisible to every future
// fan-out and reconciliation, yet listed as an active row) that only a full
// rebuild could remove.
func (s *SyncEngine) applyProjection(ctx context.Context, viewName, id string, stages []Document, upsert bool) error {
	before := s.viewSignal.Before(ctx, viewName, id)
	if err := s.mongo.ApplyProjection(ctx, s.resolver.Active(viewName), id, stages, upsert); err != nil {
		return err
	}
	if shadow, on := s.resolver.ShadowActive(viewName); on {
		if err := s.dualApply(ctx, viewName, func() error { return s.mongo.ApplyProjection(ctx, shadow, id, stages, upsert) }); err != nil {
			return err
		}
	}
	// The signal rides EVERY successful projection write — including the
	// upsert=false fan-out passes, which are no-ops when the document is absent:
	// the post-write read-back is what decides, and an absent document produces
	// no signal at all (see viewEmbedSignal.Written).
	s.viewSignal.WrittenWithBefore(ctx, viewName, id, before)
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
	// The pre-delete document is the ONLY source of the 1:N parent id once the
	// row is gone — captured before either delete path runs.
	before := s.viewSignal.Before(ctx, viewName, id)
	if rev > 0 {
		if err := s.stampTombstone(ctx, viewName, id, rev, createdAtMs); err != nil {
			return err
		}
		if err := s.mongo.DeleteGuarded(ctx, s.resolver.Active(viewName), id, rev, createdAtMs); err != nil {
			return err
		}
		if shadow, on := s.resolver.ShadowActive(viewName); on {
			if err := s.dualApply(ctx, viewName, func() error { return s.mongo.DeleteGuarded(ctx, shadow, id, rev, createdAtMs) }); err != nil {
				return err
			}
		}
		s.viewSignal.Deleted(ctx, viewName, id, before, rev)
		return nil
	}
	if err := s.mongo.Delete(ctx, s.resolver.Active(viewName), id); err != nil {
		return err
	}
	if shadow, on := s.resolver.ShadowActive(viewName); on {
		if err := s.dualApply(ctx, viewName, func() error { return s.mongo.Delete(ctx, shadow, id) }); err != nil {
			return err
		}
	}
	s.viewSignal.Deleted(ctx, viewName, id, before, 0)
	return nil
}

// dualApply runs a shadow-slot write with a bounded retry. On exhaustion it
// aborts the rebuild — clearing the shadow flag cluster-wide — instead of
// failing the live path: the active write already succeeded and the offset
// advances, so a shadow that cannot be kept current is abandoned, not flipped.
func (s *SyncEngine) dualApply(ctx context.Context, viewName string, write func() error) error {
	return dualApplyShadow(ctx, s.eng, s.resolver, viewName, write)
}

// dualApplyShadow is the shared shadow-write discipline behind SyncEngine's
// dual-apply, also used by the UpstreamSubscriber's recompose-ripple: EVERY
// writer of a view document must reach the shadow slot during a rebuild window,
// or the flipped collection silently misses the writes that landed only on the
// retiring active slot.
//
// It returns the error rather than swallowing it, so a caller on the event path
// FAILS THE EVENT and the message is retried — with the shadow write among the
// obligations replayed. That is the cheap, local recovery.
//
// Abandoning the rebuild is the expensive, cluster-wide one, and it is now taken
// on EVIDENCE: only after shadowAbortThreshold consecutive events failed to
// reach this view's shadow. Previously three attempts spanning 150 ms of ONE
// event could discard hours of backfill for every pod — a transient Mongo blip
// priced as a permanent verdict.
func dualApplyShadow(ctx context.Context, eng core.RelationalEngine, resolver *ViewResolver, viewName string, write func() error) error {
	var err error
	for attempt := 0; attempt < shadowWriteRetries; attempt++ {
		if err = write(); err == nil {
			shadowHealth.succeeded(viewName)
			return nil
		}
		if attempt < shadowWriteRetries-1 {
			select {
			case <-time.After(shadowWriteBackoff):
			case <-ctx.Done():
				return err
			}
		}
	}
	streak := shadowHealth.failed(viewName)
	shadowHealth.metrics.inc(MetricProjectionShadowWriteFailed)
	slog.WarnContext(ctx, "projection.shadow_write.failed",
		slog.String("view", viewName),
		slog.Int("attempts", shadowWriteRetries),
		slog.Int("consecutiveFailingEvents", streak),
		slog.String("err", errString(err)))
	if streak < shadowAbortThreshold {
		return err
	}
	slog.ErrorContext(ctx, "projection.rebuild.abandoned",
		slog.String("view", viewName),
		slog.Int("consecutiveFailingEvents", streak),
		slog.String("reason", "shadow slot unreachable"))
	shadowHealth.succeeded(viewName) // reset: the flag is gone, the streak is meaningless
	if aerr := abortSlotRebuild(ctx, eng.Querier(), eng.Dialect(), viewName); aerr != nil {
		slog.ErrorContext(ctx, "projection.rebuild.abandon_failed",
			slog.String("view", viewName), slog.String("err", aerr.Error()))
		return err
	}
	if rerr := resolver.Refresh(ctx); rerr != nil {
		slog.ErrorContext(ctx, "projection.rebuild.refresh_after_abandon_failed",
			slog.String("view", viewName), slog.String("err", rerr.Error()))
	}
	return err
}

// shadowAbortThreshold is how many CONSECUTIVE events must fail to reach a
// view's shadow slot before the rebuild is abandoned cluster-wide. Sized well
// above any plausible transient (each event has already exhausted its own
// shadowWriteRetries, and the event itself is retried on top of that) and well
// below "an operator will notice first".
const shadowAbortThreshold = 20

// shadowHealth tracks the consecutive shadow-write failure streak per view. It
// is process-wide because the thing it guards is process-wide: every writer of a
// view document — SyncEngine, the recompose-ripple, the surgical embed writer —
// shares one verdict about whether that view's shadow is reachable.
var shadowHealth = &shadowFailureTracker{streaks: map[string]int{}, metrics: newProjectionMetrics()}

type shadowFailureTracker struct {
	mu      sync.Mutex
	streaks map[string]int
	// metrics counts exhausted shadow writes process-wide. Shadow health is a
	// property of the view, not of one engine, so its counter lives with the
	// tracker rather than on a SyncEngine.
	metrics *projectionMetrics
}

// failed records one fully-exhausted shadow write and returns the new streak.
func (t *shadowFailureTracker) failed(viewName string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streaks[viewName]++
	return t.streaks[viewName]
}

// succeeded clears the streak: one reachable write proves the shadow is alive,
// so the evidence for abandoning it is gone.
func (t *shadowFailureTracker) succeeded(viewName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, tracked := t.streaks[viewName]; tracked {
		delete(t.streaks, viewName)
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
		// Terminal by nature — retrying a parse failure is pure waste — but NOT
		// silent: it is parked, so a corrupt payload is a visible, queryable defect
		// instead of a gap nobody can name.
		s.parkEvent(ctx, event, errors.New("undecodable payload"))
		return nil
	}

	ids := rawPayloadIDs(raw)

	// One event carries several INDEPENDENT obligations over DIFFERENT documents,
	// and they are discharged here in a deliberate order with deliberate error
	// handling. Both used to be wrong, and together they cost an aggregate:
	//
	//   - ORDER. The event's OWN document (O1) ran LAST, after the shared-base
	//     fan-out probe. That probe reads the view's active slot, so when a
	//     blue-green flip retired that collection mid-read the server killed the
	//     query, the error propagated, and the aggregate's own projection — the
	//     one thing only this event can supply — never ran at all.
	//   - ISOLATION. The first error returned, abandoning every obligation after
	//     it. The fan-out is a PUSH to sibling role documents; its failure says
	//     nothing about whether this event's own document can be written.
	//
	// So: O1 first, and every obligation is attempted even when a sibling failed.
	// The collected error is what the caller retries on — and because every
	// obligation is idempotent (guarded pipelines, guarded deletes, advance-only
	// registry stamps), the retry replays the whole message and the ones that
	// already succeeded are no-ops. That is why there is no per-obligation replay
	// machinery here.
	//
	// "Independent" has exactly one exception, marked below: the base-revision
	// stamp must precede the fan-out probe, so a failed stamp BLOCKS the probe
	// rather than merely being recorded beside it.
	var errs []error
	fail := func(what string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}

	// O1 — the event's OWN documents. First, and never skipped for someone
	// else's failure.
	views := s.index.byPGTable[event.AggregateType]
	fail("own views", s.projectOwnViews(ctx, event, raw, ids, views))

	// O2, base-table trigger: a SharedBase change recomposes every role view's
	// document that references the changed identity. The event's aggregate_id IS
	// the base id here. Idempotent, so overlapping with the role trigger below
	// across one identity's writes is harmless.
	if baseViews, ok := s.index.bySharedBase[event.AggregateType]; ok {
		fail("shared-base fan-out", s.fanOutSharedBase(ctx, event.AggregateID, baseViews))
		// The purge destroyed the identity: drop its registry base-revision record.
		// (A zombie role event may recreate it later — inert garbage, no role
		// document exists to pull from it; the TTL-less base-revision record is
		// bounded by the purge volume.)
		if event.EventType == "DELETED" && ids.BasePurged {
			fail("drop base revision", s.dropBaseRevision(ctx, event.AggregateType, event.AggregateID))
		}
	}

	// O2, role trigger. THE DEPENDENCY EDGE: the base revision is stamped into
	// the registry BEFORE the fan-out probes for target documents. If the probe
	// misses a document still being born, that document's writer — whose own
	// write necessarily enters the store after our probe, hence after this stamp
	// — finds the newer base revision on its post-write pull check and repairs.
	// A failed stamp dissolves that premise, so the probe is NOT attempted:
	// running it anyway would silently lose the late-born document instead of
	// deferring it to the retry.
	roleBaseTable, isRole := s.index.baseOfRole[event.AggregateType]
	if isRole && ids.BaseID != "" {
		if err := s.stampBaseRevision(ctx, roleBaseTable, ids.BaseID, ids.BaseRevision); err != nil {
			fail("stamp base revision (shared-base fan-out blocked)", err)
		} else if baseViews, ok := s.index.bySharedBase[roleBaseTable]; ok {
			fail("shared-base payload fan-out", s.fanOutSharedBasePayload(ctx, raw, ids.BaseID, baseViews))
		}
	}

	// O3 — the INVERSE direction: an event on a ROLE table recomposes the person
	// document of every base-rooted SharedBaseView declaring that role. All event
	// types route here — including ARCHIVED/UNARCHIVED (the base convergence is
	// SQL without its own outbox row, so the role event is the only signal) and
	// DELETED (the segment must flip to null, or the whole document must go when
	// the identity was purged).
	if routes, ok := s.index.byRoleTable[event.AggregateType]; ok {
		fail("base-rooted recompose", s.recomposeBaseRooted(ctx, event, routes))
	}

	// HANDSHAKE, pull side. Defined as a POST-WRITE check, so it stays after O1.
	if isRole && ids.BaseID != "" && ids.BaseRevision > 0 &&
		(event.EventType == "INSERTED" || event.EventType == "UNARCHIVED") {
		fail("base-revision pull repair", s.pullSideRepair(ctx, event, ids, roleBaseTable, views))
	}

	return errors.Join(errs...)
}

// projectOwnViews discharges O1: the event's own document in every view rooted
// at its table. One view's failure does not abandon the others — the same
// isolation principle as process, one level down.
func (s *SyncEngine) projectOwnViews(ctx context.Context, event kafkaEvent, raw map[string]any, ids payloadIDs, views []*ViewDefinition) error {
	var errs []error
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
				errs = append(errs, fmt.Errorf("delete from %q: %w", view.name, err))
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
		if !view.isSharedBaseView && len(view.embeds) == 0 && len(view.childEmbeds) == 0 {
			stages := buildProjectionStages(view.schema, coercePayloadEvent(view.schema, raw))
			if len(stages) == 0 {
				continue
			}
			if err := s.applyProjection(ctx, view.name, event.AggregateID, stages, true); err != nil {
				errs = append(errs, fmt.Errorf("project %q: %w", view.name, err))
				continue
			}
			if err := s.checkTombstone(ctx, view.name, event.AggregateID, ids.Revision); err != nil {
				errs = append(errs, fmt.Errorf("tombstone check %q: %w", view.name, err))
			}
			continue
		}
		doc, err := s.composer.Compose(ctx, view, event.AggregateID)
		if err != nil {
			errs = append(errs, fmt.Errorf("compose %q: %w", view.name, err))
			continue
		}
		if doc == nil {
			continue
		}
		if err := s.applyConsultUpsert(ctx, view, event.AggregateID, doc); err != nil {
			errs = append(errs, fmt.Errorf("consult upsert %q: %w", view.name, err))
		}
	}
	return errors.Join(errs...)
}

// pullSideRepair is the pull half of the base-revision handshake. A document
// this event just MATERIALIZED (INSERTED, or UNARCHIVED re-creating under
// DeleteOnArchive) may carry base state older than a fan-out that could not find
// it — the fan-out's FindIDsByField snapshot predates the document. The
// registry's base-revision record proves it: when a newer revision was pushed,
// repair by consult. The composed closure is read fresh from the relational and
// applied guarded, so the repair can never regress anything. The store's write
// order makes the two sides meet: if the fan-out's probe missed this document,
// this pull necessarily sees that fan-out's stamp.
func (s *SyncEngine) pullSideRepair(ctx context.Context, event kafkaEvent, ids payloadIDs, roleBaseTable string, views []*ViewDefinition) error {
	stamped, err := s.stampedBaseRevision(ctx, roleBaseTable, ids.BaseID)
	if err != nil {
		return err
	}
	if stamped <= ids.BaseRevision {
		return nil
	}
	var errs []error
	for _, view := range views {
		doc, err := s.composer.Compose(ctx, view, event.AggregateID)
		if err != nil {
			errs = append(errs, fmt.Errorf("compose %q: %w", view.name, err))
			continue
		}
		if doc == nil {
			continue
		}
		if err := s.applyConsultUpsert(ctx, view, event.AggregateID, doc); err != nil {
			errs = append(errs, fmt.Errorf("consult upsert %q: %w", view.name, err))
		}
	}
	return errors.Join(errs...)
}

// fanOutSharedBase recomposes every role-view document that references a changed
// shared identity (baseID — from a base event's aggregate_id or a role event's
// _ids.base_id). For each role view embedding the base, it finds the role docs
// whose ParentID equals the base id (index-only via FindIDsByField on the role's link
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
		slog.WarnContext(ctx, "projection.role_event.unresolvable_base_id",
			slog.String("eventType", event.EventType),
			slog.String("aggregateType", event.AggregateType),
			slog.String("aggregateId", event.AggregateID))
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
