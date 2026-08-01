//go:build !kafka && !nats

package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// TestNoopSubscriber_D6 proves the tagless transport posture (D6): the build
// links a no-op subscriber (not a hard boot failure), and any actual attempt to
// consume fails with an actionable "build with a transport tag" error rather
// than a nil panic or silence.
func TestNoopSubscriber_D6(t *testing.T) {
	sub, err := newTransportSubscriber(&Config{})
	if err != nil {
		t.Fatalf("tagless build must yield a no-op subscriber, not a boot error: %v", err)
	}
	if sub == nil {
		t.Fatal("no-op subscriber must be non-nil")
	}

	_, subErr := sub.Subscribe(context.Background(), transport.SubscribeConfig{})
	if subErr == nil || !strings.Contains(subErr.Error(), "no transport linked") {
		t.Errorf("Subscribe on the no-op transport must return an actionable error, got %v", subErr)
	}
	if topErr := sub.EnsureTopics(context.Background(), nil); topErr == nil || !strings.Contains(topErr.Error(), "no transport linked") {
		t.Errorf("EnsureTopics on the no-op transport must return an actionable error, got %v", topErr)
	}
}
