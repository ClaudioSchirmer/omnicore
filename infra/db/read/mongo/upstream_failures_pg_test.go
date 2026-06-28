package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// These tests drive the UpstreamSubscriber failure-registry helpers, which reach
// the relational backend through the engine's neutral Querier/Dialect seam, so a
// fakeEngine with a scriptable Querier stands in for a live database.

func newUpstreamWithEngine(eng db.RelationalEngine) *UpstreamSubscriber {
	s, err := NewUpstreamSubscriber(
		eng, newFakeMongo(&fakeColl{}),
		NewComposerWithMongo(eng, newFakeMongo(&fakeColl{})),
		UpstreamSubscriberConfig{Topic: "users.events", Collection: "users"},
		nil, nil, nil,
	)
	if err != nil {
		panic(err)
	}
	return s
}

func TestUpstreamSubscriber_RetryPendingFailures_NoPG(t *testing.T) {
	s := &UpstreamSubscriber{}
	if _, err := s.RetryPendingFailures(context.Background()); err == nil {
		t.Fatal("expected error when subscriber has no relational engine handle")
	}
}

func TestUpstreamSubscriber_RetryPendingFailures_Empty(t *testing.T) {
	eng := newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (db.Rows, error) { return &fakeRows{}, nil },
	})
	s := newUpstreamWithEngine(eng)

	n, err := s.RetryPendingFailures(context.Background())
	if err != nil {
		t.Fatalf("RetryPendingFailures: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 (no pending)", n)
	}
}

func TestUpstreamSubscriber_RetryPendingFailures_ListError(t *testing.T) {
	eng := newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (db.Rows, error) { return nil, errFake },
	})
	s := newUpstreamWithEngine(eng)

	if _, err := s.RetryPendingFailures(context.Background()); err == nil {
		t.Fatal("expected error when listing pending fails")
	}
}

func TestUpstreamSubscriber_RecordResolveFailures(t *testing.T) {
	eng := newFakeEngine(&fakeQuerier{})
	s := newUpstreamWithEngine(eng)
	ctx := context.Background()

	// recordFailure and resolveFailures are best-effort; with the fake engine
	// they execute the registry SQL without error.
	s.recordFailure(ctx, "orders", "u1", "ord-7", UpstreamFailureStageCompose, errFake)
	s.resolveFailures(ctx, "orders", "u1")
}
