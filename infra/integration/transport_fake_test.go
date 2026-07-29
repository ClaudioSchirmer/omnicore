package integration

import (
	"context"
	"errors"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// fakeSubscriber is the unit-test stand-in for the message-transport port,
// modelling an unreachable broker deterministically (formerly an unroutable
// broker address). Subscribe succeeds so the consumer group's goroutine spins
// up; Read errors on a short timer so the loop periodically re-checks its stop
// signal and drains promptly on Shutdown. The ConsumerPool never calls
// EnsureTopics (only SyncEngine does), so it is a nil-safe no-op here.
type fakeSubscriber struct{}

func (fakeSubscriber) EnsureTopics(context.Context, []transport.TopicSpec) error { return nil }

func (fakeSubscriber) Subscribe(context.Context, transport.SubscribeConfig) (transport.Subscription, error) {
	return fakeSubscription{}, nil
}

type fakeSubscription struct{}

func (fakeSubscription) Read(ctx context.Context) (transport.Message, transport.Completion, error) {
	select {
	case <-ctx.Done():
		return transport.Message{}, nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return transport.Message{}, nil, errors.New("fake transport: broker unreachable")
	}
}

func (fakeSubscription) Close() error { return nil }
