package query

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// defaultUpstreamCommitInterval mirrors SyncEngine's CommitInterval —
// switches kafka-go from per-message sync commits to async batches every
// second. Safe under at-least-once because the local Upsert and the
// recompose ripple are both idempotent against retried messages.
const defaultUpstreamCommitInterval = time.Second

// UpstreamSubscriberConfig is the infra-level flattened view of a
// bootstrap.UpstreamSubscription. bootstrap converts the operator-facing
// shape to this so infra/ has zero dependency on the bootstrap package.
//
// Fields mirror UpstreamSubscription one-to-one; StartFrom and
// OnUpstreamDelete are kept as raw strings because the runtime only
// branches on the symbolic values and accepts the "offset:<N>" template.
type UpstreamSubscriberConfig struct {
	Topic            string
	Collection       string
	ConsumerGroup    string
	Workers          int
	Filter           []string
	DeleteOnArchive  bool
	StartFrom        string
	OnUpstreamDelete string
	AnonymizeFields  []string
}

// Symbolic raw values consumed by UpstreamSubscriber.dispatch. Kept here
// (rather than imported from bootstrap) so the runtime is self-contained.
const (
	upstreamStartFromLatest        = "latest"
	upstreamStartFromEarliest      = "earliest"
	upstreamDeletePolicyCascade    = "cascade"
	upstreamDeletePolicyAnonymize  = "anonymize"
	upstreamDeletePolicyKeep       = "keep"
	upstreamStartFromOffsetPrefix  = "offset:"
	upstreamSubscriberWorkerDepth  = 4
	upstreamRecomposeStageDiscover = "discover"
	upstreamRecomposeStageCompose  = "compose"
	upstreamRecomposeStageUpsert   = "upsert"
)

// upstreamMetrics counts failures by (subscription, view, stage). Exposed
// via a Snapshot accessor for tests and any future Prometheus adapter.
// The framework keeps the counter in-memory and increments atomically; a
// real metric sink (Prometheus, OpenTelemetry) plugs in by reading the
// snapshot and re-exporting via the operator's chosen library.
type upstreamMetrics struct {
	mu       sync.Mutex
	failures map[string]uint64 // key: "topic|view|stage"
}

func newUpstreamMetrics() *upstreamMetrics {
	return &upstreamMetrics{failures: map[string]uint64{}}
}

func (m *upstreamMetrics) inc(topic, view, stage string) {
	if m == nil {
		return
	}
	key := topic + "|" + view + "|" + stage
	m.mu.Lock()
	m.failures[key]++
	m.mu.Unlock()
}

// Snapshot returns a copy of the counter map keyed by "topic|view|stage".
// Used by tests to assert failure-isolation behavior; production sinks
// poll this on a ticker.
func (m *upstreamMetrics) Snapshot() map[string]uint64 {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]uint64, len(m.failures))
	for k, v := range m.failures {
		out[k] = v
	}
	return out
}

// UpstreamSubscriber materializes one upstream topic into a local Mongo
// collection and triggers downstream recompose-ripple on every view that
// embeds the collection via an external query.JoinUpstream. One instance per
// bootstrap.UpstreamSubscription declared in the service Wiring / yaml.
//
// The subscriber's lifecycle is owned by bootstrap.Run, which spawns one
// supervisor goroutine per subscription and shares the same ctx as the
// SyncEngine. Shutdown follows the same drain shape: ctx.Done() →
// supervisor closes worker channels → workers drain in-flight events →
// Kafka reader closes.
//
// Failure isolation (§7.3): every recompose error is logged + counted +
// skipped. The Kafka offset still advances at the end of the message —
// stale documents are recoverable through the next upstream event for
// the affected entity or through an operator-triggered rebuild. Blocking
// the consumer group on a deterministic compose error would turn a
// recoverable degradation into a poison pill.
type UpstreamSubscriber struct {
	// eng is the relational engine the side-channel failure registry
	// (omnicore_projection_failures, kind=ripple) is read/written through, via the neutral
	// Querier + core.Dialect; the recompose ripple itself is Mongo + the composer.
	eng      core.RelationalEngine
	mongo    ReadModelStore
	composer *Composer
	// resolver maps this subscription's own collection and each dependent
	// view name to the physical collection it currently resolves to; shared
	// process-wide so every read-model component observes one pointer view.
	resolver       *ViewResolver
	cfg            UpstreamSubscriberConfig
	dependentViews []*ViewDefinition
	// hasManyEmbed is true when at least one dependent view embeds this
	// subscription's collection via a one-to-many EmbedMany. It gates the extra
	// "read the doc before the change" step the 1:N recompose-ripple needs (to
	// learn which parent the changed child belonged to); a subscription feeding
	// only one-to-one Embeds skips that read entirely, so its behavior is
	// byte-identical to before this path existed.
	hasManyEmbed bool
	// sub is the transport port this subscriber opens its consumer subscription
	// through — the seam that keeps the ripple loop broker-neutral.
	sub              transport.Subscriber
	logger           *slog.Logger
	metrics          *upstreamMetrics
	offsetSeekTarget *int64 // populated when cfg.StartFrom is "offset:<N>"

	// inflight counts in-flight processMessage invocations. Drained by
	// Shutdown so coordinated shutdown can wait for every in-flight ripple
	// to commit (or the drain timeout to elapse) before infra deps close.
	inflight sync.WaitGroup
	// stop signals the supervisor loop to exit. Closed by Shutdown so the
	// reader.ReadMessage call unblocks promptly without waiting for the
	// shared shutdown context to time out at the Kafka socket level.
	stop     chan struct{}
	stopOnce sync.Once
	// done closes when the supervisor loop (run) has FULLY exited — worker
	// drain (wg.Wait, which subsumes every in-flight processMessage) and
	// reader Close() (the Kafka LeaveGroup) included, in that dependency
	// order. Shutdown waits on it when the subscriber was Started, so the
	// process never exits with the LeaveGroup still in flight.
	done      chan struct{}
	startOnce sync.Once
	started   atomic.Bool
	// traceKafka gates the per-message consumer span (the tracing `kafka`
	// instrument toggle). bootstrap sets it via WithKafkaTracing; false (the
	// default) leaves the ripple loop untraced and pays nothing.
	traceKafka bool
	// viewSignal chains this subscriber's ripple into the view→view fan-out:
	// a mirror change refreshes view Y, and Y may itself be materialized into X
	// (query.JoinView), so Y's refreshed document must signal onward. Installed
	// by WithViewChaining; nil (the default) makes the chain a no-op.
	viewSignal *viewEmbedSignal
	// syncGroup is the SYNC consumer group that owns this subscriber's ledger
	// rows (installed by WithViewChaining). Falls back to the subscription's
	// own group in the degenerate no-views boot, where no parked-retry loop
	// exists to replay them but the rows stay queryable.
	syncGroup string
}

// WithViewChaining connects this subscriber's recompose ripple to the view→view
// fan-out owned by the SyncEngine, so a chain upstream → Y → X propagates: the
// ripple refreshes Y's document, and that write signals every view materializing
// Y. Without it the chain would stop at the first hop and X would drift.
//
// Takes the engine (rather than the fan-out) so the two writers of a view
// document always share ONE instance — there is no way to wire a second,
// divergent fan-out by accident. A nil engine, or an engine whose view set
// embeds no view, leaves chaining off.
func (s *UpstreamSubscriber) WithViewChaining(engine *SyncEngine) *UpstreamSubscriber {
	if engine != nil {
		s.viewSignal = engine.viewSignal
		// The unified ledger: this subscriber's ripple failures are recorded
		// under the SYNC group (the parked-retry loop replays rows scoped to
		// that group), and the engine learns how to replay this topic's rows —
		// re-run the ripple for the source id against CURRENT state, exactly
		// what a live event would do.
		s.syncGroup = engine.groupID
		engine.registerRippleReplayer(s.cfg.Topic, func(ctx context.Context, sourceID string) {
			current := s.readLocalDoc(ctx, sourceID)
			s.ripple(ctx, sourceID, nil, current)
		})
	}
	return s
}

// WithKafkaTracing enables the consumer span on each processed message. bootstrap
// passes tracing.Instruments(SubKafka); off (the default) keeps the loop untraced.
func (s *UpstreamSubscriber) WithKafkaTracing(on bool) *UpstreamSubscriber {
	s.traceKafka = on
	return s
}

// NewUpstreamSubscriber wires the subscriber. dependentViews is the slice
// of B views that embed cfg.Collection via an external JoinUpstream — bootstrap looks
// this up from viewIndex.byMongoColl after collectViews returns and
// passes the result here so the subscriber's per-message recompose loop
// is index-only.
//
// composer can be a freshly-built NewComposerWithMongo or a shared
// instance (e.g. SyncEngine's). Both paths work; sharing avoids an extra
// allocation but is not required.
//
// logger and sub come from the framework's already-built singletons.
func NewUpstreamSubscriber(
	eng core.RelationalEngine,
	mongo ReadModelStore,
	composer *Composer,
	resolver *ViewResolver,
	cfg UpstreamSubscriberConfig,
	dependentViews []*ViewDefinition,
	sub transport.Subscriber,
	logger *slog.Logger,
) (*UpstreamSubscriber, error) {
	if cfg.Topic == "" {
		return nil, fmt.Errorf("upstream subscriber: cfg.Topic is required")
	}
	if cfg.Collection == "" {
		return nil, fmt.Errorf("upstream subscriber: cfg.Collection is required")
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.StartFrom == "" {
		cfg.StartFrom = upstreamStartFromLatest
	}
	if cfg.OnUpstreamDelete == "" {
		cfg.OnUpstreamDelete = upstreamDeletePolicyCascade
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &UpstreamSubscriber{
		eng:            eng,
		mongo:          mongo,
		composer:       composer,
		resolver:       resolver,
		cfg:            cfg,
		dependentViews: dependentViews,
		sub:            sub,
		logger:         logger,
		metrics:        newUpstreamMetrics(),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	for _, v := range dependentViews {
		if anyManyEmbedOf(v.Embeds(), cfg.Collection) {
			s.hasManyEmbed = true
			break
		}
	}
	if err := s.parseOffsetSeek(); err != nil {
		return nil, err
	}
	return s, nil
}

// Metrics exposes the failure-isolation counter. Returns the underlying
// pointer so consumers wire it to their metric system without copying;
// concurrent reads are safe through Snapshot.
func (s *UpstreamSubscriber) Metrics() *upstreamMetrics { return s.metrics }

// Pending ripple failures for this subscriber's topic are replayed by the
// SyncEngine's parked-retry loop (the mongo.parkedRetry knob), through the
// replayer registered in WithViewChaining — the same driver that replays
// parked events, so the ledger has ONE clock. On retry there is no
// before/after event pair; the replayer reads the current local doc so a 1:N
// EmbedMany can still resolve its parent (a 1:1 embed rediscovers by
// FindIDsByField on the source id), which is exactly what a live upstream
// event would do.

// parseOffsetSeek extracts the numeric N from a "offset:N" StartFrom
// value. Symbolic values leave offsetSeekTarget nil. Validation is
// belt-and-suspenders: bootstrap.validateUpstreamSubscriptions already
// rejected malformed values at boot.
func (s *UpstreamSubscriber) parseOffsetSeek() error {
	if !strings.HasPrefix(s.cfg.StartFrom, upstreamStartFromOffsetPrefix) {
		return nil
	}
	raw := strings.TrimPrefix(s.cfg.StartFrom, upstreamStartFromOffsetPrefix)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf(
			"upstream subscriber: StartFrom=%q invalid offset (parse %q: %v)",
			s.cfg.StartFrom, raw, err,
		)
	}
	s.offsetSeekTarget = &n
	return nil
}

// Start runs the supervisor in a goroutine and returns immediately. Use
// to integrate with bootstrap.Run's lifecycle (which spawns one Start
// per subscription and drains via Shutdown). Returns nothing because
// the goroutine logs its own progress + failures. Idempotent — a second
// call is a no-op (guarding the done channel against a double close).
func (s *UpstreamSubscriber) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		s.started.Store(true)
		go func() {
			// done closes only after run() returned — after its deferred
			// chain: worker queues drained (wg.Wait, every in-flight
			// processMessage finished) → r.Close() (LeaveGroup sent).
			// Coordination is by dependency, never by timing.
			defer close(s.done)
			s.run(ctx)
		}()
	})
}

func (s *UpstreamSubscriber) run(ctx context.Context) {
	// CommitInterval matches SyncEngine — async commits batched each second.
	// Safe under at-least-once because Upsert is idempotent and recompose is
	// deterministic from current relational + Mongo state.
	var startFrom string
	switch s.cfg.StartFrom {
	case upstreamStartFromLatest, "":
		startFrom = transport.StartFromLatest
	default:
		// earliest — and the "offset:<N>" fallback, which under kafka-go's
		// unset StartOffset defaulted to the earliest offset — both begin at
		// the earliest offset (the committed group offset wins once present).
		startFrom = transport.StartFromEarliest
	}
	// "offset:<N>" is a coordinated-PITR shape and a Kafka-specific concept:
	// there is no per-message Seek API on a consumer-group reader, and the
	// consumer group's committed offset wins anyway. Spec §7.4 documents the
	// operator-side reset flow (Kafka: `kafka-consumer-groups.sh --reset-offsets
	// --to-offset N`). Transports without numeric offsets (e.g. NATS JetStream)
	// have no analogue and resume from the durable's committed position instead.
	// Here we log a warning so any boot under that posture surfaces the manual
	// step rather than silently ignoring the requested offset.
	if s.offsetSeekTarget != nil {
		s.logger.Warn("upstream subscriber: StartFrom=offset:N requires an "+
			"external offset reset via the broker's admin tooling (Kafka: "+
			"kafka-consumer-groups.sh); the framework does NOT auto-seek, and "+
			"transports without numeric offsets ignore N",
			"topic", s.cfg.Topic,
			"consumerGroup", s.cfg.ConsumerGroup,
			"offset", *s.offsetSeekTarget)
	}

	sub, err := s.sub.Subscribe(ctx, transport.SubscribeConfig{
		Topics:         []string{s.cfg.Topic},
		GroupID:        s.cfg.ConsumerGroup,
		StartFrom:      startFrom,
		CommitInterval: defaultUpstreamCommitInterval,
	})
	if err != nil {
		s.logger.Error("upstream subscriber: subscribe failed",
			"topic", s.cfg.Topic, "consumerGroup", s.cfg.ConsumerGroup, "err", err)
		return
	}
	defer sub.Close()

	queues := make([]chan queuedUpstream, s.cfg.Workers)
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		queues[i] = make(chan queuedUpstream, upstreamSubscriberWorkerDepth)
		wg.Add(1)
		go func(q <-chan queuedUpstream, idx int) {
			defer wg.Done()
			for qm := range q {
				s.inflight.Add(1)
				s.processMessage(ctx, qm.msg, idx)
				s.inflight.Done()
				// processMessage is total: every failure it can suffer is already
				// recorded in the unified ledger and replayed by the parked-retry loop,
				// which is this subscriber's parking mechanism. So the message is
				// genuinely finished from the transport's point of view — the work
				// either landed or is durably queued for repair.
				_ = qm.completion.Done(ctx)
			}
		}(queues[i], i)
	}
	defer func() {
		for _, q := range queues {
			close(q)
		}
		wg.Wait()
	}()

	s.logger.Info("upstream subscriber started",
		"topic", s.cfg.Topic,
		"collection", s.cfg.Collection,
		"consumerGroup", s.cfg.ConsumerGroup,
		"workers", s.cfg.Workers,
		"dependentViews", len(s.dependentViews))

	for {
		select {
		case <-s.stop:
			return
		default:
		}
		msg, completion, err := sub.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-s.stop:
				return
			default:
			}
			s.logger.Error("upstream subscriber: read error",
				"topic", s.cfg.Topic, "err", err)
			continue
		}
		select {
		case queues[bucketOf(decodeAggregateID(msg.Key), s.cfg.Workers)] <- queuedUpstream{msg: msg, completion: completion}:
		case <-ctx.Done():
			_ = completion.Failed(ctx)
			return
		case <-s.stop:
			// Shutting down before this message was dispatched: hand it back so
			// the next boot receives it, rather than confirming work never done.
			_ = completion.Failed(ctx)
			return
		}
	}
}

// Shutdown signals the supervisor to stop reading new messages and waits
// for every in-flight processMessage to finish (or for drainCtx to expire,
// whichever comes first). Returns drainCtx.Err() on timeout so the
// coordinated drain in bootstrap can surface partial drains in a single
// shutdown summary. Safe to call from any goroutine; idempotent against
// multiple invocations (Shutdown via close-once on the stop channel).
//
// Two gaps this closes, both coordinated by DEPENDENCY (never timing):
//   - a SIGTERM mid-ripple would drop the in-flight recompose on the floor,
//     leaving stale Mongo docs without a ledger row to
//     surface them — waiting the worker drain guarantees every in-flight
//     ripple either completes (offset advances; failure registry updated;
//     Mongo matches PG) or surfaces as a slog.Warn drain timeout;
//   - the process could exit while the reader's LeaveGroup was still in
//     flight, leaving a ghost member holding the consumer-group slot — the
//     next boot's JoinGroup then blocks until the session times the ghost
//     out. Waiting the supervisor's exit (whose deferred chain drains the
//     workers, THEN closes the reader) guarantees the LeaveGroup went out
//     before Shutdown unblocks.
//
// A subscriber that was never Started (unit-test construction) falls back to
// draining the inflight counter only — there is no supervisor to wait for.
func (s *UpstreamSubscriber) Shutdown(drainCtx context.Context) error {
	if s == nil {
		return nil
	}
	// Signal the supervisor to exit at the next loop iteration. sync.Once
	// guards against repeated Shutdown calls panicking on a closed channel.
	s.stopOnce.Do(func() { close(s.stop) })
	if s.started.Load() {
		// The supervisor's exit subsumes the worker drain (inflight included)
		// AND the reader close — one wait covers the whole dependency chain.
		select {
		case <-s.done:
			return nil
		case <-drainCtx.Done():
			return drainCtx.Err()
		}
	}
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-drainCtx.Done():
		return drainCtx.Err()
	}
}

// queuedUpstream is one dispatched upstream message plus the handle that
// reports its outcome to the transport. The completion travels with the message
// so the confirmation is issued by the worker that finished the work, never by
// the reader that merely saw it.
type queuedUpstream struct {
	msg        transport.Message
	completion transport.Completion
}

// processMessage handles one Kafka message end to end: decode, dispatch
// by event type, perform the Mongo write, trigger recompose ripple. All
// failures are logged + counted on the metric and skipped — the function
// always returns control to the worker loop so the offset can advance.
func (s *UpstreamSubscriber) processMessage(ctx context.Context, msg transport.Message, workerIdx int) {
	_ = workerIdx // reserved for per-worker tagging if instrumentation grows
	bumpUpstreamSubscriberCounter()
	event := extractEvent(msg)
	ctx, span := tracing.StartConsumerSpanIf(s.traceKafka, ctx,
		"github.com/ClaudioSchirmer/omnicore/infra/upstream",
		"upstream "+event.AggregateType, event.Traceparent)
	defer span.End()
	if event.AggregateID == "" || event.EventType == "" {
		s.logger.Warn("upstream subscriber: incomplete metadata, skipping",
			"topic", s.cfg.Topic,
			"key", string(msg.Key),
			"eventType", event.EventType)
		return
	}
	// DELETED is dispatched on the aggregate id ALONE, before any payload
	// decode. A current hard-delete's outbox row carries a keys-only payload
	// (PK + shared-base FK), but rows produced before that change — and any
	// replay of them — carry NULL, which the CDC pipeline surfaces as a JSON
	// scalar (not an object) whose decode into a map fails. Since
	// dispatchDelete needs only the id (cascade/anonymize/keep all key off
	// it), gating the decode behind the payload-bearing verbs keeps the delete
	// cascade from being skipped by a decode error on a payload-less event.
	if event.EventType == "DELETED" {
		s.dispatchDelete(ctx, event.AggregateID)
		return
	}
	payload, err := s.decodePayload(msg.Value)
	if err != nil {
		s.logger.Error("upstream subscriber: payload decode failed",
			"topic", s.cfg.Topic, "id", event.AggregateID, "err", err)
		return
	}
	if len(s.cfg.Filter) > 0 {
		payload = s.applyFilter(payload)
	}
	switch event.EventType {
	case "INSERTED", "UPDATED", "UNARCHIVED":
		s.upsertAndRipple(ctx, event.AggregateID, payload)

	case "ARCHIVED":
		if s.cfg.DeleteOnArchive {
			s.deleteAndRipple(ctx, event.AggregateID)
		} else {
			// Mirror the doc-survives-with-deleted_at semantic: an ARCHIVED
			// outbox row carries the full field payload with the soft-delete
			// column populated (the write side's ARCHIVED payload), so the upsert
			// lands the archived state on the local document.
			s.upsertAndRipple(ctx, event.AggregateID, payload)
		}

	default:
		// Unknown event types are not an error — the upstream may emit
		// custom verbs the consumer chose not to handle. Log at info
		// so the operator can decide if a handler is missing.
		s.logger.Info("upstream subscriber: unrecognized event type, skipping",
			"topic", s.cfg.Topic, "id", event.AggregateID, "eventType", event.EventType)
	}
}

// upsertAndRipple writes the filtered payload to the local Mongo
// collection keyed by aggregate_id, then triggers recompose-ripple on
// every dependent view. Used by INSERTED / UPDATED / UNARCHIVED / soft
// ARCHIVED.
func (s *UpstreamSubscriber) upsertAndRipple(ctx context.Context, id string, payload bson.M) {
	// Read the pre-change doc first: a 1:N EmbedMany needs the OLD parent id (the
	// child's prior FK value) to recompose a parent the child just moved away
	// from. No-op read for one-to-one-only subscriptions.
	before := s.readLocalDoc(ctx, id)
	if err := s.mongo.Upsert(ctx, s.resolver.Active(s.cfg.Collection), id, payload); err != nil {
		s.logger.Error("upstream subscriber: local upsert failed",
			"topic", s.cfg.Topic, "collection", s.cfg.Collection, "id", id, "err", err)
		return
	}
	s.ripple(ctx, id, before, payload)
}

// deleteAndRipple removes the local doc and triggers recompose-ripple.
// Used by hard ARCHIVED (DeleteOnArchive=true) and by DELETED under the
// cascade policy.
func (s *UpstreamSubscriber) deleteAndRipple(ctx context.Context, id string) {
	// Capture the doc before deleting it: a 1:N EmbedMany learns which parent to
	// recompose (drop the child from its array) from the child's FK value, which
	// is gone once the doc is deleted. No-op read for one-to-one-only subscriptions.
	before := s.readLocalDoc(ctx, id)
	if err := s.mongo.Delete(ctx, s.resolver.Active(s.cfg.Collection), id); err != nil {
		s.logger.Error("upstream subscriber: local delete failed",
			"topic", s.cfg.Topic, "collection", s.cfg.Collection, "id", id, "err", err)
		return
	}
	s.ripple(ctx, id, before, nil)
}

// dispatchDelete routes a DELETED event by the configured
// OnUpstreamDelete policy. See §9 for the rationale of each branch.
func (s *UpstreamSubscriber) dispatchDelete(ctx context.Context, id string) {
	switch s.cfg.OnUpstreamDelete {
	case upstreamDeletePolicyCascade:
		s.deleteAndRipple(ctx, id)

	case upstreamDeletePolicyAnonymize:
		before := s.readLocalDoc(ctx, id)
		blanked := make(bson.M, len(s.cfg.AnonymizeFields))
		for _, f := range s.cfg.AnonymizeFields {
			blanked[f] = nil
		}
		if err := s.mongo.UpdateFields(ctx, s.resolver.Active(s.cfg.Collection), id, blanked); err != nil {
			s.logger.Error("upstream subscriber: anonymize failed",
				"topic", s.cfg.Topic, "id", id, "err", err)
			return
		}
		// Ripple with the RETAINED post-update doc as the after state: the
		// anonymized mirror doc still embeds (with blanked fields) — a nil
		// after would read as a mirror delete and strip the element from its
		// parents. Read it back directly (readLocalDoc is gated on 1:N views
		// and this is needed for 1:1 too).
		var after Document
		if docs, err := s.mongo.FindManyByField(ctx, s.resolver.Active(s.cfg.Collection), "_id", id); err == nil && len(docs) > 0 {
			after = docs[0]
		}
		s.ripple(ctx, id, before, after)

	case upstreamDeletePolicyKeep:
		// No-op on the local collection AND no downstream recompose
		// (the dependent views' embed already resolves to the
		// retained doc, so a recompose would not change the shape).
		s.logger.Info("upstream subscriber: DELETED + onUpstreamDelete=keep — no-op",
			"topic", s.cfg.Topic, "id", id)

	default:
		// Defensive: bootstrap.validateUpstreamSubscriptions rejected
		// invalid values, but a future code path that constructs the
		// config directly could miss it.
		s.logger.Error("upstream subscriber: unknown OnUpstreamDelete policy",
			"topic", s.cfg.Topic, "id", id, "policy", s.cfg.OnUpstreamDelete)
	}
}

// rippleEngine derives the shared embedRippler for this subscription — the
// recompose pass extracted to embed_rippler.go so other source kinds (a view
// embedded via query.JoinView) drive the identical pass. Derived per call from
// the subscriber's current fields (never cached): literal-constructed test
// subscribers stay valid, and the cost is one small struct next to the Mongo
// round trips a ripple performs anyway.
func (s *UpstreamSubscriber) rippleEngine() *embedRippler {
	group := s.syncGroup
	if group == "" {
		group = s.cfg.ConsumerGroup
	}
	r := &embedRippler{
		eng:            s.eng,
		mongo:          s.mongo,
		composer:       s.composer,
		resolver:       s.resolver,
		topic:          s.cfg.Topic,
		group:          group,
		collection:     s.cfg.Collection,
		dependentViews: s.dependentViews,
		logger:         s.logger,
		metrics:        s.metrics,
	}
	if s.viewSignal != nil {
		r.onViewWritten = s.viewSignal.Written
	}
	return r
}

// ripple delegates the downstream recompose pass to the shared embedRippler
// (see embed_rippler.go for the full contract: per-view failure isolation,
// failure registry, surgical edits, dual-apply to a rebuild shadow).
//
// The watermark is 0: an upstream mirror has ONE writer and per-id serialized
// events, so its edits need no revision guard (see the srcRev commentary in
// embed_surgical.go) and the emitted stages stay byte-identical to what this
// path always produced.
func (s *UpstreamSubscriber) ripple(ctx context.Context, upstreamID string, before, after Document) {
	s.rippleEngine().ripple(ctx, upstreamID, before, after, 0)
}

// collectChildMongoEmbeds returns the EmbedInChild declarations of the view whose
// external source is the named collection — the child-array enrichments a change
// to that collection must ripple into.
func collectChildMongoEmbeds(childEmbeds []childEmbedDef, collection string) []childEmbedDef {
	var out []childEmbedDef
	for _, ce := range childEmbeds {
		if ce.leg == nil {
			continue
		}
		if ce.leg.IsMongo() && ce.leg.Collection() == collection {
			out = append(out, ce)
		}
	}
	return out
}

// discoverRippleTargets delegates to the shared embedRippler (see
// embed_rippler.go) — kept as a subscriber method because the unit suite
// drives the discovery contract through the subscriber's surface.
func (s *UpstreamSubscriber) discoverRippleTargets(
	ctx context.Context,
	v *ViewDefinition,
	embeds []embedDef,
	upstreamID string,
	before, after Document,
) ([]string, bool) {
	return s.rippleEngine().discoverRippleTargets(ctx, v, embeds, upstreamID, before, after)
}

// readLocalDoc returns the current local upstream document by id — the source of
// a 1:N EmbedMany's parent id (read BEFORE an upsert/delete to learn the old
// parent, or on a retry to learn the current one). Nil (and no Mongo read) when
// no dependent view embeds this collection 1:N, so a purely one-to-one
// subscription pays nothing.
func (s *UpstreamSubscriber) readLocalDoc(ctx context.Context, id string) Document {
	if !s.hasManyEmbed {
		return nil
	}
	docs, err := s.mongo.FindManyByField(ctx, s.resolver.Active(s.cfg.Collection), "_id", id)
	if err != nil || len(docs) == 0 {
		return nil
	}
	return docs[0]
}

// collectMongoEmbeds returns every embed whose source is the given upstream
// Mongo collection. Embeds are single-level, so this walks the view's top-level
// embeds only — a source has no nested embeds to descend into.
func collectMongoEmbeds(embeds []embedDef, collection string) []embedDef {
	var out []embedDef
	for _, e := range embeds {
		if e.leg == nil {
			continue
		}
		if e.leg.IsMongo() && e.leg.Collection() == collection {
			out = append(out, e)
		}
	}
	return out
}

// anyManyEmbedOf reports whether any embed of the collection is a one-to-many
// EmbedMany — the signal that the subscriber must read the pre-change document.
func anyManyEmbedOf(embeds []embedDef, collection string) bool {
	for _, e := range collectMongoEmbeds(embeds, collection) {
		if e.many {
			return true
		}
	}
	return false
}

// docFieldString extracts doc[field] as a string ("" when the doc is nil, the
// field is absent, or the value is nil).
func docFieldString(doc Document, field string) string {
	if doc == nil {
		return ""
	}
	v, ok := doc[field]
	if !ok || v == nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return fmt.Sprintf("%v", v)
}

// recordFailure / resolveFailures delegate to the shared embedRippler (see
// embed_rippler.go): best-effort, nil-engine-safe side-channel writes to the
// unified failure ledger (kind=ripple) — the contract is unchanged.
func (s *UpstreamSubscriber) recordFailure(
	ctx context.Context,
	viewName, upstreamID, localID string,
	stage ProjectionFailureStage,
	cause error,
) {
	s.rippleEngine().recordFailure(ctx, viewName, upstreamID, localID, stage, cause)
}

func (s *UpstreamSubscriber) resolveFailures(ctx context.Context, viewName, upstreamID string) {
	s.rippleEngine().resolveFailures(ctx, viewName, upstreamID)
}

// joinFieldFor walks v.Embeds() looking for the embed that points at
// s.cfg.Collection via an external JoinUpstream. Used by ripple to compute the Mongo
// query "which docs reference the changed upstream id?".
//
// A view typically declares a single embed per upstream collection, but
// the framework does not prohibit declaring two — the first hit wins.
// The boot guards already rejected views without a join field, so the
// empty-string return is defensive only.
func (s *UpstreamSubscriber) joinFieldFor(v *ViewDefinition) string {
	if jf := findMongoJoinField(v.Embeds(), s.cfg.Collection); jf != "" {
		return jf
	}
	return ""
}

func findMongoJoinField(embeds []embedDef, collection string) string {
	for _, e := range embeds {
		if e.leg != nil && e.leg.IsMongo() && e.leg.Collection() == collection {
			return e.JoinColumn()
		}
	}
	return ""
}

// decodePayload turns the raw outbox payload into the mirror document. Two
// shapes are handled:
//   - a Debezium "payload" envelope (some Debezium configurations leak the
//     schema envelope, wrapping the row under {"payload": ...}) is unwrapped;
//   - framework reserved keys (the "_" namespace: _ids, _children,
//     _base_children) are STRIPPED from the mirror by default — they are
//     routing metadata, not upstream state. A consumer that wants one listed
//     in cfg.Filter keeps it (the allowlist wins).
func (s *UpstreamSubscriber) decodePayload(raw []byte) (bson.M, error) {
	if len(raw) == 0 {
		return bson.M{}, nil
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	if inner, ok := top["payload"].(map[string]any); ok {
		top = inner
	}
	allow := make(map[string]bool, len(s.cfg.Filter))
	for _, f := range s.cfg.Filter {
		allow[f] = true
	}
	for k := range top {
		if strings.HasPrefix(k, "_") && !allow[k] {
			delete(top, k)
		}
	}
	return bson.M(top), nil
}

// applyFilter narrows the payload to the declared allowlist. Anything
// outside the list is dropped — including conventional metadata fields
// (id/created_at/updated_at) unless the operator explicitly lists them.
// Returns a fresh map; the input is not mutated.
func (s *UpstreamSubscriber) applyFilter(payload bson.M) bson.M {
	allow := make(map[string]bool, len(s.cfg.Filter))
	for _, f := range s.cfg.Filter {
		allow[f] = true
	}
	out := make(bson.M, len(allow))
	for k, v := range payload {
		if allow[k] {
			out[k] = v
		}
	}
	return out
}

// upstreamSubscriberCounter exposes the total messages processed across
// all subscribers (sum of every event reaching processMessage). Useful
// for end-to-end smoke tests that want a "did anything flow?" signal
// without coupling to slog parsing.
var upstreamSubscriberCounter atomic.Uint64

func bumpUpstreamSubscriberCounter() { upstreamSubscriberCounter.Add(1) }

// UpstreamMessagesProcessed returns the running total of messages every
// UpstreamSubscriber in the process has dispatched (across topics).
// Used by integration tests; production sinks are expected to read the
// per-subscriber metric via Metrics().Snapshot() instead.
func UpstreamMessagesProcessed() uint64 { return upstreamSubscriberCounter.Load() }
