package query

import (
	"context"
	"errors"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// fakeSubscriber is the unit-test stand-in for the message-transport port. It
// models an unreachable broker deterministically — the same role the old tests
// gave an unroutable broker address, now expressed behind the seam:
//
//   - EnsureTopics always errors, so SyncEngine.run stays alive in its
//     ensureTopics retry loop until its context is cancelled (the shutdown
//     tests' "the loop is still working" stand-in).
//   - Subscription.Read errors on a short timer, so a consumer loop periodically
//     re-checks its stop signal and exits promptly on Shutdown — exactly as the
//     real reader did when the unroutable dial kept failing, but without a
//     tight spin.
type fakeSubscriber struct{}

func (fakeSubscriber) EnsureTopics(context.Context, []transport.TopicSpec) error {
	return errors.New("fake transport: broker unreachable")
}

func (fakeSubscriber) Subscribe(context.Context, transport.SubscribeConfig) (transport.Subscription, error) {
	return fakeSubscription{}, nil
}

type fakeSubscription struct{}

func (fakeSubscription) Read(ctx context.Context) (transport.Message, error) {
	select {
	case <-ctx.Done():
		return transport.Message{}, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return transport.Message{}, errors.New("fake transport: broker unreachable")
	}
}

func (fakeSubscription) Close() error { return nil }
