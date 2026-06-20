package infra

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// These tests drive the UpstreamSubscriber failure-registry helpers, which
// reach Postgres through the internal querier() seam, so the fakePool exec/
// query handlers can stand in for a live database.

func newUpstreamWithPool(pool *fakePool) *UpstreamSubscriber {
	s, err := NewUpstreamSubscriber(
		newFakePostgres(pool), newFakeMongo(&fakeColl{}),
		NewComposerWithMongo(newFakePostgres(pool), newFakeMongo(&fakeColl{})),
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
		t.Fatal("expected error when subscriber has no Postgres handle")
	}
}

func TestUpstreamSubscriber_RetryPendingFailures_Empty(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{}, nil }
	s := newUpstreamWithPool(pool)

	n, err := s.RetryPendingFailures(context.Background())
	if err != nil {
		t.Fatalf("RetryPendingFailures: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 (no pending)", n)
	}
}

func TestUpstreamSubscriber_RetryPendingFailures_ListError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return nil, errFake }
	s := newUpstreamWithPool(pool)

	if _, err := s.RetryPendingFailures(context.Background()); err == nil {
		t.Fatal("expected error when listing pending fails")
	}
}

func TestUpstreamSubscriber_RecordResolveFailures(t *testing.T) {
	pool := newFakePool()
	s := newUpstreamWithPool(pool)
	ctx := context.Background()

	// recordFailure and resolveFailures are best-effort; with the fake pool
	// they execute the registry SQL without error.
	s.recordFailure(ctx, "orders", "discover", "u1", "", errFake)
	s.resolveFailures(ctx, "orders", "u1")
}
