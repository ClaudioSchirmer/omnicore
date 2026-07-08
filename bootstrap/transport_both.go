//go:build kafka && nats

package bootstrap

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// This file compiles when BOTH transport tags are set. Unlike the relational
// engine seam — where a both-engines build selects the active dialect at runtime
// from relational.dialect — the transport is chosen per deployment at build
// time, so a two-transport binary has no runtime selector to disambiguate. Until
// a runtime transport selector exists, linking both tags is a build mistake,
// surfaced at boot rather than silently favoring one.
func newTransportSubscriber(_ *Config) (transport.Subscriber, error) {
	return nil, fmt.Errorf("transport: building with both `kafka` and `nats` tags is not supported — link exactly one transport")
}
