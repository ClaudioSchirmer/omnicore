package infra

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Error-branch coverage for executor.go that the matrices in executor_more_test
// do not isolate: the outbox-Exec and audit_events-Exec failure returns of the
// flat verbs, and the per-op error returns inside execWithTx (Batch).

func TestFlatInsertUpdate_OutboxError(t *testing.T) {
	t.Run("insert", func(t *testing.T) {
		pool := newFakePool()
		pool.tx.execErr = errFake // data write is QueryRow; the first Exec is the outbox
		pg := newFakePostgres(pool)
		if _, err := pg.Insert(newBuilderCtx(), mustInsertable(t, &builderTestEntity{Name: "a"}), builderTestSchema, writeHook{}); err == nil {
			t.Fatal("expected outbox error")
		}
		if !pool.tx.rolledBack {
			t.Error("expected rollback")
		}
	})
	t.Run("update", func(t *testing.T) {
		pool := newFakePool()
		pool.tx.execErr = errFake
		pg := newFakePostgres(pool)
		if _, err := pg.Update(newBuilderCtx(), mustUpdatable(t, newFlatEntity()), builderTestSchema, writeHook{}); err == nil {
			t.Fatal("expected outbox error")
		}
	})
}

func TestFlatVerbs_AuditError(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pool.tx.execErrSubstr = "audit_events" // data + outbox succeed, only audit fails
			pg := auditedPostgres(pool)
			if err := v.drive(t, pg, newBuilderCtxWithIdentity("u-1", "iss", nil), writeHook{}); err == nil {
				t.Fatalf("%s: expected audit_events Exec error", v.name)
			}
			if pool.tx.committed {
				t.Errorf("%s: must not commit on audit error", v.name)
			}
		})
	}
}

// Flat Archive/Unarchive/Delete write the data row via Exec then the outbox via
// a second Exec; execErrSubstr="outbox" fails only the outbox INSERT.
func TestFlatArchiveUnarchiveDelete_OutboxError(t *testing.T) {
	for _, v := range flatVerbs() {
		if v.dataViaQueryRow {
			continue // Insert/Update covered by TestFlatInsertUpdate_OutboxError
		}
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pool.tx.execErrSubstr = "outbox"
			pg := newFakePostgres(pool)
			if err := v.drive(t, pg, newBuilderCtx(), writeHook{}); err == nil {
				t.Fatalf("%s: expected outbox error", v.name)
			}
			if pool.tx.committed {
				t.Errorf("%s: must not commit on outbox error", v.name)
			}
		})
	}
}

func TestExecWithTx_UpdateQueryRowError(t *testing.T) {
	pool := newFakePool()
	pool.tx.queryRowErr = errFake
	pg := newFakePostgres(pool)
	upd := mustUpdatable(t, newFlatEntity())
	if _, err := pg.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{upd}), []*TableSchema{builderTestSchema}); err == nil {
		t.Fatal("expected Update RETURNING scan error inside execWithTx")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback")
	}
}

func TestExecWithTx_OutboxAndDataErrors(t *testing.T) {
	cases := []struct {
		name   string
		op     func(t *testing.T) domain.ValidEntity
		substr string // empty → fail every Exec (data write); else fail only matching SQL
	}{
		{"archiveOutbox", func(t *testing.T) domain.ValidEntity { return mustArchivable(t, newFlatEntity()) }, "outbox"},
		{"unarchiveData", func(t *testing.T) domain.ValidEntity { return mustUnarchivable(t, newFlatEntity()) }, ""},
		{"unarchiveOutbox", func(t *testing.T) domain.ValidEntity { return mustUnarchivable(t, newFlatEntity()) }, "outbox"},
		{"deleteOutbox", func(t *testing.T) domain.ValidEntity { return mustDeletable(t, newFlatEntity()) }, "outbox"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := newFakePool()
			if c.substr == "" {
				pool.tx.execErr = errFake
			} else {
				pool.tx.execErrSubstr = c.substr
			}
			pg := newFakePostgres(pool)
			op := c.op(t)
			if _, err := pg.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{op}), []*TableSchema{builderTestSchema}); err == nil {
				t.Fatalf("%s: expected error inside execWithTx", c.name)
			}
			if pool.tx.committed {
				t.Errorf("%s: must not commit", c.name)
			}
		})
	}
}
