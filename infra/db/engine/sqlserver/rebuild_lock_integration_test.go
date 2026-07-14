//go:build integration && sqlserver

package sqlserver

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// The Mongo-view rebuild control plane is backend-neutral, so its concurrency
// primitive (AcquireRebuildLock) and the registry status writes run on SQL
// Server too. This proves the genuinely SQL Server-specific bits against a
// real container:
//
//	go test -tags=integration,sqlserver ./infra/db/engine/sqlserver/ -count=1
//
//   - sp_getapplock @LockOwner='Session' acquires the applock on a pinned
//     session (and SURVIVES the registry writes' transactions — the reason
//     'Transaction' ownership is forbidden);
//   - a second acquire of the same view on another session is excluded
//     (Acquired()==false) and reports a holder via sys.dm_tran_locks;
//   - the pinned-session Querier writes the registry row, so
//     BeginRebuild/EndRebuild co-locate with the lock — done→processing→done;
//   - sp_releaseapplock frees the lock so a later acquire succeeds again.

func createMongoViewsTable(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, `CREATE TABLE omnicore_mongo_views (
		view_name              NVARCHAR(255) NOT NULL,
		version                INTEGER       NOT NULL,
		rebuild_hash           VARCHAR(64)   NOT NULL,
		artifact_hash          VARCHAR(64)   NOT NULL,
		combined_hash          VARCHAR(64)   NOT NULL,
		previous_version       INTEGER       NULL,
		previous_combined_hash VARCHAR(64)   NULL,
		previous_applied_at    DATETIME2(6)  NULL,
		status                 VARCHAR(16)   NOT NULL DEFAULT 'done',
		started_at             DATETIME2(6)  NULL,
		pid                    VARCHAR(64)   NULL,
		host                   NVARCHAR(255) NULL,
		applied_at             DATETIME2(6)  NOT NULL,
		applied_by             NVARCHAR(255) NOT NULL,
		code_version           NVARCHAR(255) NULL,
		PRIMARY KEY (view_name)
	)`); err != nil {
		t.Fatalf("create omnicore_mongo_views: %v", err)
	}
}

func TestSQLServerRebuildLock_AcquireExcludeRegistryRelease(t *testing.T) {
	eng, raw := setup(t)
	createMongoViewsTable(t, raw)
	ctx := context.Background()
	const view = "users"

	// 1. Acquire the lock on a pinned session.
	lock1, err := eng.AcquireRebuildLock(ctx, view)
	if err != nil {
		t.Fatalf("AcquireRebuildLock #1: %v", err)
	}
	if !lock1.Acquired() {
		t.Fatalf("first AcquireRebuildLock should win the lock, got Acquired()=false (holder=%q)", lock1.Holder())
	}

	// 2. A second acquire of the same view on another session is excluded.
	lock2, err := eng.AcquireRebuildLock(ctx, view)
	if err != nil {
		t.Fatalf("AcquireRebuildLock #2: %v", err)
	}
	if lock2.Acquired() {
		t.Fatal("second AcquireRebuildLock must NOT win the lock while the first holds it")
	}
	if h := lock2.Holder(); !strings.Contains(h, "session=") {
		t.Errorf("excluded acquire should report a holder via sys.dm_tran_locks, got %q", h)
	}
	if err := lock2.Release(ctx); err != nil {
		t.Errorf("Release of a non-acquired lock should be clean, got %v", err)
	}

	// 3. The registry row is initialized on the pool, then driven done→processing
	//    →done through the FIRST lock's pinned-session Querier.
	now := time.Now().UTC().Truncate(time.Second)
	if err := query.InitViewRegistry(ctx, eng.Querier(), eng.Dialect(), query.InitViewRegistryInput{
		ViewName: view, Version: 1,
		RebuildHash: "rh1", ArtifactHash: "ah1", CombinedHash: "ch1",
		ServiceName: "sqlserver-test", Now: now,
	}); err != nil {
		t.Fatalf("InitViewRegistry: %v", err)
	}

	if err := query.BeginRebuild(ctx, lock1.Querier(), eng.Dialect(), view, now); err != nil {
		t.Fatalf("BeginRebuild (pinned session): %v", err)
	}
	row, err := query.ReadViewRegistry(ctx, eng.Querier(), eng.Dialect(), view)
	if err != nil || row == nil {
		t.Fatalf("ReadViewRegistry after begin: row=%v err=%v", row, err)
	}
	if row.Status != query.ViewRegistryStatusProcessing {
		t.Errorf("after BeginRebuild status = %q, want processing", row.Status)
	}

	if err := query.EndRebuild(ctx, lock1.Querier(), eng.Dialect(), query.EndRebuildInput{
		ViewName: view, Version: 2,
		RebuildHash: "rh2", ArtifactHash: "ah2", CombinedHash: "ch2",
		ServiceName: "sqlserver-test", Now: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("EndRebuild (pinned session): %v", err)
	}
	row, err = query.ReadViewRegistry(ctx, eng.Querier(), eng.Dialect(), view)
	if err != nil || row == nil {
		t.Fatalf("ReadViewRegistry after end: row=%v err=%v", row, err)
	}
	if row.Status != query.ViewRegistryStatusDone {
		t.Errorf("after EndRebuild status = %q, want done", row.Status)
	}
	if row.CombinedHash != "ch2" || row.Version != 2 {
		t.Errorf("after EndRebuild row = {v:%d ch:%q}, want {2 ch2}", row.Version, row.CombinedHash)
	}
	if row.PreviousCombinedHash == nil || *row.PreviousCombinedHash != "ch1" {
		t.Errorf("EndRebuild should capture previous_combined_hash=ch1, got %v", row.PreviousCombinedHash)
	}

	// 4. Release frees the lock; a later acquire wins again.
	if err := lock1.Release(ctx); err != nil {
		t.Fatalf("Release of the held lock: %v", err)
	}
	lock3, err := eng.AcquireRebuildLock(ctx, view)
	if err != nil {
		t.Fatalf("AcquireRebuildLock #3: %v", err)
	}
	if !lock3.Acquired() {
		t.Errorf("after Release the lock should be free, but acquire #3 was excluded (holder=%q)", lock3.Holder())
	}
	_ = lock3.Release(ctx)

	// ReadViewRegistry on an unknown view returns (nil, nil) — the neutral
	// no-rows path (presence-by-Next, no driver sentinel).
	missing, err := query.ReadViewRegistry(ctx, eng.Querier(), eng.Dialect(), "no-such-view")
	if err != nil {
		t.Fatalf("ReadViewRegistry(missing): %v", err)
	}
	if missing != nil {
		t.Errorf("ReadViewRegistry on unknown view should be nil, got %+v", missing)
	}
}
