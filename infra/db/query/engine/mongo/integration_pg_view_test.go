//go:build integration && postgres

package mongo

import (
	"strings"
	"testing"
	"time"

	"context"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// view_registry: Init / Read / Begin / End / ListNonDone, end-to-end against a
// real Postgres through the engine's neutral Querier/Dialect seam. The advisory
// lock is exercised separately in infra/db/pg (AcquireRebuildLock integration).

func TestInitViewRegistry_AndRead(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()
	q, d := pg.Querier(), pg.Dialect()

	// Missing row → ReadViewRegistry returns (nil, nil).
	row, err := query.ReadViewRegistry(ctx, q, d, "users")
	if err != nil {
		t.Fatalf("ReadViewRegistry on empty: %v", err)
	}
	if row != nil {
		t.Errorf("expected nil row before init, got %+v", row)
	}

	// Initialize.
	now := time.Now().UTC()
	in := query.InitViewRegistryInput{
		ViewName:     "users",
		Version:      1,
		RebuildHash:  "rh",
		ArtifactHash: "ah",
		CombinedHash: "ch",
		ServiceName:  "svc",
		Now:          now,
	}
	if err := query.InitViewRegistry(ctx, q, d, in); err != nil {
		t.Fatalf("InitViewRegistry: %v", err)
	}

	row, err = query.ReadViewRegistry(ctx, q, d, "users")
	if err != nil {
		t.Fatalf("ReadViewRegistry: %v", err)
	}
	if row == nil {
		t.Fatal("expected populated row")
	}
	if row.Status != query.ViewRegistryStatusDone {
		t.Errorf("Status = %q, want done", row.Status)
	}
	if row.Version != 1 || row.CombinedHash != "ch" {
		t.Errorf("row mismatch: %+v", row)
	}
	if !strings.HasPrefix(row.AppliedBy, "svc@pid:") {
		t.Errorf("AppliedBy = %q, want svc@pid:<n>", row.AppliedBy)
	}

	// Double-init violates the primary key.
	if err := query.InitViewRegistry(ctx, q, d, in); err == nil {
		t.Error("expected InitViewRegistry to fail on conflicting row")
	}
}

func TestBeginEndRebuild_StateMachine(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()
	q, d := pg.Querier(), pg.Dialect()

	// Seed.
	if err := query.InitViewRegistry(ctx, q, d, query.InitViewRegistryInput{
		ViewName: "users", Version: 1, RebuildHash: "rh", ArtifactHash: "ah",
		CombinedHash: "ch", ServiceName: "svc", Now: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Begin.
	now := time.Now().UTC()
	if err := query.BeginRebuild(ctx, q, d, "users", now); err != nil {
		t.Fatalf("BeginRebuild: %v", err)
	}
	row, _ := query.ReadViewRegistry(ctx, q, d, "users")
	if row.Status != query.ViewRegistryStatusProcessing {
		t.Errorf("Status after Begin = %q, want processing", row.Status)
	}
	if row.StartedAt == nil || row.PID == nil || row.Host == nil {
		t.Errorf("Begin should populate started_at/pid/host, got %+v", row)
	}

	// End.
	if err := query.EndRebuild(ctx, q, d, query.EndRebuildInput{
		ViewName: "users", Version: 2, RebuildHash: "rh2", ArtifactHash: "ah2",
		CombinedHash: "ch2", ServiceName: "svc", Now: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("EndRebuild: %v", err)
	}
	row, _ = query.ReadViewRegistry(ctx, q, d, "users")
	if row.Status != query.ViewRegistryStatusDone {
		t.Errorf("Status after End = %q, want done", row.Status)
	}
	if row.Version != 2 || row.CombinedHash != "ch2" {
		t.Errorf("End should update version/hashes, got %+v", row)
	}
	if row.PreviousVersion == nil || *row.PreviousVersion != 1 {
		t.Errorf("End should snapshot previous_version=1, got %+v", row.PreviousVersion)
	}
}

func TestListNonDone_PicksUpProcessingViewsOnly(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()
	q, d := pg.Querier(), pg.Dialect()
	now := time.Now().UTC()

	// Two views — one will be left in 'processing', the other in 'done'.
	for _, n := range []string{"users", "orders"} {
		if err := query.InitViewRegistry(ctx, q, d, query.InitViewRegistryInput{
			ViewName: n, Version: 1, RebuildHash: "r", ArtifactHash: "a",
			CombinedHash: "c", ServiceName: "svc", Now: now,
		}); err != nil {
			t.Fatalf("init %s: %v", n, err)
		}
	}
	if err := query.BeginRebuild(ctx, q, d, "users", now); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	rows, err := query.ListNonDone(ctx, q)
	if err != nil {
		t.Fatalf("ListNonDone: %v", err)
	}
	if len(rows) != 1 || rows[0].ViewName != "users" {
		t.Errorf("ListNonDone = %+v, want one entry for 'users'", rows)
	}
}

// FormatRegistryAppliedBy already covered by view_registry_test.go.
