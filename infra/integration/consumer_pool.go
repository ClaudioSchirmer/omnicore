package integration

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

// defaultIntegrationCommitInterval mirrors SyncEngine and the upstream
// subscriber — async commits batched once a second. Safe under
// at-least-once because the framework's per-message pipeline records
// dedup post-success; a crash between handler COMMIT and dedup INSERT
// produces a double-invoke that idempotent handlers absorb (the
// documented contract).
const defaultIntegrationCommitInterval = time.Second

// ConsumerPool owns the Kafka consumer goroutines that drive every
// Receiver registered on the Registry. Bootstrap calls Start(ctx)
// once after Phase Receivers completes; Shutdown(drainCtx) is invoked
// by the coordinated shutdown path on SIGINT/SIGTERM.
type ConsumerPool struct {
	registry *Registry
	cfg      *Config
	// eng is the relational engine the dedup + failure registries are
	// read/written through (neutral Querier + Dialect).
	eng     db.RelationalEngine
	brokers []string
	pipe    *pipeline.Pipeline
	logger  *slog.Logger

	// drains coordinates per-receiver supervisor shutdown. inflight
	// counts per-message processing so Shutdown can wait for every
	// in-flight handler to finish before infra deps close.
	stop     chan struct{}
	stopOnce sync.Once
	inflight sync.WaitGroup
	workers  sync.WaitGroup
	// traceKafka gates the per-message consumer span (the tracing `kafka`
	// instrument toggle). bootstrap sets it via WithKafkaTracing; false (the
	// default) leaves the receiver loop untraced and pays nothing.
	traceKafka bool
}

// WithKafkaTracing enables the consumer span on each received message. bootstrap
// passes tracing.Instruments(SubKafka); off (the default) keeps the loop untraced.
func (p *ConsumerPool) WithKafkaTracing(on bool) *ConsumerPool {
	p.traceKafka = on
	return p
}

// NewConsumerPool wires the pool. brokers + pipe are framework
// singletons already present on bootstrap.Deps; eng is the relational
// engine, used here to read/write the dedup + failure tables through the
// neutral Querier + Dialect.
func NewConsumerPool(
	registry *Registry,
	cfg *Config,
	eng db.RelationalEngine,
	brokers []string,
	pipe *pipeline.Pipeline,
	logger *slog.Logger,
) *ConsumerPool {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConsumerPool{
		registry: registry,
		cfg:      cfg,
		eng:      eng,
		brokers:  brokers,
		pipe:     pipe,
		logger:   logger,
		stop:     make(chan struct{}),
	}
}

// Start resolves every receiver against YAML, groups them by (topic,
// consumerGroup), and starts one supervisor goroutine per GROUP — one
// Kafka reader that demultiplexes by event_type to the matching receiver.
//
// Why per group and not per receiver: Kafka assigns each partition of a
// topic to exactly ONE member of a consumer group. Two readers sharing
// the same (topic, consumerGroup) would split the topic's partitions
// between them, and — because the reader auto-commits — each would
// silently drop the events meant for the other (it reads them, finds the
// event_type does not match its receiver, and the offset advances anyway).
// A source declaring two events (From(s).On(A).On(B)) resolves both to the
// same topic + consumerGroup, so the per-receiver topology would lose
// roughly half of every event type. One reader per (topic, group) that
// routes by event_type is the canonical "one topic, many event types,
// many handlers" shape and reads every message exactly once.
//
// Returns an error when YAML resolution fails on any receiver, or when two
// receivers map to the same (topic, consumerGroup, event_type) — both boot
// abort surfaces.
func (p *ConsumerPool) Start(ctx context.Context) error {
	if p == nil || p.registry == nil || p.registry.IsEmpty() {
		return nil
	}
	receivers := p.registry.Receivers()
	for _, r := range receivers {
		if err := r.resolveAgainstYAML(p.cfg); err != nil {
			return fmt.Errorf("integration: consumer pool start: %w", err)
		}
		if r.workers <= 0 {
			r.workers = runtime.NumCPU()
		}
	}
	groups, err := groupReceivers(receivers)
	if err != nil {
		return fmt.Errorf("integration: consumer pool start: %w", err)
	}
	for _, g := range groups {
		p.workers.Add(1)
		go p.runConsumerGroup(ctx, g)
	}
	p.logger.Info("integration consumer pool started",
		"receivers", len(receivers),
		"groups", len(groups),
		"brokers", strings.Join(p.brokers, ","))
	return nil
}

// receiverGroup is one Kafka consumer: a single reader bound to a (topic,
// consumerGroup) coordinate that demultiplexes each incoming message to
// the receiver whose wireEventType matches the message's event_type
// header. workers/startFrom derive from the source and are identical
// across a group's receivers (both topic and consumerGroup come from the
// source); workers takes the max defensively in the rare case two sources
// share the same coordinate.
type receiverGroup struct {
	topic         string
	consumerGroup string
	workers       int
	startFrom     string
	byEventType   map[string]*Receiver
}

// groupReceivers folds the flat receiver slice into one group per (topic,
// consumerGroup). Two receivers under the same coordinate with the SAME
// wireEventType is a misconfiguration (one event type cannot route to two
// handlers) and aborts the boot with a diagnostic naming both.
func groupReceivers(receivers []*Receiver) (map[string]*receiverGroup, error) {
	groups := make(map[string]*receiverGroup)
	for _, r := range receivers {
		key := r.topic + "\x00" + r.consumerGroup
		g, ok := groups[key]
		if !ok {
			g = &receiverGroup{
				topic:         r.topic,
				consumerGroup: r.consumerGroup,
				workers:       r.workers,
				startFrom:     r.startFrom,
				byEventType:   make(map[string]*Receiver),
			}
			groups[key] = g
		}
		if existing, dup := g.byEventType[r.wireEventType]; dup {
			return nil, fmt.Errorf(
				"two receivers map to topic=%q consumerGroup=%q event_type=%q "+
					"(sourceKey=%q eventKey=%q and sourceKey=%q eventKey=%q) — "+
					"one event type cannot route to two handlers",
				r.topic, r.consumerGroup, r.wireEventType,
				existing.sourceKey, existing.eventKey, r.sourceKey, r.eventKey)
		}
		g.byEventType[r.wireEventType] = r
		if r.workers > g.workers {
			g.workers = r.workers
		}
	}
	return groups, nil
}

// runConsumerGroup drives ONE Kafka reader for a (topic, consumerGroup)
// coordinate and demultiplexes each message to the receiver whose
// wireEventType matches the event_type header. Each message is delivered
// to a fixed worker pool keyed by aggregate_id so per-aggregate ordering
// is preserved across event types. The supervisor exits on the stop
// signal OR ctx.Done.
func (p *ConsumerPool) runConsumerGroup(ctx context.Context, g *receiverGroup) {
	defer p.workers.Done()

	readerCfg := kafka.ReaderConfig{
		Brokers:        p.brokers,
		Topic:          g.topic,
		GroupID:        g.consumerGroup,
		CommitInterval: defaultIntegrationCommitInterval,
	}
	switch g.startFrom {
	case "earliest":
		readerCfg.StartOffset = kafka.FirstOffset
	default:
		readerCfg.StartOffset = kafka.LastOffset
	}

	reader := kafka.NewReader(readerCfg)
	defer func() { _ = reader.Close() }()

	workers := g.workers
	if workers < 1 {
		workers = 1
	}
	queues := make([]chan kafka.Message, workers)
	var workerWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		queues[i] = make(chan kafka.Message, 4)
		workerWG.Add(1)
		go func(q <-chan kafka.Message) {
			defer workerWG.Done()
			for msg := range q {
				p.inflight.Add(1)
				p.processGroupMessage(ctx, g, msg)
				p.inflight.Done()
			}
		}(queues[i])
	}
	defer func() {
		for _, q := range queues {
			close(q)
		}
		workerWG.Wait()
	}()

	p.logger.Info("integration consumer group started",
		"topic", g.topic,
		"consumer_group", g.consumerGroup,
		"event_types", len(g.byEventType),
		"workers", workers)

	for {
		select {
		case <-p.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-p.stop:
				return
			default:
			}
			p.logger.Warn("integration consumer read error",
				"topic", g.topic, "err", err)
			continue
		}
		// Route to a worker by aggregate_id bucket for ordering. Empty
		// aggregate_id falls back to bucket 0 — order is undefined but
		// dedup still works.
		bucket := bucketOfMessage(msg, workers)
		select {
		case queues[bucket] <- msg:
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		}
	}
}

// processGroupMessage demultiplexes one message by its event_type header
// to the receiver registered for it on this (topic, consumerGroup) group,
// then defers to Receiver.handleMessage for the rest of the pipeline.
//
// A message whose event_type matches no receiver is a foreign event on the
// topic the service does not subscribe to — it is skipped, and the offset
// auto-commits so it is not re-read. Back-compat: a single-receiver group
// whose message carries an ABSENT event_type header delivers to its only
// receiver (mirrors the prior lenient filter that processed when the header
// was missing); a present-but-unmatched header is always a skip.
func (p *ConsumerPool) processGroupMessage(ctx context.Context, g *receiverGroup, msg kafka.Message) {
	headers := flattenHeaders(msg.Headers)
	r := g.route(headers["event_type"])
	if r == nil {
		return
	}
	ctx, span := tracing.StartConsumerSpanIf(p.traceKafka, ctx,
		"github.com/ClaudioSchirmer/omnicore/infra/integration",
		"receive "+headers["event_type"], headers["traceparent"])
	defer span.End()
	eventID := parseEventID(headers["event_id"], msg.Key)
	_ = r.handleMessage(ctx, p.eng, headers, eventID, msg.Value, p.pipe, p.logger)
}

// route selects the receiver an event_type maps to within this group, or
// nil to skip. A present event_type matches by exact equality. An absent
// event_type ("") routes to the only receiver when the group has exactly
// one (back-compat with the prior lenient filter); with more than one
// receiver an absent or unmatched event_type is unroutable.
func (g *receiverGroup) route(eventType string) *Receiver {
	if r := g.byEventType[eventType]; r != nil {
		return r
	}
	if eventType == "" && len(g.byEventType) == 1 {
		for _, only := range g.byEventType {
			return only
		}
	}
	return nil
}

// parseEventID prefers an explicit event_id header, falls back to
// parsing msg.Key as UUID. Returns uuid.Nil when neither yields a
// valid UUID — handleMessage logs and skips.
func parseEventID(header string, key []byte) uuid.UUID {
	if header != "" {
		if u, err := uuid.Parse(header); err == nil {
			return u
		}
	}
	if len(key) == 16 {
		var u uuid.UUID
		copy(u[:], key)
		return u
	}
	if len(key) > 0 {
		if u, err := uuid.Parse(string(key)); err == nil {
			return u
		}
	}
	return uuid.Nil
}

// flattenHeaders converts kafka-go's slice shape into a map for
// simpler downstream lookup. Duplicate keys keep the LAST occurrence —
// kafka-go preserves Producer order so this matches the wire intent.
func flattenHeaders(headers []kafka.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		out[h.Key] = string(h.Value)
	}
	return out
}

// bucketOfMessage groups messages by aggregate_id (when present) so
// the framework preserves per-aggregate ordering across workers.
// Mirrors how SyncEngine + UpstreamSubscriber drive their own worker
// fan-out.
func bucketOfMessage(msg kafka.Message, workers int) int {
	if workers <= 1 {
		return 0
	}
	var sum int
	if len(msg.Key) > 0 {
		for _, b := range msg.Key {
			sum = sum*31 + int(b)
		}
	}
	if sum < 0 {
		sum = -sum
	}
	return sum % workers
}

// Shutdown stops every receiver supervisor and waits for every
// in-flight message to complete (or drainCtx to expire). Mirrors
// the shape UpstreamSubscriber.Shutdown follows so bootstrap's
// coordinated drain can call both with the same shared drainCtx.
// Idempotent across calls.
func (p *ConsumerPool) Shutdown(drainCtx context.Context) error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() { close(p.stop) })

	// Wait for every supervisor to exit before waiting on inflight —
	// supervisors close their worker channels in their own defer, so
	// the order matters.
	done := make(chan struct{})
	go func() {
		p.workers.Wait()
		p.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-drainCtx.Done():
		return drainCtx.Err()
	}
}
