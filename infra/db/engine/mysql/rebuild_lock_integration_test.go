//go:build integration && mysql

package mysql

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/read/mongo"
)

// Item-8 integration test: the Mongo-view rebuild control plane is backend-
// neutral, so its concurrency primitive (AcquireRebuildLock) and the registry
// status writes run on MySQL too. This proves the genuinely-new, MySQL-specific
// bits against a real MySQL 8.4 container (devops/docker-compose.yml `mysql`
// service, host :3307):
//
//	go test -tags=integration,mysql ./infra/db/mysql/ -count=1
//
//   - GET_LOCK(name, 0) acquires the named user-level lock on a pinned session;
//   - a second acquire of the same view on another session is excluded
//     (Acquired()==false) and reports a holder via IS_USED_LOCK;
//   - the pinned-session Querier (Fork B) writes the registry row, so
//     BeginRebuild/EndRebuild co-locate with the lock — done→processing→done;
//   - RELEASE_LOCK frees the lock so a later acquire succeeds again.

func createMongoViewsTable(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS omnicore_mongo_views`); err != nil {
		t.Fatalf("drop omnicore_mongo_views: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE omnicore_mongo_views (
		view_name              VARCHAR(255) NOT NULL,
		version                INTEGER      NOT NULL,
		rebuild_hash           VARCHAR(64)  NOT NULL,
		artifact_hash          VARCHAR(64)  NOT NULL,
		combined_hash          VARCHAR(64)  NOT NULL,
		previous_version       INTEGER      NULL,
		previous_combined_hash VARCHAR(64)  NULL,
		previous_applied_at    DATETIME     NULL,
		status                 VARCHAR(16)  NOT NULL DEFAULT 'done',
		started_at             DATETIME     NULL,
		pid                    VARCHAR(64)  NULL,
		host                   VARCHAR(255) NULL,
		applied_at             DATETIME     NOT NULL,
		applied_by             VARCHAR(255) NOT NULL,
		code_version           VARCHAR(255) NULL,
		PRIMARY KEY (view_name)
	)`); err != nil {
		t.Fatalf("create omnicore_mongo_views: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS omnicore_mongo_views`) })
}

func TestMySQLRebuildLock_AcquireExcludeRegistryRelease(t *testing.T) {
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
	if h := lock2.Holder(); !strings.Contains(h, "connection=") {
		t.Errorf("excluded acquire should report a holder via IS_USED_LOCK, got %q", h)
	}
	if err := lock2.Release(ctx); err != nil {
		t.Errorf("Release of a non-acquired lock should be clean, got %v", err)
	}

	// 3. The registry row is initialized on the pool, then driven done→processing
	//    →done through the FIRST lock's pinned-session Querier (Fork B).
	now := time.Now().UTC().Truncate(time.Second)
	if err := mongo.InitViewRegistry(ctx, eng.Querier(), eng.Dialect(), mongo.InitViewRegistryInput{
		ViewName: view, Version: 1,
		RebuildHash: "rh1", ArtifactHash: "ah1", CombinedHash: "ch1",
		ServiceName: "mysql-test", Now: now,
	}); err != nil {
		t.Fatalf("InitViewRegistry: %v", err)
	}

	if err := mongo.BeginRebuild(ctx, lock1.Querier(), eng.Dialect(), view, now); err != nil {
		t.Fatalf("BeginRebuild (pinned session): %v", err)
	}
	row, err := mongo.ReadViewRegistry(ctx, eng.Querier(), eng.Dialect(), view)
	if err != nil || row == nil {
		t.Fatalf("ReadViewRegistry after begin: row=%v err=%v", row, err)
	}
	if row.Status != mongo.ViewRegistryStatusProcessing {
		t.Errorf("after BeginRebuild status = %q, want processing", row.Status)
	}

	if err := mongo.EndRebuild(ctx, lock1.Querier(), eng.Dialect(), mongo.EndRebuildInput{
		ViewName: view, Version: 2,
		RebuildHash: "rh2", ArtifactHash: "ah2", CombinedHash: "ch2",
		ServiceName: "mysql-test", Now: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("EndRebuild (pinned session): %v", err)
	}
	row, err = mongo.ReadViewRegistry(ctx, eng.Querier(), eng.Dialect(), view)
	if err != nil || row == nil {
		t.Fatalf("ReadViewRegistry after end: row=%v err=%v", row, err)
	}
	if row.Status != mongo.ViewRegistryStatusDone {
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
	// no-rows path (database/sql sentinel differs from pgx; presence-by-Next).
	missing, err := mongo.ReadViewRegistry(ctx, eng.Querier(), eng.Dialect(), "no-such-view")
	if err != nil {
		t.Fatalf("ReadViewRegistry(missing): %v", err)
	}
	if missing != nil {
		t.Errorf("ReadViewRegistry on unknown view should be nil, got %+v", missing)
	}
}
