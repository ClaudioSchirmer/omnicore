//go:build !kafka && !nats

package bootstrap

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// This file compiles when neither transport build tag is set. No adapter
// registers, so bootstrap.Build aborts at newTransportSubscriber with an
// actionable message before any consumer loop starts — mirroring engine_none.go.
// It exists only so the neutral bootstrap code still compiles tagless; building
// with no transport is a configuration error caught at boot, not a compile error.
func newTransportSubscriber(_ *Config) (transport.Subscriber, error) {
	return nil, fmt.Errorf("transport: no transport linked — build with a transport build tag (e.g. -tags kafka)")
}
