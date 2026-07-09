//go:build nats && !kafka

package bootstrap

import (
	"github.com/ClaudioSchirmer/omnicore/infra/transport"
	_ "github.com/ClaudioSchirmer/omnicore/infra/transport/nats"
)

// This file is the NATS transport binding, compiled only under the `nats` build
// tag. The blank import runs the adapter package's init(), which registers the
// "nats" transport in the subscriber registry so transport.NewSubscriber
// resolves it — behind the build tag so a default build links neither the
// adapter nor nats.go. Mirrors transport_kafka.go.
func newTransportSubscriber(cfg *Config) (transport.Subscriber, error) {
	return transport.NewSubscriber("nats", transport.Config{Endpoints: cfg.Transport.Endpoints})
}
