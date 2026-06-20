package infra

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
)

// These tests extend executor_unit_test.go to the remaining flat write verbs
// (Update / Archive / Unarchive / Delete), the audit-enabled branch
// (writeAuditRow + echoAuditSlog + Build*Event inside the TX) and the
// lifecycle-hook branches (AfterBegin / BeforeCommit, including the
// error-rolls-back path). Everything runs against the in-process fakePool.

// auditedPostgres wires a Postgres over the fake pool with BOTH audit
// destinations active so writeAuditRow (in-TX) and echoAuditSlog (post-commit)
// both fire. A nil logger degrades to slog.Default() inside the echo path.
func auditedPostgres(pool pgxPool) *Postgres {
	cfg := &audit.Config{Destinations: []audit.Destination{audit.DestinationSlog, audit.DestinationDatabase}}
	return (&Postgres{pool: pool}).WithAudit(cfg, nil, nil)
}

func newFlatEntity() *builderTestEntity {
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))
	return e
}

func mustUpdatable(t *testing.T, e *builderTestEntity) domain.Updatable {
	t.Helper()
	u, err := domain.GetUpdatable(e, func(x *builderTestEntity) { x.Name = "bob" }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return u
}

func mustArchivable(t *testing.T, e *builderTestEntity) domain.Archivable {
	t.Helper()
	a, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	return a
}

func mustUnarchivable(t *testing.T, e *builderTestEntity) domain.Unarchivable {
	t.Helper()
	u, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	return u
}

func mustDeletable(t *testing.T, e *builderTestEntity) domain.Deletable {
	t.Helper()
	d, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	return d
}

// flatVerb describes one flat write verb so the scenario matrix below can drive
// each through the same begin/data/commit/hook error branches. dataViaQueryRow
// marks the verbs whose data write is a RETURNING-id QueryRow (Insert/Update)
// versus an Exec (Archive/Unarchive/Delete).
type flatVerb struct {
	name            string
	dataViaQueryRow bool
	drive           func(t *testing.T, pg *Postgres, ctx persistence.RequestContext, hook writeHook) error
}

func flatVerbs() []flatVerb {
	return []flatVerb{
		{
			name:            "Insert",
			dataViaQueryRow: true,
			drive: func(t *testing.T, pg *Postgres, ctx persistence.RequestContext, hook writeHook) error {
				_, err := pg.Insert(ctx, mustInsertable(t, &builderTestEntity{Name: "alice", Email: "a@x.com"}), builderTestSchema, hook)
				return err
			},
		},
		{
			name:            "Update",
			dataViaQueryRow: true,
			drive: func(t *testing.T, pg *Postgres, ctx persistence.RequestContext, hook writeHook) error {
				_, err := pg.Update(ctx, mustUpdatable(t, newFlatEntity()), builderTestSchema, hook)
				return err
			},
		},
		{
			name:            "Archive",
			dataViaQueryRow: false,
			drive: func(t *testing.T, pg *Postgres, ctx persistence.RequestContext, hook writeHook) error {
				return pg.Archive(ctx, mustArchivable(t, newFlatEntity()), builderTestSchema, hook)
			},
		},
		{
			name:            "Unarchive",
			dataViaQueryRow: false,
			drive: func(t *testing.T, pg *Postgres, ctx persistence.RequestContext, hook writeHook) error {
				return pg.Unarchive(ctx, mustUnarchivable(t, newFlatEntity()), builderTestSchema, hook)
			},
		},
		{
			name:            "Delete",
			dataViaQueryRow: false,
			drive: func(t *testing.T, pg *Postgres, ctx persistence.RequestContext, hook writeHook) error {
				return pg.Delete(ctx, mustDeletable(t, newFlatEntity()), builderTestSchema, hook)
			},
		},
	}
}

func TestFlatVerbs_HappyPath_NoAudit(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pg := newFakePostgres(pool)
			if err := v.drive(t, pg, newBuilderCtx(), writeHook{}); err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			if !pool.tx.committed {
				t.Error("transaction was not committed")
			}
			if pool.tx.rolledBack {
				// Rollback is always deferred, but a committed TX should report committed.
			}
			if len(pool.tx.execCalls) == 0 {
				t.Error("expected at least one Exec (outbox) inside the TX")
			}
		})
	}
}

func TestFlatVerbs_HappyPath_AuditEnabled(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pg := auditedPostgres(pool)
			if err := v.drive(t, pg, newBuilderCtxWithIdentity("u-1", "iss", nil), writeHook{}); err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			if !pool.tx.committed {
				t.Error("audited transaction was not committed")
			}
			// outbox Exec + audit_events Exec must both have landed in the TX.
			if len(pool.tx.execCalls) < 2 {
				t.Errorf("expected outbox + audit Exec inside the TX, got %d execs", len(pool.tx.execCalls))
			}
		})
	}
}

func TestFlatVerbs_BeginError(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pool.beginErr = errFake
			pg := newFakePostgres(pool)
			if err := v.drive(t, pg, newBuilderCtx(), writeHook{}); err == nil {
				t.Fatal("expected error when Begin fails")
			}
		})
	}
}

func TestFlatVerbs_DataWriteError_RollsBack(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			// Fail whichever driver path materializes the data row first.
			pool.tx.queryRowErr = errFake
			pool.tx.execErr = errFake
			pg := newFakePostgres(pool)
			if err := v.drive(t, pg, newBuilderCtx(), writeHook{}); err == nil {
				t.Fatal("expected error from the data write")
			}
			if !pool.tx.rolledBack {
				t.Error("expected rollback after the data write error")
			}
			if pool.tx.committed {
				t.Error("must not commit after a data write error")
			}
		})
	}
}

func TestFlatVerbs_CommitError(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pool.tx.commitErr = errFake
			pg := newFakePostgres(pool)
			if err := v.drive(t, pg, newBuilderCtx(), writeHook{}); err == nil {
				t.Fatal("expected error when Commit fails")
			}
		})
	}
}

func TestFlatVerbs_HooksFireAndCommit(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pg := newFakePostgres(pool)
			var afterBegin, beforeCommit bool
			hook := writeHook{
				AfterBegin: func(_ persistence.RequestContext, _ domain.Entity, _ persistence.TxHandle) error {
					afterBegin = true
					return nil
				},
				BeforeCommit: func(_ persistence.RequestContext, _ domain.Entity, _ domain.ID, _ persistence.TxHandle) error {
					beforeCommit = true
					return nil
				},
			}
			if err := v.drive(t, pg, newBuilderCtx(), hook); err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			if !afterBegin || !beforeCommit {
				t.Errorf("hooks did not both fire: afterBegin=%v beforeCommit=%v", afterBegin, beforeCommit)
			}
			if !pool.tx.committed {
				t.Error("transaction was not committed after hooks")
			}
		})
	}
}

func TestFlatVerbs_AfterBeginHookError_RollsBack(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pg := newFakePostgres(pool)
			hook := writeHook{
				AfterBegin: func(_ persistence.RequestContext, _ domain.Entity, _ persistence.TxHandle) error {
					return errFake
				},
			}
			if err := v.drive(t, pg, newBuilderCtx(), hook); err == nil {
				t.Fatal("expected error from AfterBegin hook")
			}
			if !pool.tx.rolledBack {
				t.Error("expected rollback after AfterBegin hook error")
			}
			if pool.tx.committed {
				t.Error("must not commit after AfterBegin hook error")
			}
		})
	}
}

func TestFlatVerbs_BeforeCommitHookError_RollsBack(t *testing.T) {
	for _, v := range flatVerbs() {
		t.Run(v.name, func(t *testing.T) {
			pool := newFakePool()
			pg := newFakePostgres(pool)
			hook := writeHook{
				BeforeCommit: func(_ persistence.RequestContext, _ domain.Entity, _ domain.ID, _ persistence.TxHandle) error {
					return errFake
				},
			}
			if err := v.drive(t, pg, newBuilderCtx(), hook); err == nil {
				t.Fatal("expected error from BeforeCommit hook")
			}
			if !pool.tx.rolledBack {
				t.Error("expected rollback after BeforeCommit hook error")
			}
			if pool.tx.committed {
				t.Error("must not commit after BeforeCommit hook error")
			}
		})
	}
}

// Archive / Unarchive on a schema without SoftDelete must fail loudly at the
// requireSoftDelete backstop before any TX is opened.
func TestArchiveUnarchive_RequireSoftDelete(t *testing.T) {
	noSD := NewTableSchema[*builderTestEntity]("flat_no_sd").
		PK("ID", "id").
		Field("Name", "name").
		Field("Email", "email")

	pool := newFakePool()
	pg := newFakePostgres(pool)

	if err := pg.Archive(newBuilderCtx(), mustArchivable(t, newFlatEntity()), noSD, writeHook{}); err == nil {
		t.Error("Archive without SoftDelete must error")
	}
	if err := pg.Unarchive(newBuilderCtx(), mustUnarchivable(t, newFlatEntity()), noSD, writeHook{}); err == nil {
		t.Error("Unarchive without SoftDelete must error")
	}
}

// Batch runs every op type through execWithTx in one TX.
func TestBatch_AllVerbsInOneTx(t *testing.T) {
	ins := mustInsertable(t, &builderTestEntity{Name: "i", Email: "i@x.com"})
	upd := mustUpdatable(t, newFlatEntity())
	arch := mustArchivable(t, newFlatEntity())
	una := mustUnarchivable(t, newFlatEntity())
	del := mustDeletable(t, newFlatEntity())

	ops := []domain.ValidEntity{ins, upd, arch, una, del}
	schemas := []*TableSchema{builderTestSchema, builderTestSchema, builderTestSchema, builderTestSchema, builderTestSchema}

	pool := newFakePool()
	pg := newFakePostgres(pool)
	results, err := pg.Batch(newBuilderCtx(), domain.NewBatch(ops), schemas)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(results) != len(ops) {
		t.Errorf("results = %d, want %d", len(results), len(ops))
	}
	if !pool.tx.committed {
		t.Error("batch TX was not committed")
	}
}

func TestBatch_SchemaCountMismatch(t *testing.T) {
	ins := mustInsertable(t, &builderTestEntity{Name: "i"})
	pool := newFakePool()
	pg := newFakePostgres(pool)
	if _, err := pg.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{ins}), nil); err == nil {
		t.Fatal("expected error when schema count != op count")
	}
}

func TestBatch_BeginError(t *testing.T) {
	ins := mustInsertable(t, &builderTestEntity{Name: "i"})
	pool := newFakePool()
	pool.beginErr = errFake
	pg := newFakePostgres(pool)
	if _, err := pg.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{ins}), []*TableSchema{builderTestSchema}); err == nil {
		t.Fatal("expected Begin error")
	}
}

func TestBatch_OpError_RollsBack(t *testing.T) {
	ins := mustInsertable(t, &builderTestEntity{Name: "i"})
	pool := newFakePool()
	pool.tx.queryRowErr = errFake // the INSERT op's RETURNING scan fails
	pg := newFakePostgres(pool)
	if _, err := pg.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{ins}), []*TableSchema{builderTestSchema}); err == nil {
		t.Fatal("expected op error")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback after batch op error")
	}
}

func TestBatch_CommitError(t *testing.T) {
	ins := mustInsertable(t, &builderTestEntity{Name: "i"})
	pool := newFakePool()
	pool.tx.commitErr = errFake
	pg := newFakePostgres(pool)
	if _, err := pg.Batch(newBuilderCtx(), domain.NewBatch([]domain.ValidEntity{ins}), []*TableSchema{builderTestSchema}); err == nil {
		t.Fatal("expected Commit error")
	}
}

// archiveSQL / unarchiveSQL / deleteSQL / buildUpdate are exercised end-to-end
// above; this asserts their literal shape so a column-name regression surfaces.
func TestWriteSQLBuilders_Shape(t *testing.T) {
	if got := archiveSQL("users", "deleted_at", "id"); got != "UPDATE users SET deleted_at = NOW() WHERE id = $1" {
		t.Errorf("archiveSQL = %q", got)
	}
	if got := unarchiveSQL("users", "deleted_at", "id"); got != "UPDATE users SET deleted_at = NULL WHERE id = $1" {
		t.Errorf("unarchiveSQL = %q", got)
	}
	if got := deleteSQL("users", "id"); got != "DELETE FROM users WHERE id = $1" {
		t.Errorf("deleteSQL = %q", got)
	}
	sql, args := buildUpdate("users", "id", "the-id", domain.Fields{"name": "bob"}, []string{"updated_at"})
	if sql != "UPDATE users SET name = $1, updated_at = NOW() WHERE id = $2 RETURNING id" {
		t.Errorf("buildUpdate sql = %q", sql)
	}
	if len(args) != 2 || args[0] != "bob" || args[1] != "the-id" {
		t.Errorf("buildUpdate args = %v", args)
	}
}
