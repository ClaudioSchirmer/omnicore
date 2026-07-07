package resilience

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func enabledPolicy() BreakerPolicy {
	return BreakerPolicy{Enabled: true, FailureThreshold: 2, SuccessThreshold: 2, OpenFor: 50 * time.Millisecond}
}

func TestBreakerStateStringLabels(t *testing.T) {
	cases := map[BreakerState]string{
		BreakerClosed:    "closed",
		BreakerOpen:      "open",
		BreakerHalfOpen:  "half-open",
		BreakerState(99): "closed", // default
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("String(%d) = %q, want %q", k, got, want)
		}
	}
}

func TestBreakerNilAndDisabledAlwaysAllow(t *testing.T) {
	var nilB *Breaker
	if ok, label := nilB.Allow(); !ok || label != "closed" {
		t.Fatalf("nil breaker: %v %q", ok, label)
	}
	nilB.RecordSuccess()
	nilB.RecordFailure()
	if nilB.SnapshotState() != "closed" {
		t.Fatalf("nil snapshot")
	}
	disabled := NewBreaker(BreakerPolicy{})
	if ok, _ := disabled.Allow(); !ok {
		t.Fatalf("disabled breaker must allow")
	}
	disabled.RecordFailure()
	if disabled.SnapshotState() != "closed" {
		t.Fatalf("disabled breaker must stay closed")
	}
}

func TestBreakerFullCycle(t *testing.T) {
	b := NewBreaker(enabledPolicy())

	// closed: failures below threshold keep it closed; a success resets.
	b.RecordFailure()
	b.RecordSuccess()
	b.RecordFailure()
	if b.SnapshotState() != "closed" {
		t.Fatalf("threshold not reached, want closed")
	}

	// second consecutive failure opens.
	b.RecordFailure()
	b.RecordFailure()
	if b.SnapshotState() != "open" {
		t.Fatalf("want open, got %s", b.SnapshotState())
	}
	if ok, label := b.Allow(); ok || label != "open" {
		t.Fatalf("open must reject: %v %q", ok, label)
	}

	// after OpenFor elapses: half-open, single probe admitted.
	time.Sleep(60 * time.Millisecond)
	ok, label := b.Allow()
	if !ok || label != "half-open" {
		t.Fatalf("probe not admitted: %v %q", ok, label)
	}
	if ok, _ := b.Allow(); ok {
		t.Fatalf("second concurrent probe must be rejected")
	}

	// probe succeeds twice → closed.
	b.RecordSuccess()
	if ok, _ := b.Allow(); !ok {
		t.Fatalf("next probe after success must be admitted")
	}
	b.RecordSuccess()
	if b.SnapshotState() != "closed" {
		t.Fatalf("successThreshold reached, want closed, got %s", b.SnapshotState())
	}
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	b := NewBreaker(enabledPolicy())
	b.RecordFailure()
	b.RecordFailure()
	time.Sleep(60 * time.Millisecond)
	if ok, _ := b.Allow(); !ok {
		t.Fatalf("probe not admitted")
	}
	b.RecordFailure()
	if b.SnapshotState() != "open" {
		t.Fatalf("half-open failure must reopen, got %s", b.SnapshotState())
	}
}

func TestBackoffCurves(t *testing.T) {
	base := 10 * time.Millisecond
	maxD := 35 * time.Millisecond
	cases := []struct {
		strategy BackoffStrategy
		attempt  int
		want     time.Duration
	}{
		{BackoffConstant, 1, base},
		{BackoffConstant, 5, base},
		{BackoffLinear, 3, 30 * time.Millisecond},
		{BackoffLinear, 4, maxD}, // capped
		{BackoffExponential, 1, base},
		{BackoffExponential, 2, 20 * time.Millisecond},
		{BackoffExponential, 3, maxD}, // capped
		{BackoffStrategy(0), 2, base}, // default falls back to initial
		{BackoffExponential, 0, base}, // attempt < 1 clamps to 1
	}
	for _, tc := range cases {
		got := Backoff(BackoffPolicy{Strategy: tc.strategy, InitialDelay: base, MaxDelay: maxD}, tc.attempt)
		if got != tc.want {
			t.Errorf("strategy %d attempt %d: want %v, got %v", tc.strategy, tc.attempt, got, tc.want)
		}
	}
}

func TestBackoffZeroInitialDelay(t *testing.T) {
	if got := Backoff(BackoffPolicy{Strategy: BackoffConstant, MaxDelay: time.Second}, 3); got != 0 {
		t.Fatalf("no initial delay must yield 0, got %v", got)
	}
}

func TestBackoffJitterWithinCeiling(t *testing.T) {
	p := BackoffPolicy{Strategy: BackoffExponentialJitter, InitialDelay: 10 * time.Millisecond, MaxDelay: 25 * time.Millisecond}
	for i := 0; i < 50; i++ {
		got := Backoff(p, 3) // ceiling = 40ms → capped at 25ms
		if got < 0 || got > p.MaxDelay {
			t.Fatalf("jitter out of range: %v", got)
		}
	}
}

func TestBackoffOverflowFallsBackToMax(t *testing.T) {
	// a large attempt overflows the exponential shift into negative — the
	// guard returns MaxDelay (1s << 34 wraps past int64 max).
	p := BackoffPolicy{Strategy: BackoffExponential, InitialDelay: time.Second, MaxDelay: 30 * time.Second}
	if got := Backoff(p, 35); got != p.MaxDelay {
		t.Fatalf("overflow must cap at MaxDelay, got %v", got)
	}
}

func TestJitterNonPositive(t *testing.T) {
	if Jitter(0) != 0 || Jitter(-5) != 0 {
		t.Fatalf("non-positive ceiling must yield 0")
	}
	if v := Jitter(1000); v < 0 || v >= 1000 {
		t.Fatalf("jitter out of range: %d", v)
	}
}

func TestSleepCtx(t *testing.T) {
	if !SleepCtx(context.Background(), 0) {
		t.Fatalf("zero sleep on live ctx must complete")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if SleepCtx(canceled, 0) {
		t.Fatalf("zero sleep on canceled ctx must abort")
	}
	if SleepCtx(canceled, 10*time.Millisecond) {
		t.Fatalf("sleep on canceled ctx must abort")
	}
	if !SleepCtx(context.Background(), time.Millisecond) {
		t.Fatalf("short sleep must complete")
	}
}

func TestNewIdempotencyKeyIsUUID(t *testing.T) {
	k, err := NewIdempotencyKey()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	parsed, err := uuid.Parse(k)
	if err != nil || parsed.Version() != 7 {
		t.Fatalf("want UUIDv7, got %q (err %v)", k, err)
	}
}

func TestBreakerAllowUnknownStateDefaultsToAdmit(t *testing.T) {
	// defensive default arm of the state switch — unreachable through the
	// public transitions, pinned here by constructing the state directly
	// (same-package test).
	b := NewBreaker(enabledPolicy())
	b.state = BreakerState(99)
	if ok, label := b.Allow(); !ok || label != "closed" {
		t.Fatalf("unknown state must admit as closed: %v %q", ok, label)
	}
}
