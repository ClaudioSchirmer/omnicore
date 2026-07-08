// Package transport is the message-transport seam: the backend-neutral port the
// framework's async consumers (the read-side SyncEngine, the integration
// ConsumerPool, and the UpstreamSubscriber) read through, so a broker drops in
// without those loops naming a concrete client. A transport is selected once, at
// build time, through the subscriber registry (RegisterSubscriber / NewSubscriber);
// the concrete adapters live in sibling packages (infra/transport/kafka, later
// infra/transport/nats), each compiled behind its own build tag, and implement
// the Subscriber port.
//
// This mirrors the relational-engine seam (infra/db/core: RegisterEngine /
// NewEngine): an adapter self-registers in init() behind its build tag, and the
// composition root looks it up by name — so bootstrap never imports a broker
// client directly and a build links exactly the one adapter its tag selects.
package transport

import (
	"context"
	"fmt"
	"time"
)

// Message is the transport-neutral envelope the framework's consumers process.
// Each adapter translates its native message into this shape:
//
//   - Key is the partition/ordering key (the aggregate_id for outbox events).
//   - Value is the raw payload bytes (the outbox JSON).
//   - Headers is the flattened header map; on a duplicate header key the LAST
//     occurrence wins, matching the producer's on-the-wire intent.
//   - Topic is the source topic/subject, carried for diagnostics.
type Message struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers map[string]string
}

// StartFrom values for a fresh consumer group with no committed offset. An empty
// SubscribeConfig.StartFrom is treated as StartFromLatest.
const (
	StartFromEarliest = "earliest"
	StartFromLatest   = "latest"
)

// SubscribeConfig describes one consumer subscription. A single-topic
// subscription (len(Topics) == 1) and a multi-topic group subscription are both
// expressed here; the adapter picks the native shape.
type SubscribeConfig struct {
	Topics         []string      // one or many topics/subjects; all read under GroupID
	GroupID        string        // consumer group / durable-consumer name
	StartFrom      string        // StartFromEarliest | StartFromLatest (empty = latest)
	CommitInterval time.Duration // async offset/ack batching window (0 lets the adapter default)
}

// TopicSpec is a topic the framework wants to exist before subscribing.
// EnsureTopics is best-effort and idempotent: an already-present topic is a
// no-op. NumPartitions/ReplicationFactor apply only when the adapter has to
// create the topic (single-broker dev); production pre-provisioning ignores them.
type TopicSpec struct {
	Name              string
	NumPartitions     int
	ReplicationFactor int
}

// Subscription is one live consumer stream. Read blocks for the next message or
// until ctx is cancelled; Close leaves the group (or the adapter's equivalent)
// and releases resources. Close ordering matters for graceful shutdown: the
// consumer loops drain their workers, THEN Close the subscription, so the
// group-leave goes out only after in-flight work has settled.
type Subscription interface {
	Read(ctx context.Context) (Message, error)
	Close() error
}

// Subscriber is the transport port. One is built at boot from the linked adapter
// and shared by every consumer loop. Subscribe opens a stream; EnsureTopics
// best-effort provisions topics the framework intends to consume (single-broker
// dev convenience — see TopicSpec).
type Subscriber interface {
	Subscribe(ctx context.Context, cfg SubscribeConfig) (Subscription, error)
	EnsureTopics(ctx context.Context, topics []TopicSpec) error
}

// Config carries everything a SubscriberFactory needs to reach the broker. It is
// the options-struct generalization of the connection settings; a new adapter's
// knob is added as a field, not another positional argument. Brokers feeds the
// Kafka/Redpanda adapter; the NATS adapter reads its own fields.
type Config struct {
	Brokers []string
}

// SubscriberFactory builds a Subscriber for one transport. Registered by each
// adapter package in init(), behind its own build tag (kafka under -tags kafka,
// nats under -tags nats; a build links exactly one adapter).
type SubscriberFactory func(cfg Config) (Subscriber, error)

// subscriberFactories is the transport → factory registry. Mirrors the
// relational engine registry (and the database/sql driver pattern): an adapter
// self-registers in init(), the composition root looks it up by name. Keeping
// the swap here — not a hardcoded switch in bootstrap — is what lets an adapter
// live behind a build tag without bootstrap importing its client.
var subscriberFactories = map[string]SubscriberFactory{}

// RegisterSubscriber records a factory under a transport name. Called from an
// adapter package's init(). A duplicate registration panics — two adapters
// claiming the same name is a build-time bug.
func RegisterSubscriber(name string, f SubscriberFactory) {
	if _, dup := subscriberFactories[name]; dup {
		panic(fmt.Sprintf("transport.RegisterSubscriber: transport %q already registered", name))
	}
	subscriberFactories[name] = f
}

// NewSubscriber builds the Subscriber for the requested transport. An unknown
// name is a clear, actionable error — typically no transport build tag was set
// (e.g. `go build -tags kafka` or `go build -tags nats`).
func NewSubscriber(name string, cfg Config) (Subscriber, error) {
	f, ok := subscriberFactories[name]
	if !ok {
		return nil, fmt.Errorf("transport: no transport registered for %q (build with the transport's build tag?)", name)
	}
	return f(cfg)
}
