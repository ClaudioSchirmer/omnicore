//go:build integration

package infra

import (
	"context"
	"strings"
	"testing"
	"time"
)

// --- pg_view_lock: TryAcquire / Release / ReadHolder ---------------------

func TestTryAcquireViewLock_AcquiresAndReleases(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	// Pin a connection so the lock's lifetime is bound to it.
	conn, err := pg.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()

	got, err := TryAcquireViewLock(ctx, conn, "users")
	if err != nil {
		t.Fatalf("TryAcquireViewLock: %v", err)
	}
	if !got {
		t.Fatal("expected to acquire the advisory lock")
	}

	if err := ReleaseViewLock(ctx, conn, "users"); err != nil {
		t.Fatalf("ReleaseViewLock: %v", err)
	}
}

func TestTryAcquireViewLock_SecondCallerSeesItHeld(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	// Pin two distinct connections — the second represents a competing
	// process / boot.
	connA, err := pg.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	defer connA.Release()
	connB, err := pg.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire B: %v", err)
	}
	defer connB.Release()

	a, err := TryAcquireViewLock(ctx, connA, "users")
	if err != nil || !a {
		t.Fatalf("first acquire: a=%v err=%v", a, err)
	}
	b, err := TryAcquireViewLock(ctx, connB, "users")
	if err != nil {
		t.Fatalf("second TryAcquire: %v", err)
	}
	if b {
		t.Fatal("second caller should NOT acquire the lock")
	}

	// ReadViewLockHolder must surface the holder's PID via pg_locks.
	holder, err := ReadViewLockHolder(ctx, connB, "users")
	if err != nil {
		t.Fatalf("ReadViewLockHolder: %v", err)
	}
	if holder == nil {
		t.Fatal("expected non-nil holder")
	}
	if holder.PID == 0 {
		t.Error("expected non-zero PID on holder")
	}

	_ = ReleaseViewLock(ctx, connA, "users")
}

func TestReadViewLockHolder_NobodyHoldingReturnsNil(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()

	// "users" — chosen because its FNV hash keeps the upper 32 bits in the
	// non-negative int32 range so the pg_locks(classid::oid) binding works.
	// Other names can trip BUGS_FOUND.md item 003 — see
	// TestReadViewLockHolder_ViewNameWithNegativeUpper.
	holder, err := ReadViewLockHolder(context.Background(), pg.Pool(), "users")
	if err != nil {
		t.Fatalf("ReadViewLockHolder: %v", err)
	}
	if holder != nil {
		t.Errorf("expected nil holder when no one is locking, got %+v", holder)
	}
}

// TestReadViewLockHolder_NameWithUpper32BitsHighSucceeds locks the fix of
// the original encoding regression: ReadViewLockHolder forwards classid +
// objid as uint32 (matching pg_locks.classid / pg_locks.objid which are oid
// columns). FNV hashes whose upper 32 bits land in the high range
// (> 2^31-1) used to fail at encode time when the helper returned int32;
// now they round-trip cleanly and the no-holder query returns (nil, nil).
func TestReadViewLockHolder_NameWithUpper32BitsHighSucceeds(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()

	// "no_one_holds_me" is one of many view names whose FNV upper 32 bits
	// fall in the high range (would have produced a negative int32 under
	// the prior helper signature).
	holder, err := ReadViewLockHolder(context.Background(), pg.Pool(), "no_one_holds_me")
	if err != nil {
		t.Fatalf("expected (nil, nil) for absent holder, got err=%v", err)
	}
	if holder != nil {
		t.Errorf("expected nil holder when no one is locking, got %+v", holder)
	}
}

func TestReleaseViewLock_OnConnectionWithoutLockErrors(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()
	conn, err := pg.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()

	err = ReleaseViewLock(ctx, conn, "never_locked")
	if err == nil {
		t.Error("expected error when releasing a lock this connection never held")
	}
	if !strings.Contains(err.Error(), "not held") {
		t.Errorf("error should mention 'not held', got %v", err)
	}
}

// --- pg_view_registry: Init / Read / Begin / End / ListNonDone -----------

func TestInitViewRegistry_AndRead(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	// Missing row → ReadViewRegistry returns (nil, nil).
	row, err := ReadViewRegistry(ctx, pg.Pool(), "users")
	if err != nil {
		t.Fatalf("ReadViewRegistry on empty: %v", err)
	}
	if row != nil {
		t.Errorf("expected nil row before init, got %+v", row)
	}

	// Initialize.
	now := time.Now().UTC()
	in := InitViewRegistryInput{
		ViewName:     "users",
		Version:      1,
		RebuildHash:  "rh",
		ArtifactHash: "ah",
		CombinedHash: "ch",
		ServiceName:  "svc",
		Now:          now,
	}
	if err := InitViewRegistry(ctx, pg.Pool(), in); err != nil {
		t.Fatalf("InitViewRegistry: %v", err)
	}

	row, err = ReadViewRegistry(ctx, pg.Pool(), "users")
	if err != nil {
		t.Fatalf("ReadViewRegistry: %v", err)
	}
	if row == nil {
		t.Fatal("expected populated row")
	}
	if row.Status != ViewRegistryStatusDone {
		t.Errorf("Status = %q, want done", row.Status)
	}
	if row.Version != 1 || row.CombinedHash != "ch" {
		t.Errorf("row mismatch: %+v", row)
	}
	if !strings.HasPrefix(row.AppliedBy, "svc@pid:") {
		t.Errorf("AppliedBy = %q, want svc@pid:<n>", row.AppliedBy)
	}

	// Double-init violates the primary key.
	if err := InitViewRegistry(ctx, pg.Pool(), in); err == nil {
		t.Error("expected InitViewRegistry to fail on conflicting row")
	}
}

func TestBeginEndRebuild_StateMachine(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	// Seed.
	if err := InitViewRegistry(ctx, pg.Pool(), InitViewRegistryInput{
		ViewName: "users", Version: 1, RebuildHash: "rh", ArtifactHash: "ah",
		CombinedHash: "ch", ServiceName: "svc", Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Begin.
	now := time.Now().UTC()
	if err := BeginRebuild(ctx, pg.Pool(), "users", now); err != nil {
		t.Fatalf("BeginRebuild: %v", err)
	}
	row, _ := ReadViewRegistry(ctx, pg.Pool(), "users")
	if row.Status != ViewRegistryStatusProcessing {
		t.Errorf("Status after Begin = %q, want processing", row.Status)
	}
	if row.StartedAt == nil || row.PID == nil || row.Host == nil {
		t.Errorf("Begin should populate started_at/pid/host, got %+v", row)
	}

	// End.
	if err := EndRebuild(ctx, pg.Pool(), EndRebuildInput{
		ViewName: "users", Version: 2, RebuildHash: "rh2", ArtifactHash: "ah2",
		CombinedHash: "ch2", ServiceName: "svc", Now: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("EndRebuild: %v", err)
	}
	row, _ = ReadViewRegistry(ctx, pg.Pool(), "users")
	if row.Status != ViewRegistryStatusDone {
		t.Errorf("Status after End = %q, want done", row.Status)
	}
	if row.Version != 2 || row.CombinedHash != "ch2" {
		t.Errorf("End should update version/hashes, got %+v", row)
	}
	if row.PreviousVersion == nil || *row.PreviousVersion != 1 {
		t.Errorf("End should snapshot previous_version=1, got %+v", row.PreviousVersion)
	}
}

func TestBeginRebuild_MissingRowFails(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	err := BeginRebuild(context.Background(), pg.Pool(), "ghost", time.Now().UTC())
	if err == nil {
		t.Error("expected BeginRebuild to fail when no row exists")
	}
}

func TestEndRebuild_MissingRowFails(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	err := EndRebuild(context.Background(), pg.Pool(), EndRebuildInput{
		ViewName: "ghost", Version: 1, Now: time.Now().UTC(),
	})
	if err == nil {
		t.Error("expected EndRebuild to fail when no row exists")
	}
}

func TestListNonDone_PicksUpProcessingViewsOnly(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// Two views — one will be left in 'processing', the other in 'done'.
	for _, n := range []string{"users", "orders"} {
		if err := InitViewRegistry(ctx, pg.Pool(), InitViewRegistryInput{
			ViewName: n, Version: 1, RebuildHash: "r", ArtifactHash: "a",
			CombinedHash: "c", ServiceName: "svc", Now: now,
		}); err != nil {
			t.Fatalf("init %s: %v", n, err)
		}
	}
	if err := BeginRebuild(ctx, pg.Pool(), "users", now); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	rows, err := ListNonDone(ctx, pg.Pool())
	if err != nil {
		t.Fatalf("ListNonDone: %v", err)
	}
	if len(rows) != 1 || rows[0].ViewName != "users" {
		t.Errorf("ListNonDone = %+v, want one entry for 'users'", rows)
	}
}

// FormatRegistryAppliedBy already covered by pg_view_registry_test.go.
