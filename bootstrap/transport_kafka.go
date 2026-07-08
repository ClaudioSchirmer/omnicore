//go:build kafka && !nats

package bootstrap

import (
	"github.com/ClaudioSchirmer/omnicore/infra/transport"
	_ "github.com/ClaudioSchirmer/omnicore/infra/transport/kafka"
)

// This file is the Kafka/Redpanda transport binding, compiled only under the
// `kafka` build tag. The blank import runs the adapter package's init(), which
// registers the "kafka" transport in the subscriber registry so
// transport.NewSubscriber resolves it — behind the build tag so a default build
// links neither the adapter nor segmentio/kafka-go. Mirrors bootstrap's
// tag-gated relational-engine bindings (engine_postgres.go / engine_mysql.go).

// newTransportSubscriber builds the Kafka/Redpanda subscriber from the broker
// list. Redpanda is reached by pointing kafka.brokers at it — same adapter, same
// wire protocol; the choice is deployment config, not a code path.
func newTransportSubscriber(cfg *Config) (transport.Subscriber, error) {
	return transport.NewSubscriber("kafka", transport.Config{Endpoints: cfg.Transport.Endpoints})
}
