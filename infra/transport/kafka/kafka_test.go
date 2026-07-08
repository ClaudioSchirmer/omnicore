//go:build kafka

package kafka

import (
	"context"
	"testing"

	"github.com/segmentio/kafka-go"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

func TestInitRegistersKafka(t *testing.T) {
	// init() registered "kafka" in the shared registry, so the neutral
	// constructor resolves it without this package being imported by name.
	if _, err := transport.NewSubscriber("kafka", transport.Config{Brokers: []string{"b1:9092"}}); err != nil {
		t.Fatalf("transport.NewSubscriber(\"kafka\"): %v", err)
	}
}

func TestNew(t *testing.T) {
	s, err := New(transport.Config{Brokers: []string{"b1:9092"}})
	if err != nil || s == nil {
		t.Fatalf("New = (%v, %v), want non-nil, nil", s, err)
	}
}

func TestEnsureTopics_EmptyIsNoop(t *testing.T) {
	s, _ := New(transport.Config{Brokers: nil})
	if err := s.EnsureTopics(context.Background(), nil); err != nil {
		t.Fatalf("empty topic list must be a no-op, got %v", err)
	}
}

func TestEnsureTopics_NoBrokersErrors(t *testing.T) {
	s, _ := New(transport.Config{Brokers: nil})
	err := s.EnsureTopics(context.Background(), []transport.TopicSpec{{Name: "t", NumPartitions: 1, ReplicationFactor: 1}})
	if err == nil {
		t.Fatal("EnsureTopics with no brokers must error")
	}
}

func TestEnsureTopics_DialFailure(t *testing.T) {
	s, _ := New(transport.Config{Brokers: []string{"127.0.0.1:1"}})
	// A cancelled context makes the controller dial fail immediately and
	// deterministically, exercising the dial-error path without a broker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.EnsureTopics(ctx, []transport.TopicSpec{{Name: "t", NumPartitions: 1, ReplicationFactor: 1}}); err == nil {
		t.Fatal("EnsureTopics must error when the controller dial fails")
	}
}

func TestSubscribe_BuildsSubscription(t *testing.T) {
	s, _ := New(transport.Config{Brokers: []string{"127.0.0.1:9092"}})
	t.Run("single-topic-latest", func(t *testing.T) {
		sub, err := s.Subscribe(context.Background(), transport.SubscribeConfig{
			Topics:  []string{"users.events"},
			GroupID: "g1",
		})
		if err != nil || sub == nil {
			t.Fatalf("Subscribe = (%v, %v), want non-nil, nil", sub, err)
		}
		if err := sub.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	t.Run("multi-topic-earliest", func(t *testing.T) {
		sub, err := s.Subscribe(context.Background(), transport.SubscribeConfig{
			Topics:    []string{"a.events", "b.events"},
			GroupID:   "g2",
			StartFrom: transport.StartFromEarliest,
		})
		if err != nil || sub == nil {
			t.Fatalf("Subscribe (multi) = (%v, %v), want non-nil, nil", sub, err)
		}
		_ = sub.Close()
	})
}

func TestFlattenHeaders(t *testing.T) {
	if out := flattenHeaders(nil); len(out) != 0 {
		t.Fatalf("nil headers must flatten to empty map, got %v", out)
	}
	out := flattenHeaders([]kafka.Header{
		{Key: "event_type", Value: []byte("UserCreated")},
		{Key: "event_id", Value: []byte("123")},
		{Key: "event_type", Value: []byte("UserUpdated")}, // duplicate → last wins
	})
	if out["event_type"] != "UserUpdated" {
		t.Errorf("duplicate key must keep last occurrence, got %q", out["event_type"])
	}
	if out["event_id"] != "123" {
		t.Errorf("event_id = %q, want 123", out["event_id"])
	}
}
