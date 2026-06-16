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
	"github.com/ClaudioSchirmer/omnicore/infra"

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
	pg       *infra.Postgres
	brokers  []string
	pipe     *pipeline.Pipeline
	logger   *slog.Logger

	// drains coordinates per-receiver supervisor shutdown. inflight
	// counts per-message processing so Shutdown can wait for every
	// in-flight handler to finish before infra deps close.
	stop     chan struct{}
	stopOnce sync.Once
	inflight sync.WaitGroup
	workers  sync.WaitGroup
}

// NewConsumerPool wires the pool. brokers + pipe are framework
// singletons already present on bootstrap.Deps; pg is the same handle
// the producer side uses, repurposed here to read/write the dedup +
// failure tables.
func NewConsumerPool(
	registry *Registry,
	cfg *Config,
	pg *infra.Postgres,
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
		pg:       pg,
		brokers:  brokers,
		pipe:     pipe,
		logger:   logger,
		stop:     make(chan struct{}),
	}
}

// Start resolves every receiver against YAML, starts one supervisor
// goroutine per receiver, and returns immediately. The supervisor
// drives a Kafka reader per receiver — one consumer group per (source,
// receiver) pair is the simplest topology; multi-receiver-same-topic
// patterns can layer on top later. Returns an error when YAML
// resolution fails on any receiver (boot abort surface).
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
	for _, r := range receivers {
		p.workers.Add(1)
		go p.runReceiver(ctx, r)
	}
	p.logger.Info("integration consumer pool started",
		"receivers", len(receivers),
		"brokers", strings.Join(p.brokers, ","))
	return nil
}

// runReceiver drives one Kafka reader for one receiver. Each message
// is delivered to a fixed worker pool (one channel per worker) so
// per-aggregate ordering is preserved when receivers care about it.
// The supervisor exits on the stop signal OR ctx.Done.
func (p *ConsumerPool) runReceiver(ctx context.Context, r *Receiver) {
	defer p.workers.Done()

	readerCfg := kafka.ReaderConfig{
		Brokers:        p.brokers,
		Topic:          r.topic,
		GroupID:        r.consumerGroup,
		CommitInterval: defaultIntegrationCommitInterval,
	}
	switch r.startFrom {
	case "earliest":
		readerCfg.StartOffset = kafka.FirstOffset
	default:
		readerCfg.StartOffset = kafka.LastOffset
	}

	reader := kafka.NewReader(readerCfg)
	defer func() { _ = reader.Close() }()

	workers := r.workers
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
				p.processOne(ctx, r, msg)
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

	p.logger.Info("integration receiver started",
		"source_key", r.sourceKey,
		"event_key", r.eventKey,
		"topic", r.topic,
		"consumer_group", r.consumerGroup,
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
				"topic", r.topic, "err", err)
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

// processOne extracts the event metadata + payload, then defers to
// Receiver.handleMessage for the rest of the pipeline.
func (p *ConsumerPool) processOne(ctx context.Context, r *Receiver, msg kafka.Message) {
	headers := flattenHeaders(msg.Headers)
	// Only route messages whose wire event_type matches THIS receiver.
	// One topic may carry many event types; the framework's per-event
	// receivers each filter at this layer.
	if r.wireEventType != "" && headers["event_type"] != "" && headers["event_type"] != r.wireEventType {
		return
	}
	eventID := parseEventID(headers["event_id"], msg.Key)
	_ = r.handleMessage(ctx, p.pg.Pool(), headers, eventID, msg.Value, p.pipe, p.logger)
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
// in-flight processOne to complete (or drainCtx to expire). Mirrors
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
