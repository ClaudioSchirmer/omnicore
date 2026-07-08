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
package kafka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

func init() {
	transport.RegisterSubscriber("kafka", New)
}

// dialTimeout bounds the controller dial used by EnsureTopics.
const dialTimeout = 5 * time.Second

// New builds the Kafka subscriber from the neutral transport config. Endpoints is
// the bootstrap-servers list (Kafka or Redpanda); a nil/empty list is accepted
// here and surfaces as a dial error at Subscribe/EnsureTopics time, matching the
// prior behavior where the reader was constructed with whatever brokers config
// provided.
func New(cfg transport.Config) (transport.Subscriber, error) {
	return &subscriber{brokers: cfg.Endpoints}, nil
}

type subscriber struct {
	brokers []string
}

// Subscribe opens a consumer-group reader. A single-topic subscription uses the
// Topic field; a multi-topic one uses GroupTopics (the shape SyncEngine needs to
// fan several aggregate topics into one group) — both read under GroupID.
func (s *subscriber) Subscribe(_ context.Context, cfg transport.SubscribeConfig) (transport.Subscription, error) {
	rc := kafka.ReaderConfig{
		Brokers:        s.brokers,
		GroupID:        cfg.GroupID,
		CommitInterval: cfg.CommitInterval,
	}
	if len(cfg.Topics) == 1 {
		rc.Topic = cfg.Topics[0]
	} else {
		rc.GroupTopics = cfg.Topics
	}
	switch cfg.StartFrom {
	case transport.StartFromEarliest:
		rc.StartOffset = kafka.FirstOffset
	default:
		rc.StartOffset = kafka.LastOffset
	}
	return &subscription{reader: kafka.NewReader(rc)}, nil
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

type subscription struct {
	reader *kafka.Reader
}

// Read blocks for the next message and translates it into the neutral envelope.
// Headers flatten to a map, LAST occurrence winning — kafka-go preserves
// producer order, so this matches the on-the-wire intent (and the behavior of
// the previous flattenHeaders helper).
func (s *subscription) Read(ctx context.Context) (transport.Message, error) {
	msg, err := s.reader.ReadMessage(ctx)
	if err != nil {
		return transport.Message{}, err
	}
	return transport.Message{
		Topic:   msg.Topic,
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: flattenHeaders(msg.Headers),
	}, nil
}

// flattenHeaders converts kafka-go's ordered header slice into the neutral map,
// LAST occurrence winning on a duplicate key — kafka-go preserves producer
// order, so this matches the on-the-wire intent.
func flattenHeaders(headers []kafka.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		out[h.Key] = string(h.Value)
	}
	return out
}

func (s *subscription) Close() error { return s.reader.Close() }
