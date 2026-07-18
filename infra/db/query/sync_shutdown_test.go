package query

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// The graceful-shutdown contract, locked as tests: Shutdown is coordinated by
// DEPENDENCY, never by timing — it blocks while the loop goroutine is alive
// and unblocks only after the goroutine fully returned (whose own deferred
// chain drains the workers and closes the reader — the Kafka LeaveGroup —
// before `done` closes; see Start).

func syncEngineForShutdownTest() *SyncEngine {
	// The fake transport's EnsureTopics always errors, so run() stays alive in
	// the ensureTopics retry loop until its ctx is cancelled — a deterministic
	// stand-in for "the loop is still working" (formerly an unroutable broker).
	view := View("gadgets").Version(1).Root("gadgets").Schema(rootSchema("gadgets"))
	return NewSyncEngine(nil, nil, identityResolver, fakeSubscriber{}, "shutdown-test-group",
		[]*ViewDefinition{view}, 1)
}

func TestSyncEngine_ShutdownNilAndNeverStarted(t *testing.T) {
	var nilEngine *SyncEngine
	if err := nilEngine.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil engine must be a no-op, got %v", err)
	}
	s := syncEngineForShutdownTest()
	start := time.Now()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("never-started engine must be a no-op, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("never-started Shutdown must return immediately")
	}
}

func TestSyncEngine_ShutdownWaitsForRunExit(t *testing.T) {
	old := ensureTopicsTimeout
	ensureTopicsTimeout = 5 * time.Second
	defer func() { ensureTopicsTimeout = old }()

	s := syncEngineForShutdownTest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// While the loop goroutine is ALIVE, Shutdown must WAIT (dependency),
	// surfacing the drain timeout — never returning success early.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer shortCancel()
	if err := s.Shutdown(shortCtx); err == nil {
		t.Fatal("Shutdown returned before the loop exited — the dependency wait is broken")
	}

	// Once the loop's exit condition fires (ctx cancel → ensureTopics retry
	// loop returns → run returns → done closes), Shutdown unblocks with nil.
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := s.Shutdown(drainCtx); err != nil {
		t.Fatalf("Shutdown must unblock after the loop exits, got %v", err)
	}
}

func TestSyncEngine_DoubleStartIsNoop(t *testing.T) {
	old := ensureTopicsTimeout
	ensureTopicsTimeout = 5 * time.Second
	defer func() { ensureTopicsTimeout = old }()

	s := syncEngineForShutdownTest()
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	s.Start(ctx) // must not spawn a second loop nor double-close done
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := s.Shutdown(drainCtx); err != nil {
		t.Fatalf("unexpected error after double Start: %v", err)
	}
	// A second Shutdown is idempotent too.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated Shutdown must stay nil, got %v", err)
	}
}

// The upstream subscriber follows the same contract once Started: Shutdown
// unblocks ONLY after the supervisor fully exited (its deferred chain drained
// the workers and closed the reader before `done` closed). The supervisor
// exits promptly on the stop signal here (nothing in flight), so the
// assertion is the POST-CONDITION — a nil return implies `done` is already
// closed — not a duration.
func TestUpstreamSubscriber_ShutdownWaitsForRunExit(t *testing.T) {
	sub, err := NewUpstreamSubscriber(nil, nil, nil, identityResolver,
		UpstreamSubscriberConfig{
			Topic:         "shutdown.test.topic",
			Collection:    "shutdown_test",
			ConsumerGroup: "shutdown-test-upstream",
			Workers:       1,
		},
		nil, fakeSubscriber{}, slog.Default())
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if err := sub.Shutdown(drainCtx); err != nil {
		t.Fatalf("Shutdown must unblock after the supervisor exits, got %v", err)
	}
	// The dependency contract: a nil Shutdown means the supervisor RETURNED —
	// done must already be closed (never "returned early while run lives").
	select {
	case <-sub.done:
	default:
		t.Fatal("Shutdown returned nil while the supervisor was still alive — dependency broken")
	}
	// Idempotent second call.
	if err := sub.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated Shutdown must stay nil, got %v", err)
	}
}
