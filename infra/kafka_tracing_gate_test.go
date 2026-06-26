package infra

import "testing"

// The kafka instrument toggle threads through each engine's WithKafkaTracing
// setter; default off, fluent, returns the receiver.
func TestSyncEngineWithKafkaTracing(t *testing.T) {
	s := &SyncEngine{}
	if s.traceKafka {
		t.Fatal("default must be off")
	}
	if got := s.WithKafkaTracing(true); got != s || !s.traceKafka {
		t.Fatal("setter must enable tracing and return the receiver")
	}
	if s.WithKafkaTracing(false); s.traceKafka {
		t.Fatal("setter must disable tracing")
	}
}

func TestUpstreamSubscriberWithKafkaTracing(t *testing.T) {
	s := &UpstreamSubscriber{}
	if s.traceKafka {
		t.Fatal("default must be off")
	}
	if got := s.WithKafkaTracing(true); got != s || !s.traceKafka {
		t.Fatal("setter must enable tracing and return the receiver")
	}
}
