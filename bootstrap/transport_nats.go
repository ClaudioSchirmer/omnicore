//go:build nats && !kafka

package bootstrap

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// This file compiles under the `nats` build tag. The NATS adapter is not part of
// this build yet (it lands with the JetStream work); until then a `nats`-tagged
// binary compiles but aborts at boot with a clear message, mirroring how
// engine_none.go keeps the neutral bootstrap compiling while turning a
// missing-transport build into a boot-time configuration error rather than a
// compile error.
func newTransportSubscriber(_ *Config) (transport.Subscriber, error) {
	return nil, fmt.Errorf("transport: the nats adapter is not available in this build — rebuild with -tags kafka")
}
