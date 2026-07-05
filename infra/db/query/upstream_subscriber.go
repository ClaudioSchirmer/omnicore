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

	"github.com/segmentio/kafka-go"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
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
// embeds the collection via an external fwinfra.FromSchema. One instance per
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
	// (omnicore_upstream_failures) is read/written through, via the neutral
	// Querier + core.Dialect; the recompose ripple itself is Mongo + the composer.
	eng              core.RelationalEngine
	mongo            ReadModelStore
	composer         *Composer
	cfg              UpstreamSubscriberConfig
	dependentViews   []*ViewDefinition
	// hasManyEmbed is true when at least one dependent view embeds this
	// subscription's collection via a one-to-many EmbedMany. It gates the extra
	// "read the doc before the change" step the 1:N recompose-ripple needs (to
	// learn which parent the changed child belonged to); a subscription feeding
	// only one-to-one Embeds skips that read entirely, so its behavior is
	// byte-identical to before this path existed.
	hasManyEmbed     bool
	brokers          []string
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
}

// WithKafkaTracing enables the consumer span on each processed message. bootstrap
// passes tracing.Instruments(SubKafka); off (the default) keeps the loop untraced.
func (s *UpstreamSubscriber) WithKafkaTracing(on bool) *UpstreamSubscriber {
	s.traceKafka = on
	return s
}

// NewUpstreamSubscriber wires the subscriber. dependentViews is the slice
// of B views that embed cfg.Collection via an external FromSchema — bootstrap looks
// this up from viewIndex.byMongoColl after collectViews returns and
// passes the result here so the subscriber's per-message recompose loop
// is index-only.
//
// composer can be a freshly-built NewComposerWithMongo or a shared
// instance (e.g. SyncEngine's). Both paths work; sharing avoids an extra
// allocation but is not required.
//
// logger and brokers come from the framework's already-built singletons.
func NewUpstreamSubscriber(
	eng core.RelationalEngine,
	mongo ReadModelStore,
	composer *Composer,
	cfg UpstreamSubscriberConfig,
	dependentViews []*ViewDefinition,
	brokers []string,
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
		cfg:            cfg,
		dependentViews: dependentViews,
		brokers:        brokers,
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

// RetryPendingFailures re-runs the recompose ripple for every distinct
// upstream_id that currently has at least one pending row in
// omnicore_upstream_failures for this subscriber's topic. Returns the
// number of upstream_ids whose ripple was attempted.
//
// Use this when an operator wants to drain accumulated stale docs
// without waiting for the next live upstream event. Typical wiring is
// either an in-process cron (time.Ticker invoking this every N minutes)
// or an authenticated HTTP endpoint that the operator hits manually —
// the consumer service decides the ergonomics; the framework exposes
// only the primitive.
//
// Semantics:
//   - Idempotent. A successful ripple for (view, upstream_id) calls
//     ResolveUpstreamFailures so the pending rows under that coordinate
//     get marked resolved automatically. A second call against the same
//     drained slate is a no-op.
//   - Per-upstream_id deduplication. Multiple pending rows for the same
//     upstream_id (different views, different stages, different local_ids)
//     collapse into one ripple call — ripple already iterates every
//     dependent view and every local doc.
//   - Best-effort writes. RecordFailure / ResolveFailures inside ripple
//     stay slog.Warn-and-discard on a database error, matching the failure
//     isolation contract.
//   - Returns the read error from the relational database only — that is the single failure
//     mode the caller cannot ignore (no list = nothing to retry).
func (s *UpstreamSubscriber) RetryPendingFailures(ctx context.Context) (int, error) {
	if s.eng == nil {
		return 0, fmt.Errorf("retry pending upstream failures: subscriber has no relational engine handle")
	}
	pendings, err := ListPendingUpstreamFailuresByTopic(ctx, s.eng.Querier(), s.eng.Dialect(), s.cfg.Topic)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(pendings))
	for _, p := range pendings {
		if seen[p.UpstreamID] {
			continue
		}
		seen[p.UpstreamID] = true
		// On retry there is no before/after event pair; read the current local doc
		// so a 1:N EmbedMany can still resolve its parent. A one-to-one embed
		// ignores it (it rediscovers by FindIDsByField on the upstream id).
		current := s.readLocalDoc(ctx, p.UpstreamID)
		s.ripple(ctx, p.UpstreamID, nil, current)
		if ctx.Err() != nil {
			return len(seen), ctx.Err()
		}
	}
	return len(seen), nil
}

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
	readerCfg := kafka.ReaderConfig{
		Brokers: s.brokers,
		Topic:   s.cfg.Topic,
		GroupID: s.cfg.ConsumerGroup,
		// CommitInterval matches SyncEngine — async commits batched
		// each second. Safe under at-least-once because Upsert is
		// idempotent and recompose is deterministic from current PG +
		// Mongo state.
		CommitInterval: defaultUpstreamCommitInterval,
	}
	switch s.cfg.StartFrom {
	case upstreamStartFromEarliest:
		readerCfg.StartOffset = kafka.FirstOffset
	case upstreamStartFromLatest, "":
		readerCfg.StartOffset = kafka.LastOffset
	}
	// "offset:<N>" is a coordinated-PITR shape: kafka-go has no
	// per-message Seek API on a consumer-group reader, and the
	// consumer group's committed offset wins anyway. Spec §7.4
	// documents the operator-side `kafka-consumer-groups.sh
	// --reset-offsets --to-offset N` flow; here we log a warning so
	// any boot under that posture surfaces the manual step.
	if s.offsetSeekTarget != nil {
		s.logger.Warn("upstream subscriber: StartFrom=offset:N "+
			"requires external offset reset via kafka-consumer-groups.sh — "+
			"the framework does NOT auto-seek",
			"topic", s.cfg.Topic,
			"consumerGroup", s.cfg.ConsumerGroup,
			"offset", *s.offsetSeekTarget)
	}

	r := kafka.NewReader(readerCfg)
	defer r.Close()

	queues := make([]chan kafka.Message, s.cfg.Workers)
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		queues[i] = make(chan kafka.Message, upstreamSubscriberWorkerDepth)
		wg.Add(1)
		go func(q <-chan kafka.Message, idx int) {
			defer wg.Done()
			for msg := range q {
				s.inflight.Add(1)
				s.processMessage(ctx, msg, idx)
				s.inflight.Done()
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
		msg, err := r.ReadMessage(ctx)
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
		case queues[bucketOf(decodeAggregateID(msg.Key), s.cfg.Workers)] <- msg:
		case <-ctx.Done():
			return
		case <-s.stop:
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
//     leaving stale Mongo docs without an `omnicore_upstream_failures` row to
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

// processMessage handles one Kafka message end to end: decode, dispatch
// by event type, perform the Mongo write, trigger recompose ripple. All
// failures are logged + counted on the metric and skipped — the function
// always returns control to the worker loop so the offset can advance.
func (s *UpstreamSubscriber) processMessage(ctx context.Context, msg kafka.Message, workerIdx int) {
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
			// column populated (write-side softWritePayload), so the upsert
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
	if err := s.mongo.Upsert(ctx, s.cfg.Collection, id, payload); err != nil {
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
	if err := s.mongo.Delete(ctx, s.cfg.Collection, id); err != nil {
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
		if err := s.mongo.UpdateFields(ctx, s.cfg.Collection, id, blanked); err != nil {
			s.logger.Error("upstream subscriber: anonymize failed",
				"topic", s.cfg.Topic, "id", id, "err", err)
			return
		}
		s.ripple(ctx, id, before, nil)

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

// ripple is the downstream recompose pass. For every dependent view it
// asks Mongo "which docs reference the changed upstream id?", recomposes
// each one through the Composer, and upserts the result. Failures are
// per-view isolated: a Composer bug or upsert error on view A does not
// block view B referencing the same upstream entity. Kafka offset still
// advances after this returns — the alternative (block on failure) turns
// a deterministic recompose bug into a poison pill across the whole
// consumer group.
//
// Every failure is persisted to omnicore_upstream_failures alongside the
// in-memory metric so operators have a queryable record of stale docs
// surviving past the consumer group's offset. A view+upstream_id pair
// that completes the full recompose pass without errors triggers
// ResolveUpstreamFailures so prior pending rows for the same coordinate
// are marked as resolved — the failure table mirrors the live state, not
// a monotonically-growing log.
func (s *UpstreamSubscriber) ripple(ctx context.Context, upstreamID string, before, after Document) {
	for _, v := range s.dependentViews {
		embeds := collectMongoEmbeds(v.Embeds(), s.cfg.Collection)
		if len(embeds) == 0 {
			// Defensive — bootstrap.validateUpstreamSubscriptions rejected views
			// with external FromSchema embeds without a join field, and this view
			// is a dependent only because it embeds this collection. If we land
			// here, the view's shape changed at runtime (impossible today) — log
			// and skip.
			s.logger.Error("upstream subscriber: dependent view has no embed of the collection",
				"topic", s.cfg.Topic, "view", v.Name(), "collection", s.cfg.Collection)
			continue
		}
		// Discover the local parent docs to recompose — the UNION across every
		// embed of this collection on the view (a view may embed the same
		// collection both 1:1 and 1:N). See discoverRippleTargets.
		localIDs, discoverErr := s.discoverRippleTargets(ctx, v, embeds, upstreamID, before, after)
		if discoverErr {
			continue
		}
		failed := false
		for _, localID := range localIDs {
			doc, err := s.composer.Compose(ctx, v, localID)
			if err != nil {
				s.metrics.inc(s.cfg.Topic, v.Name(), upstreamRecomposeStageCompose)
				s.logger.Error("upstream.recompose.compose",
					"subscription", s.cfg.Topic,
					"collection", s.cfg.Collection,
					"view", v.Name(),
					"upstreamID", upstreamID,
					"localID", localID,
					"err", err)
				s.recordFailure(ctx, v.Name(), upstreamID, localID, UpstreamFailureStageCompose, err)
				failed = true
				continue
			}
			if doc == nil {
				continue
			}
			if err := s.mongo.Upsert(ctx, v.Name(), localID, doc); err != nil {
				s.metrics.inc(s.cfg.Topic, v.Name(), upstreamRecomposeStageUpsert)
				s.logger.Error("upstream.recompose.upsert",
					"subscription", s.cfg.Topic,
					"collection", s.cfg.Collection,
					"view", v.Name(),
					"upstreamID", upstreamID,
					"localID", localID,
					"err", err)
				s.recordFailure(ctx, v.Name(), upstreamID, localID, UpstreamFailureStageUpsert, err)
				failed = true
				continue
			}
		}
		if !failed {
			s.resolveFailures(ctx, v.Name(), upstreamID)
		}
	}
}

// discoverRippleTargets computes the distinct local parent _ids to recompose for
// one dependent view, unioning every embed of the changed collection:
//
//   - one-to-one Embed: the PARENT holds the FK column, so scan the parent view
//     for docs whose join field == the changed upstream _id
//     (FindIDsByField(view, parentFK, upstreamID)).
//   - one-to-many EmbedMany: the CHILD holds the FK, and its value IS the parent
//     _id, so read it from the doc state BEFORE and AFTER the change — a moved or
//     deleted child must recompose both the old and the new parent, and neither
//     is reachable by scanning the parent view (the FK lives on the child, under
//     the embed segment). No reverse scan, no covering index: the target is the
//     parent primary key, always indexed.
//
// Returns (ids, discoverErr): discoverErr is true when a 1:1 reverse scan errored
// (already recorded), signalling the caller to skip this view for this pass.
func (s *UpstreamSubscriber) discoverRippleTargets(
	ctx context.Context,
	v *ViewDefinition,
	embeds []embedDef,
	upstreamID string,
	before, after Document,
) ([]string, bool) {
	seen := map[string]struct{}{}
	var ordered []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	for _, e := range embeds {
		if e.Many() {
			fkCol := e.Source().SchemaDef().FKColumn()
			add(docFieldString(before, fkCol))
			add(docFieldString(after, fkCol))
			continue
		}
		ids, err := s.mongo.FindIDsByField(ctx, v.Name(), e.JoinColumn(), upstreamID)
		if err != nil {
			s.metrics.inc(s.cfg.Topic, v.Name(), upstreamRecomposeStageDiscover)
			s.logger.Error("upstream.recompose.discover",
				"subscription", s.cfg.Topic,
				"collection", s.cfg.Collection,
				"view", v.Name(),
				"upstreamID", upstreamID,
				"err", err)
			s.recordFailure(ctx, v.Name(), upstreamID, "", UpstreamFailureStageDiscover, err)
			return nil, true
		}
		for _, id := range ids {
			add(id)
		}
	}
	return ordered, false
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
	docs, err := s.mongo.FindManyByField(ctx, s.cfg.Collection, "_id", id)
	if err != nil || len(docs) == 0 {
		return nil
	}
	return docs[0]
}

// collectMongoEmbeds returns every embed (recursively, including nested) whose
// source is the given upstream Mongo collection.
func collectMongoEmbeds(embeds []embedDef, collection string) []embedDef {
	var out []embedDef
	for _, e := range embeds {
		if e.source == nil {
			continue
		}
		if e.source.IsMongo() && e.source.Collection() == collection {
			out = append(out, e)
		}
		out = append(out, collectMongoEmbeds(e.source.embeds, collection)...)
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

// recordFailure persists one ripple failure row in PG. Best-effort: any
// PG error is logged at Warn and discarded — the framework's failure
// isolation contract requires that the Kafka offset advances regardless
// of side-channel writes. Skipped entirely when s.pg is nil (test
// scaffolds that drive ripple without a Postgres handle).
func (s *UpstreamSubscriber) recordFailure(
	ctx context.Context,
	viewName, upstreamID, localID string,
	stage UpstreamFailureStage,
	cause error,
) {
	if s.eng == nil {
		return
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	rec := UpstreamFailureRecord{
		SubscriptionTopic: s.cfg.Topic,
		ViewName:          viewName,
		UpstreamID:        upstreamID,
		LocalID:           localID,
		Stage:             stage,
		Error:             msg,
	}
	if err := RecordUpstreamFailure(ctx, s.eng.Querier(), s.eng.Dialect(), rec); err != nil {
		s.logger.Warn("upstream.recompose.record_failure_failed",
			"subscription", s.cfg.Topic,
			"view", viewName,
			"upstreamID", upstreamID,
			"localID", localID,
			"stage", stage,
			"err", err)
	}
}

// resolveFailures marks any pending failure for (subscription, view,
// upstream_id) as resolved. Best-effort + nil-safe like recordFailure.
func (s *UpstreamSubscriber) resolveFailures(ctx context.Context, viewName, upstreamID string) {
	if s.eng == nil {
		return
	}
	if err := ResolveUpstreamFailures(ctx, s.eng.Querier(), s.eng.Dialect(), s.cfg.Topic, viewName, upstreamID); err != nil {
		s.logger.Warn("upstream.recompose.resolve_failures_failed",
			"subscription", s.cfg.Topic,
			"view", viewName,
			"upstreamID", upstreamID,
			"err", err)
	}
}

// joinFieldFor walks v.Embeds() looking for the embed that points at
// s.cfg.Collection via an external FromSchema. Used by ripple to compute the Mongo
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
		if e.source != nil && e.source.IsMongo() && e.source.Collection() == collection {
			return e.JoinColumn()
		}
		if jf := findMongoJoinField(e.source.embeds, collection); jf != "" {
			return jf
		}
	}
	return ""
}

// decodePayload extracts the payload map from a Debezium Outbox Event
// Router message. The event router emits the payload either as the raw
// JSON object (when StringConverter/JSONConverter is used end-to-end)
// or wrapped under {"payload": ...} (some Debezium configurations leak
// the schema envelope). The decoder tolerates both shapes.
func (s *UpstreamSubscriber) decodePayload(raw []byte) (bson.M, error) {
	if len(raw) == 0 {
		return bson.M{}, nil
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	if inner, ok := top["payload"].(map[string]any); ok {
		return bson.M(inner), nil
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
