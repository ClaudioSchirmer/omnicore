//go:build integration && postgres

package postgres

import (
	"context"
	"strings"
	"testing"
)

// End-to-end coverage of the Postgres AcquireRebuildLock against a real
// database: the advisory lock is taken on a pinned pool connection, a competing
// caller sees it held (with a holder diagnostic), and Release frees it. Relocated
// from the former infra-root pg_view lock integration tests once the advisory
// lock moved into the engine behind AcquireRebuildLock.

func TestAcquireRebuildLock_AcquiresAndReleases(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	lock, err := pg.AcquireRebuildLock(ctx, "users")
	if err != nil {
		t.Fatalf("AcquireRebuildLock: %v", err)
	}
	if !lock.Acquired() {
		t.Fatal("expected to acquire the rebuild lock")
	}
	// The pinned-session Querier is usable while the lock is held.
	if lock.Querier() == nil {
		t.Error("expected a non-nil pinned-session Querier")
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAcquireRebuildLock_SecondCallerSeesItHeld(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	first, err := pg.AcquireRebuildLock(ctx, "users")
	if err != nil || !first.Acquired() {
		t.Fatalf("first acquire: acquired=%v err=%v", first.Acquired(), err)
	}
	defer first.Release(ctx)

	// A competing caller (its own pinned connection) must NOT acquire it and
	// must surface a holder diagnostic.
	second, err := pg.AcquireRebuildLock(ctx, "users")
	if err != nil {
		t.Fatalf("second AcquireRebuildLock: %v", err)
	}
	defer second.Release(ctx)
	if second.Acquired() {
		t.Fatal("second caller should NOT acquire the lock")
	}
	if !strings.Contains(second.Holder(), "pid=") {
		t.Errorf("expected a holder diagnostic naming the PID, got %q", second.Holder())
	}
}

func TestAcquireRebuildLock_ReacquireAfterRelease(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	first, err := pg.AcquireRebuildLock(ctx, "users")
	if err != nil || !first.Acquired() {
		t.Fatalf("first acquire: acquired=%v err=%v", first.Acquired(), err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// After release the lock is free again — a fresh acquire must succeed.
	second, err := pg.AcquireRebuildLock(ctx, "users")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	defer second.Release(ctx)
	if !second.Acquired() {
		t.Error("expected to re-acquire the lock after the holder released it")
	}
}

// TestAcquireRebuildLock_HighFNVUpperBitsName locks the classid/objid encoding
// fix: a view name whose FNV-64a upper 32 bits land in the high range
// (> 2^31-1) must still let a competing caller read the holder via pg_locks
// (the oid columns are uint32) without an encode-time failure.
func TestAcquireRebuildLock_HighFNVUpperBitsName(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	first, err := pg.AcquireRebuildLock(ctx, "no_one_holds_me")
	if err != nil || !first.Acquired() {
		t.Fatalf("first acquire: acquired=%v err=%v", first.Acquired(), err)
	}
	defer first.Release(ctx)

	second, err := pg.AcquireRebuildLock(ctx, "no_one_holds_me")
	if err != nil {
		t.Fatalf("second acquire (holder read must not fail on high upper bits): %v", err)
	}
	defer second.Release(ctx)
	if second.Acquired() {
		t.Error("second caller should see the lock held")
	}
}
