package bootstrap

import (
	"context"
	"strings"
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

	boot := &bootRebuild{done: make(chan struct{}), errCh: make(chan error, 1), total: 3}
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

// The 503 reason must name the view under rebuild and its position once the
// goroutine records progress, and fall back to the generic message in the
// reconcile window before the first view starts.
func TestReadiness_BootRebuildReason(t *testing.T) {
	ctx := context.Background()
	boot := &bootRebuild{done: make(chan struct{}), errCh: make(chan error, 1), total: 5}
	r := &readiness{boot: boot}

	// No progress recorded yet → generic reason.
	err := r.check(ctx)
	if err == nil || err.Error() != "initializing: view rebuild in progress" {
		t.Fatalf("pre-progress reason must be generic, got %v", err)
	}

	// A view under rebuild → reason names it with its 1-based position.
	boot.progress.Store(&rebuildProgress{view: "users_view", index: 2})
	err = r.check(ctx)
	if err == nil {
		t.Fatal("a rebuild in progress must report not-ready")
	}
	if got, want := err.Error(), `initializing: rebuilding view "users_view" (2/5)`; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}

	// Sanity: it stays a substring-friendly, operator-readable string.
	if !strings.Contains(err.Error(), "users_view") {
		t.Fatalf("reason must name the view, got %q", err.Error())
	}
}
