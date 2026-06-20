package infra

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// These tests drive the flat (non-aggregate) write path of executor.go
// against the in-process fakePool, with no live database. They cover the
// happy path plus the failure branches around Begin / QueryRow / Commit.

func mustInsertable(t *testing.T, e domain.Entity) domain.Insertable {
	t.Helper()
	i, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	return i
}

func TestPostgresInsert_FlatHappyPath(t *testing.T) {
	pool := newFakePool()
	pg := newFakePostgres(pool)
	ins := mustInsertable(t, &builderTestEntity{Name: "alice", Email: "a@x.com"})

	res, err := pg.Insert(newBuilderCtx(), ins, builderTestSchema, writeHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if res.ID != pool.tx.scanID {
		t.Errorf("ID = %q, want %q", res.ID, pool.tx.scanID)
	}
	if !pool.tx.committed {
		t.Error("transaction was not committed")
	}
	// Outbox INSERT must have run inside the TX.
	if len(pool.tx.execCalls) == 0 {
		t.Error("expected at least one Exec (outbox) inside the TX")
	}
}

func TestPostgresInsert_BeginError(t *testing.T) {
	pool := newFakePool()
	pool.beginErr = errFake
	pg := newFakePostgres(pool)
	ins := mustInsertable(t, &builderTestEntity{Name: "x"})

	if _, err := pg.Insert(newBuilderCtx(), ins, builderTestSchema, writeHook{}); err == nil {
		t.Fatal("expected error when Begin fails")
	}
}

func TestPostgresInsert_QueryRowError(t *testing.T) {
	pool := newFakePool()
	pool.tx.queryRowErr = errFake
	pg := newFakePostgres(pool)
	ins := mustInsertable(t, &builderTestEntity{Name: "x"})

	if _, err := pg.Insert(newBuilderCtx(), ins, builderTestSchema, writeHook{}); err == nil {
		t.Fatal("expected error when RETURNING id scan fails")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback after QueryRow error")
	}
}

func TestPostgresInsert_CommitError(t *testing.T) {
	pool := newFakePool()
	pool.tx.commitErr = errFake
	pg := newFakePostgres(pool)
	ins := mustInsertable(t, &builderTestEntity{Name: "x"})

	if _, err := pg.Insert(newBuilderCtx(), ins, builderTestSchema, writeHook{}); err == nil {
		t.Fatal("expected error when Commit fails")
	}
}
