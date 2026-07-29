package query

import (
	"context"
	"errors"
	"sync"
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

func (fakeSubscription) Read(ctx context.Context) (transport.Message, transport.Completion, error) {
	select {
	case <-ctx.Done():
		return transport.Message{}, nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return transport.Message{}, nil, errors.New("fake transport: broker unreachable")
	}
}

func (fakeSubscription) Close() error { return nil }

// recordingCompletion is the unit-test Completion: it records which outcome was
// reported so a test can assert the delivery contract itself — that a processed
// event is confirmed, a shutdown-interrupted one is handed back, and neither is
// reported twice. Mutex-guarded because the consume tests poll the counters
// from the test goroutine while a WORKER goroutine reports the outcome; the
// single-threaded tests may keep reading the fields directly.
type recordingCompletion struct {
	mu     sync.Mutex
	done   int
	failed int
}

func (c *recordingCompletion) Done(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done++
	return nil
}

func (c *recordingCompletion) Failed(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed++
	return nil
}

// counts reads both outcomes under the lock — the concurrent-test accessor.
func (c *recordingCompletion) counts() (done, failed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done, c.failed
}
