//go:build !kafka && !nats

package bootstrap

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// This file compiles when neither transport build tag is set. No adapter
// registers, so newTransportSubscriber returns a NO-OP subscriber that dials
// nothing — building tagless is valid (the infra-free / zero-broker MVP posture)
// instead of a hard boot failure. The subscriber is inert: nothing subscribes in
// the infra-free posture (no Mongo-backed views, no integration consumers, no
// upstream subscriptions), so it is never dispatched to. If a consumer DOES try
// to use it — a build that forgot the transport tag but declared messaging — the
// actionable error surfaces at the point of use (Subscribe / EnsureTopics), not
// as a compile error and not as a nil panic. The write/outbox path is unaffected
// (there is no in-process producer; the outbox is drained by an external relay).
func newTransportSubscriber(_ *Config) (transport.Subscriber, error) {
	return noopSubscriber{}, nil
}

// noopSubscriber is the inert transport for a tagless build. Both methods fail
// with the same actionable message, so any actual attempt to consume is a clear
// "build with a transport tag" error rather than silence.
type noopSubscriber struct{}

const noTransportMsg = "transport: no transport linked — this build has no messaging (build with -tags kafka or -tags nats to consume integration events, upstream subscriptions, or Mongo-projected views)"

func (noopSubscriber) Subscribe(context.Context, transport.SubscribeConfig) (transport.Subscription, error) {
	return nil, fmt.Errorf(noTransportMsg)
}

func (noopSubscriber) EnsureTopics(context.Context, []transport.TopicSpec) error {
	return fmt.Errorf(noTransportMsg)
}
