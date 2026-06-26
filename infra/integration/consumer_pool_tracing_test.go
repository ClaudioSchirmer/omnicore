package integration

import "testing"

// The kafka instrument toggle threads through ConsumerPool.WithKafkaTracing;
// default off, fluent, returns the receiver.
func TestConsumerPoolWithKafkaTracing(t *testing.T) {
	p := &ConsumerPool{}
	if p.traceKafka {
		t.Fatal("default must be off")
	}
	if got := p.WithKafkaTracing(true); got != p || !p.traceKafka {
		t.Fatal("setter must enable tracing and return the receiver")
	}
}
