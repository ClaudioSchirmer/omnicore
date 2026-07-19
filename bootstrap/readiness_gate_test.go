package bootstrap

import (
	"context"
	"testing"
)

// The readiness probe must report not-ready while the background boot rebuild
// runs (so Kubernetes keeps the pod out of rotation) and ready once it opens the
// gate. With nil stores the only signal is the boot gate.
func TestReadiness_BootRebuildGate(t *testing.T) {
	ctx := context.Background()

	// No boot rebuild → not gated (nil db/mongo, not draining → ready).
	if err := (&readiness{}).check(ctx); err != nil {
		t.Fatalf("no boot rebuild must be ready, got %v", err)
	}

	boot := &bootRebuild{done: make(chan struct{}), errCh: make(chan error, 1)}
	r := &readiness{boot: boot}

	// Rebuild in progress → 503.
	if err := r.check(ctx); err == nil {
		t.Fatal("a rebuild in progress must report not-ready")
	}

	// Rebuild finished → ready.
	boot.complete.Store(true)
	if err := r.check(ctx); err != nil {
		t.Fatalf("after the rebuild the pod must be ready, got %v", err)
	}
}
