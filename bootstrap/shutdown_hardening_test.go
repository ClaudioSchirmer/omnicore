package bootstrap

import (
	"context"
	"testing"
	"time"
)

// TestShutdownConfig_ApplyDefaults pins the drain knobs: zero → framework
// defaults; explicit positive values survive; a NEGATIVE HardGraceSeconds is
// preserved (the watchdog opt-out) rather than being overwritten with a default.
func TestShutdownConfig_ApplyDefaults(t *testing.T) {
	var zero ShutdownConfig
	zero.applyDefaults()
	if zero.DrainTimeoutSeconds != FrameworkDefaultShutdownTimeoutSeconds {
		t.Errorf("DrainTimeoutSeconds default = %d, want %d", zero.DrainTimeoutSeconds, FrameworkDefaultShutdownTimeoutSeconds)
	}
	if zero.TracingDrainSeconds != FrameworkDefaultTracingDrainSeconds {
		t.Errorf("TracingDrainSeconds default = %d, want %d", zero.TracingDrainSeconds, FrameworkDefaultTracingDrainSeconds)
	}
	if zero.HardGraceSeconds != FrameworkDefaultHardGraceSeconds {
		t.Errorf("HardGraceSeconds default = %d, want %d", zero.HardGraceSeconds, FrameworkDefaultHardGraceSeconds)
	}

	explicit := ShutdownConfig{DrainTimeoutSeconds: 12, TracingDrainSeconds: 3, HardGraceSeconds: 7}
	explicit.applyDefaults()
	if explicit.DrainTimeoutSeconds != 12 || explicit.TracingDrainSeconds != 3 || explicit.HardGraceSeconds != 7 {
		t.Errorf("explicit values overwritten: %+v", explicit)
	}

	disabled := ShutdownConfig{HardGraceSeconds: -1}
	disabled.applyDefaults()
	if disabled.HardGraceSeconds != -1 {
		t.Errorf("negative HardGraceSeconds (watchdog opt-out) not preserved: %d", disabled.HardGraceSeconds)
	}
}

// TestServe_OnShutdownHangIsBounded is the core regression guard for fix #2: a
// user OnShutdown hook that BLOCKS ignoring its context must no longer hang the
// drain. serve() races the hook against the drain deadline and returns anyway.
// Watchdog disabled (HardGraceSeconds=-1) so a regression cannot os.Exit the
// test binary; the drain budget is shrunk to 1s so the bound is observed fast.
func TestServe_OnShutdownHangIsBounded(t *testing.T) {
	d := serveDeps()
	d.Config.Shutdown.DrainTimeoutSeconds = 1
	d.Config.Shutdown.HardGraceSeconds = -1

	hold := make(chan struct{})
	defer close(hold) // release the leaked hook goroutine when the test ends
	entered := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- serve(cancelledCtx(), d, Wiring{
			OnShutdown: func(context.Context) error {
				close(entered)
				<-hold // block forever, ignoring ctx — the misbehaving hook
				return nil
			},
		})
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("OnShutdown never ran")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve hung on a blocking OnShutdown — the drain-deadline race regressed")
	}
}

// TestServe_WatchdogDisabledByNegativeGrace covers the opt-out branch: a
// negative HardGraceSeconds skips the watchdog + second-signal arming entirely,
// and a clean cancelled-context drain still returns nil.
func TestServe_WatchdogDisabledByNegativeGrace(t *testing.T) {
	d := serveDeps()
	d.Config.Shutdown.HardGraceSeconds = -1
	if err := serve(cancelledCtx(), d, Wiring{}); err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// TestServe_TracingDrainBudgetHonored exercises the dedicated tracing sub-budget
// path (fix #1) with an explicit small value; the inert test tracer returns
// immediately, but the traceCtx construction + default-resolution lines run.
func TestServe_TracingDrainBudgetHonored(t *testing.T) {
	d := serveDeps()
	d.Config.Shutdown.TracingDrainSeconds = 2
	d.Config.Shutdown.HardGraceSeconds = -1
	if err := serve(cancelledCtx(), d, Wiring{}); err != nil {
		t.Fatalf("serve: %v", err)
	}
}
