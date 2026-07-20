package bootstrap

import (
	"sync/atomic"
	"testing"
	"time"
)

// stopBootRebuild is serve()'s guarantee that the background boot-rebuild
// goroutine is unwound before runWithConfig closes the stores — so an early
// serve() exit (a listener that failed to bind, a fatal rebuild) never leaves an
// in-flight rebuild reading a just-closed Mongo client ("client is disconnected").
// It must cancel the goroutine's context and then wait for done.
func TestStopBootRebuild_CancelsAndWaits(t *testing.T) {
	// No boot rebuild → no panic, immediate return.
	stopBootRebuild(Deps{})

	// A nil cancel func must not panic; a pre-closed done returns instantly.
	closed := make(chan struct{})
	close(closed)
	stopBootRebuild(Deps{bootRebuild: &bootRebuild{done: closed, errCh: make(chan error, 1)}})

	// The real goroutine reacts to the cancel by unwinding and closing done;
	// emulate that so stopBootRebuild observes the done signal via cancel.
	var cancelled atomic.Bool
	done := make(chan struct{})
	boot := &bootRebuild{
		done:  done,
		errCh: make(chan error, 1),
		cancel: func() {
			if cancelled.CompareAndSwap(false, true) {
				close(done)
			}
		},
	}

	start := time.Now()
	stopBootRebuild(Deps{bootRebuild: boot})
	if !cancelled.Load() {
		t.Fatal("stopBootRebuild must call boot.cancel")
	}
	if time.Since(start) >= bootRebuildStopGrace {
		t.Fatal("stopBootRebuild must return as soon as done closes, not wait the full grace")
	}
}

// When the goroutine ignores its cancellation (done never closes), stopBootRebuild
// must still return, bounded by bootRebuildStopGrace, rather than block forever.
func TestStopBootRebuild_TimeoutBackstop(t *testing.T) {
	orig := bootRebuildStopGrace
	bootRebuildStopGrace = 20 * time.Millisecond
	defer func() { bootRebuildStopGrace = orig }()

	var cancelled atomic.Bool
	boot := &bootRebuild{
		done:   make(chan struct{}), // never closed
		errCh:  make(chan error, 1),
		cancel: func() { cancelled.Store(true) },
	}

	start := time.Now()
	stopBootRebuild(Deps{bootRebuild: boot})
	elapsed := time.Since(start)
	if !cancelled.Load() {
		t.Fatal("stopBootRebuild must call boot.cancel even on the timeout path")
	}
	if elapsed < bootRebuildStopGrace {
		t.Fatalf("stopBootRebuild returned before the grace elapsed (%v)", elapsed)
	}
	if elapsed > bootRebuildStopGrace+2*time.Second {
		t.Fatalf("stopBootRebuild blocked well past the grace (%v)", elapsed)
	}
}
