package write

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for the SharedBase (M2) write path: the two-level existence
// matrix on INSERT (new / active-conflict / archived-revive) and the UPDATE
// (role row + shared-base upsert). Driven through the recording fake WriteTx; the
// existence probe (tx.Query) is scripted via recTx.queryFn. The upsert SQL is the
// Dialect's contract (testPGDialect stubs BuildUpsert as "INSERT <table>").

type roleTestEntity struct {
	domain.BaseEntity
	Name      string // shared (lives on the base)
	Document  string // shared + natural key
	Matricula string // role-own
}

func (e *roleTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *roleTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func roleTestSchema() *TableSchema {
	base := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document")
	return NewTableSchema[*roleTestEntity]("aluno").
		ID("id").
		Revision("revision").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(base, "pessoa_id")
}

// rowsNone scripts the active-role probe to find nothing (no role, or only an
// archived remnant — invisible to the probe); rowsFound scripts one ACTIVE row.
func rowsNone() func(string, []any) (Rows, error) {
	return func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }
}
func rowsFound() func(string, []any) (Rows, error) {
	return func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func(dest []any) error { return nil }}, nil
	}
}

// rowsFKMatch scripts the natural-key guard probe (the ANSI
// `SELECT CASE WHEN fk = $1 THEN 1 ELSE 0 END FROM role WHERE pk = $2`
// projection) to report the stored ParentID matching the request-derived base id.
func rowsFKMatch() func(string, []any) (Rows, error) {
	return func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func(dest []any) error {
			if p, ok := dest[0].(*int64); ok {
				*p = 1
			}
			return nil
		}}, nil
	}
}

func TestInsertRoleWithBase_New(t *testing.T) {
	ins, err := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// upsert pessoa + insert aluno + outbox(role, single row) + audit = 4.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 5 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.Contains(tx.execs[0], "pessoa") {
		t.Errorf("stmt[0] must upsert the shared base, got %q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "INSERT INTO aluno") {
		t.Errorf("stmt[1] must insert the role, got %q", tx.execs[1])
	}
}

func TestInsertRoleWithBase_ActiveConflict409(t *testing.T) {
	ins, _ := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	tx := &recTx{queryFn: rowsFound()} // active role exists
	be := newFlatBE(&recBeginner{tx: tx})
	_, err := be.Insert(newBuilderCtx(), ins, roleTestSchema(), firingHook)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("an active existing role must be a conflict NotificationCarrier, got %T (%v)", err, err)
	}
	// Only the base upsert ran; no role write after the 409.
	if len(tx.execs) != 1 {
		t.Errorf("expected only the base upsert before the 409, got %v", tx.execs)
	}
}

// An ARCHIVED role is INVISIBLE to the insert probe (DeletedAt is delete on
// this path like on every other): the probe — which now filters deleted_at IS
// NULL in SQL — finds nothing, so the write proceeds as a plain INSERT and the
// schema's own constraints arbitrate the collision with the physical remnant
// (asserted E2E against real backends; the fake here scripts the probe miss).
// There is no revive UPDATE anywhere in the statement stream anymore —
// reviving a role is the explicit /unarchive verb's job.
func TestInsertRoleWithBase_ArchivedRemnantIsInvisible_InsertProceeds(t *testing.T) {
	ins, _ := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	tx := &recTx{queryFn: rowsNone()} // active-only probe: archived remnant → no row
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !strings.HasPrefix(tx.execs[1], "INSERT INTO aluno") {
		t.Errorf("stmt[1] must be a plain role INSERT (no revive), got %q", tx.execs[1])
	}
	if hasStmt(tx.execs, func(s string) bool {
		return strings.Contains(s, "deleted_at = NULL") && strings.HasPrefix(s, "UPDATE aluno")
	}) {
		t.Errorf("no revive UPDATE may run on the insert path, got %v", tx.execs)
	}
}

// The active-only probe carries the DeletedAt predicate in its SQL — the
// invisibility of archived rows is enforced by the QUERY, not by scanning.
func TestFindActiveRoleByFK_ProbeFiltersArchivedInSQL(t *testing.T) {
	var probed string
	tx := &recTx{queryFn: func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "FROM aluno") {
			probed = sql
		}
		return &fakeRows{remaining: 0}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	ins, _ := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !strings.Contains(probed, "deleted_at IS NULL") {
		t.Errorf("role probe must filter archived rows in SQL, got %q", probed)
	}
}

// roleTestSchemaPurge is roleTestSchema with the purge policy declared
// explicitly — the default is KeepOrphan, so DeleteWhenUnreferenced is always a
// conscious opt-in.
func roleTestSchemaPurge() *TableSchema {
	base := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document").
		OrphanPolicy(DeleteWhenUnreferenced)
	return NewTableSchema[*roleTestEntity]("aluno").
		ID("id").
		Revision("revision").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(base, "pessoa_id")
}

func roleTestDeletable(t *testing.T) domain.Deletable {
	t.Helper()
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	del, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	return del
}

func TestDeleteRoleWithBase_OrphanedPurgesBase(t *testing.T) {
	// No other role references the base → the refcount probe finds nothing.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE role + savepointed purge (SAVEPOINT, DELETE base, RELEASE) +
	// outbox(base DELETED) + audit(base purge) + outbox(role) + audit(role) = 8.
	if len(tx.execs) != 8 {
		t.Fatalf("expected 8 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "DELETE FROM aluno") {
		t.Errorf("stmt[0] must delete the role, got %q", tx.execs[0])
	}
	if tx.execs[1] != "SAVEPOINT "+sharedBasePurgeSavepoint {
		t.Errorf("stmt[1] must open the purge savepoint, got %q", tx.execs[1])
	}
	if !strings.HasPrefix(tx.execs[2], "DELETE FROM pessoa") {
		t.Errorf("stmt[2] must delete the orphaned base, got %q", tx.execs[2])
	}
	if tx.execs[3] != "RELEASE SAVEPOINT "+sharedBasePurgeSavepoint {
		t.Errorf("stmt[3] must release the purge savepoint, got %q", tx.execs[3])
	}
}

func TestDeleteRoleWithBase_StillReferencedKeepsBase(t *testing.T) {
	// Another role still references the base → the probe finds a row.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 1}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE role + base-revision bump + outbox + audit = 4 (base row kept,
	// purge never attempted; the revision bump is the one base statement every
	// role hard-delete emits — the base revision must advance on every
	// identity-touching write).
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements (base kept), got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[1], "UPDATE pessoa SET revision = revision + 1") {
		t.Errorf("stmt[1] must advance the base revision, got %q", tx.execs[1])
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "DELETE FROM pessoa") || strings.HasPrefix(s, "SAVEPOINT") {
			t.Errorf("a still-referenced base must not be touched, got %q", s)
		}
	}
}

func TestDeleteRoleWithBase_DefaultKeepsOrphanBase(t *testing.T) {
	// roleTestSchema declares NO OrphanPolicy → KeepOrphan (the safe default):
	// the base row survives the last role's hard-delete untouched (no probes, no
	// savepoint), and without DeletedAt on the base no archive runs either.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE role + base-revision bump + outbox + audit = 4. The default
	// policy keeps the base ROW (no delete, no archive) — but the base
	// revision still advances: a role hard-delete changes the identity's closure
	// (remnant pick, SharedBaseView segments), so the revision bump is the one
	// permitted base statement.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements (orphan base kept by default), got %d: %v", len(tx.execs), tx.execs)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE pessoa SET revision = revision + 1") {
			continue // the base revision — the only base touch this verb may emit
		}
		if strings.Contains(s, "pessoa") {
			t.Errorf("the default policy must never touch the base beyond the base revision, got %q", s)
		}
	}
}

func TestDeleteRoleWithBase_FKVetoKeepsBase(t *testing.T) {
	// The purge DELETE hits a foreign-key violation — a referencing table the
	// registry does not know about (e.g. another system sharing the database).
	// The database veto rolls back to the savepoint; the base survives and the
	// role delete still commits.
	tx := &recTx{
		queryFn:    func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil },
		execErrSub: "DELETE FROM pessoa",
		execErr:    &pgconn.PgError{Code: "23503", ConstraintName: "fk_outro_sistema_pessoa"},
	}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); err != nil {
		t.Fatalf("a vetoed purge must not fail the role delete: %v", err)
	}
	// DELETE role + SAVEPOINT + DELETE base (vetoed) + ROLLBACK TO SAVEPOINT +
	// base-revision bump + outbox(role) + audit(role) = 7; no base
	// outbox/audit, no RELEASE. The vetoed (surviving) base still gets its
	// revision bumped — the role hard-delete changed the identity's closure.
	if len(tx.execs) != 7 {
		t.Fatalf("expected 7 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if tx.execs[3] != "ROLLBACK TO SAVEPOINT "+sharedBasePurgeSavepoint {
		t.Errorf("stmt[3] must roll back to the purge savepoint, got %q", tx.execs[3])
	}
	if !tx.committed {
		t.Error("the surrounding role delete must commit after the veto")
	}
}

func TestDeleteRoleWithBase_VetoThenArchivesBase(t *testing.T) {
	// Purge policy + a archivable base: when the database vetoes the purge,
	// the standing lifecycle convergence still archives the orphaned identity.
	base := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document").
		DeletedAt("deleted_at").
		OrphanPolicy(DeleteWhenUnreferenced)
	schema := NewTableSchema[*roleTestEntity]("aluno").
		ID("id").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(base, "pessoa_id")
	tx := &recTx{
		queryFn:    func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil },
		execErrSub: "DELETE FROM pessoa",
		execErr:    &pgconn.PgError{Code: "23503", ConstraintName: "fk_outro_sistema_pessoa"},
	}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), schema, firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE role + SAVEPOINT + DELETE base (vetoed) + ROLLBACK + archive base +
	// base-revision bump + outbox(role) + audit(role) = 8. (The archive
	// bumped the revision in its own statement; the standalone bump after it
	// is the unconditional base-revision advance every role hard-delete
	// emits — double-bumping is harmless, the base revision is monotone, not dense.)
	if len(tx.execs) != 8 {
		t.Fatalf("expected 8 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[4], "UPDATE pessoa SET deleted_at = $1") {
		t.Errorf("the vetoed orphan must be archived (UPDATE pessoa SET deleted_at = $1), got %q", tx.execs[4])
	}
}

func TestDeleteRoleWithBase_PurgeNonFKErrorFails(t *testing.T) {
	// A NON-foreign-key error inside the purge (connection loss, syntax, …) is
	// not a veto — it propagates and rolls the whole delete back.
	tx := &recTx{
		queryFn:    func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil },
		execErrSub: "DELETE FROM pessoa", // default injected error is errRecExec (non-ParentID)
	}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); !errors.Is(err, errRecExec) {
		t.Fatalf("a non-ParentID purge error must propagate, got %v", err)
	}
	if tx.committed {
		t.Error("the delete must not commit on a non-ParentID purge error")
	}
}

func TestDeleteRoleWithBase_SavepointErrorFails(t *testing.T) {
	tx := &recTx{
		queryFn:    func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil },
		execErrSub: "SAVEPOINT " + sharedBasePurgeSavepoint,
	}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); !errors.Is(err, errRecExec) {
		t.Fatalf("a savepoint failure must propagate, got %v", err)
	}
}

func TestDeleteRoleWithBase_ProbeErrorFails(t *testing.T) {
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return nil, errRecExec }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); !errors.Is(err, errRecExec) {
		t.Fatalf("a refcount-probe failure must propagate, got %v", err)
	}
}

func TestDeleteRoleWithBase_ReleaseErrorFails(t *testing.T) {
	tx := &recTx{
		queryFn:    func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil },
		execErrSub: "RELEASE SAVEPOINT",
	}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); !errors.Is(err, errRecExec) {
		t.Fatalf("a RELEASE failure must propagate, got %v", err)
	}
}

func TestDeleteRoleWithBase_RollbackToSavepointErrorFails(t *testing.T) {
	// The purge is vetoed AND the rollback-to-savepoint itself fails (dead
	// connection): the rollback error propagates — never a silent half-state.
	tx := &recTx{
		queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil },
		execErrs: map[string]error{
			"DELETE FROM pessoa": &pgconn.PgError{Code: "23503", ConstraintName: "fk_x"},
			"ROLLBACK TO":        errRecExec,
		},
	}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); !errors.Is(err, errRecExec) {
		t.Fatalf("a ROLLBACK TO SAVEPOINT failure must propagate, got %v", err)
	}
}

func TestDeleteRoleWithBase_PurgeOutboxErrorFails(t *testing.T) {
	// The purge succeeded but its outbox row cannot be written: the delete must
	// fail (rolling everything back) rather than commit an invisible purge.
	tx := &recTx{
		queryFn:    func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil },
		execErrSub: "INSERT INTO outbox",
	}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchemaPurge(), firingHook); !errors.Is(err, errRecExec) {
		t.Fatalf("a purge-outbox failure must propagate, got %v", err)
	}
	if tx.committed {
		t.Error("the delete must not commit when the purge outbox row failed")
	}
}

func TestDeleteRoleWithBase_EmptyNaturalKeyErrors(t *testing.T) {
	// A shared-base role whose natural key resolved empty cannot derive the
	// identity — converging on UUIDv5("") could touch the WRONG base row, so the
	// delete fails loudly instead (same guard as the soft-write convergence).
	e := &roleTestEntity{Name: "Ana", Document: "", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	del, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	err = be.Delete(newBuilderCtx(), del, roleTestSchemaPurge(), firingHook)
	if err == nil || !strings.Contains(err.Error(), "natural key") {
		t.Fatalf("an empty natural key must fail the delete, got %v", err)
	}
}

func TestDeleteRoleWithBase_KeepOrphanArchivesSoftDeletableBase(t *testing.T) {
	// Default policy + a archivable base: the last role's hard-delete leaves
	// the identity dormant (archived), never destroyed — and revivable by a
	// future insert of the same natural key.
	base := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document").
		DeletedAt("deleted_at")
	schema := NewTableSchema[*roleTestEntity]("aluno").
		ID("id").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(base, "pessoa_id")
	// No active role remains → the anyActiveRole probe finds nothing.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), schema, firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE role + archive base + base-revision bump + outbox + audit = 5;
	// never a DELETE FROM pessoa. (The archive already bumped the revision in
	// its own statement; the standalone bump is the unconditional revision
	// advance of every role hard-delete — monotone, so double is harmless.)
	if len(tx.execs) != 5 {
		t.Fatalf("expected 5 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[1], "UPDATE pessoa SET deleted_at = $1") {
		t.Errorf("the orphaned archivable base must archive, got %q", tx.execs[1])
	}
	if !strings.HasPrefix(tx.execs[2], "UPDATE pessoa SET revision = revision + 1") {
		t.Errorf("stmt[2] must advance the base revision, got %q", tx.execs[2])
	}
}

// An AGGREGATE role (children) that also declares a SharedBase: the write must
// upsert the base, insert the role row, AND insert the children — the case that
// previously fell into insertAggregate and silently dropped the base.
type aggRoleChild struct {
	domain.Managed
	Label string
}

func (c aggRoleChild) BuildRules(string, domain.Service, *domain.Rules) {}

type aggRoleEntity struct {
	domain.AggregateRoot
	Name      string // shared (base)
	Document  string // shared + natural key
	Matricula string // role-own
}

func (e *aggRoleEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *aggRoleEntity) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *aggRoleEntity) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *aggRoleEntity) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{aggRoleChild{}}
}

func aggRoleSchema() *TableSchema {
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").Field("Document", "document").NaturalID("document")
	return NewTableSchema[*aggRoleEntity]("aluno").
		ID("id").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		Child(NewTableSchema[aggRoleChild]("aluno_disciplines").ID("id").ParentID("aluno_id").Field("Label", "label")).
		SharedBase(base, "pessoa_id")
}

func TestInsertWithBase_AggregateRole(t *testing.T) {
	e := &aggRoleEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	domain.AddAggregateChild(e, aggRoleChild{Label: "Math"})
	ins, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, aggRoleSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// upsert pessoa + insert aluno + insert child + outbox(role, single row) + audit = 5.
	if len(tx.execs) != 5 {
		t.Fatalf("expected 6 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.Contains(tx.execs[0], "pessoa") {
		t.Errorf("the aggregate role's base MUST be upserted (was silently dropped before), got stmt[0]=%q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "INSERT INTO aluno ") && !strings.HasPrefix(tx.execs[1], "INSERT INTO aluno(") {
		t.Errorf("stmt[1] must insert the role, got %q", tx.execs[1])
	}
	if !strings.HasPrefix(tx.execs[2], "INSERT INTO aluno_disciplines") {
		t.Errorf("stmt[2] must insert the aggregate child, got %q", tx.execs[2])
	}
}

func TestDeleteRoleWithBase_EngineRegistryUnionsRoles(t *testing.T) {
	// aluno and professor each declared their OWN NewSharedBaseSchema("pessoa").Revision("revision") — an
	// identical shape, but two instances, so neither instance registry sees the
	// other role. The consumer never needs a singleton: WithSchema registers
	// both on the ENGINE, and deleting the last aluno must probe professor too.
	alunoSchema := roleTestSchemaPurge()
	profBase := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document").
		OrphanPolicy(DeleteWhenUnreferenced)
	profSchema := NewTableSchema[*roleTestEntity]("professor").
		ID("id").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(profBase, "pessoa_id")

	var probed []string
	tx := &recTx{queryFn: func(sql string, _ []any) (Rows, error) {
		probed = append(probed, sql)
		if strings.Contains(sql, "FROM professor") {
			return &fakeRows{remaining: 1}, nil // a professor row still references the identity
		}
		return &fakeRows{remaining: 0}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	be.RegisterSharedBaseRole(alunoSchema)
	be.RegisterSharedBaseRole(profSchema)

	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), alunoSchema, firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	crossProbed := false
	for _, q := range probed {
		if strings.Contains(q, "FROM professor") {
			crossProbed = true
		}
	}
	if !crossProbed {
		t.Fatalf("the engine registry must extend the refcount probe to professor, probes: %v", probed)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "DELETE FROM pessoa") {
			t.Errorf("a base referenced by a professor (via the engine registry) must not be purged, got %q", s)
		}
	}
}

func TestRegisterSharedBaseRole_DivergentDeclarationPanics(t *testing.T) {
	be := &BaseEngine{}
	be.RegisterSharedBaseRole(roleTestSchemaPurge())

	divergent := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("name"). // same table, DIFFERENT natural key — a corrupting divergence
		OrphanPolicy(DeleteWhenUnreferenced)
	role := NewTableSchema[*roleTestEntity]("professor").
		ID("id").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(divergent, "pessoa_id")

	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "diverge") {
			t.Fatalf("a divergent shared-base re-declaration must panic at registration, got %v", r)
		}
	}()
	be.RegisterSharedBaseRole(role)
}

func TestRegisterSharedBaseRole_IdenticalRedeclarationAccepted(t *testing.T) {
	// The same base re-declared identically (the copy-paste-per-role-file case)
	// registers cleanly — no singleton demanded of the consumer.
	be := &BaseEngine{}
	be.RegisterSharedBaseRole(roleTestSchemaPurge())
	be.RegisterSharedBaseRole(roleTestSchemaPurge()) // same shape, new instances
	// And a non-shared-base schema is a silent no-op.
	be.RegisterSharedBaseRole(builderTestSchema)
}

func TestUpdateRoleWithBase(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M2"}
	e.SetID(domain.NewID(uuid.NewString()))
	upd, err := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1, queryFn: rowsFKMatch()} // role UPDATE matches one row; guard probe: ParentID matches
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// update aluno + upsert pessoa (with revision bump) + outbox(role, single row) + audit = 4.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 5 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "UPDATE aluno") {
		t.Errorf("stmt[0] must update the role, got %q", tx.execs[0])
	}
	if !strings.Contains(tx.execs[1], "pessoa") {
		t.Errorf("stmt[1] must upsert the shared base, got %q", tx.execs[1])
	}
}

// --- unarchive active-sibling veto ------------------------------------------
//
// The /unarchive verb carries the same invariant the INSERT probe enforces: at
// most one ACTIVE role row per identity per role table. Under the separate-ParentID
// model an identity may hold archived remnants NEXT TO a newer active row (the
// dev's active-only uniqueness contract), so reviving a remnant must be the
// same 409 a POST would raise — probed with the row being unarchived excluded.

func TestUnarchiveRoleWithBase_ActiveSiblingConflict409(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	var probeSQL string
	tx := &recTx{count: 1, queryFn: func(sql string, args []any) (Rows, error) {
		probeSQL = sql
		return &fakeRows{remaining: 1, scan: func(dest []any) error { return nil }}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	err = be.Unarchive(newBuilderCtx(), un, roleTestSchema(), firingHook)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("unarchiving next to an active sibling must be a conflict NotificationCarrier, got %T (%v)", err, err)
	}
	// The probe targets the SAME role table, excludes the row being unarchived,
	// and filters active rows only.
	for _, want := range []string{"FROM aluno", "pessoa_id", "<>", "deleted_at IS NULL"} {
		if !strings.Contains(probeSQL, want) {
			t.Errorf("veto probe SQL must contain %q, got %q", want, probeSQL)
		}
	}
	if tx.committed {
		t.Errorf("the vetoed unarchive must not commit")
	}
	if !tx.rolledBack {
		t.Errorf("the vetoed unarchive must roll back")
	}
}

func TestUnarchiveRoleWithBase_NoActiveSiblingProceeds(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	tx := &recTx{count: 1, queryFn: rowsNone()} // veto probe: no active sibling
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), un, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if !strings.HasPrefix(tx.execs[0], "UPDATE aluno") {
		t.Errorf("stmt[0] must be the role unarchive UPDATE, got %q", tx.execs[0])
	}
	if !tx.committed {
		t.Errorf("the clean unarchive must commit")
	}
}

// Under shared-ID the primary key itself caps the table at one row per identity —
// no sibling can exist, so the veto probe is skipped entirely.
func TestUnarchiveRoleWithBase_SharedPKSkipsVeto(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(deterministicBaseID("D1")))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	var probes []string
	tx := &recTx{count: 1, queryFn: func(sql string, args []any) (Rows, error) {
		probes = append(probes, sql)
		return &fakeRows{remaining: 0}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), un, roleTestSchemaSharedPK(), firingHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	// The payload reads the base revision (SELECT revision FROM pessoa) —
	// that is the only query allowed; the sibling veto probe (FROM aluno) must
	// NOT run under the shared-ID model (the ID caps the table at one row).
	for _, q := range probes {
		if strings.Contains(q, "FROM aluno") {
			t.Errorf("shared-ID unarchive must not probe for siblings (ID caps at one row), got %q", q)
		}
	}
}

func TestUnarchiveRoleWithBase_VetoProbeErrorPropagates(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	un, _ := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	probeErr := fmt.Errorf("probe boom")
	tx := &recTx{count: 1, queryFn: func(string, []any) (Rows, error) { return nil, probeErr }}
	be := newFlatBE(&recBeginner{tx: tx})
	err := be.Unarchive(newBuilderCtx(), un, roleTestSchema(), firingHook)
	if err == nil || !strings.Contains(err.Error(), "probe boom") {
		t.Fatalf("the veto probe error must propagate, got %v", err)
	}
}

func TestUnarchiveRoleWithBase_EmptyNaturalKeyErrors(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "", Matricula: "M1"} // natural key empty
	e.SetID(domain.NewID(uuid.NewString()))
	un, _ := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	tx := &recTx{count: 1, queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	err := be.Unarchive(newBuilderCtx(), un, roleTestSchema(), firingHook)
	if err == nil || !strings.Contains(err.Error(), "natural key") {
		t.Fatalf("an empty natural key must fail the veto loudly, got %v", err)
	}
}

// White-box: the defensive no-ops of the veto — a role schema without
// DeletedAt (unreachable through the unarchive verb, which requires it) and a
// convergence call with a neutral event type.
func TestVetoUnarchive_DefensiveNoOps(t *testing.T) {
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").Field("Document", "document").NaturalID("document")
	noSD := NewTableSchema[*roleTestEntity]("aluno").ID("id").Revision("revision").Field("Matricula", "matricula").SharedBase(base, "pessoa_id")
	tx := &recTx{queryFn: func(string, []any) (Rows, error) {
		t.Fatal("no probe may run for a role without DeletedAt")
		return nil, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	src := &roleTestEntity{Name: "Ana", Document: "D1"}
	if err := be.vetoUnarchiveWithActiveSibling(newBuilderCtx(), tx, testPGDialect{}, noSD, src, "some-id", "Aluno"); err != nil {
		t.Fatalf("no-DeletedAt veto must no-op, got %v", err)
	}
	if err := be.convergeBaseAfterSoftWrite(newBuilderCtx(), tx, testPGDialect{}, roleTestSchema(), src, "OTHER", writeNow()); err != nil {
		t.Fatalf("a neutral event type must no-op, got %v", err)
	}
}

// --- natural-key immutability guard ------------------------------------------
//
// The natural key derives the deterministic base id; every SharedBase
// derivation assumes it never changes after insert. The UPDATE path enforces
// it: shared-ID by arithmetic (the role id IS UUIDv5(naturalKey)), separate-ParentID
// by one ID-indexed probe comparing the stored ParentID with the request-derived id.

func roleTestUpdatable(t *testing.T, doc, id string) domain.Updatable {
	t.Helper()
	e := &roleTestEntity{Name: "Ana", Document: doc, Matricula: "M2"}
	e.SetID(domain.NewID(id))
	upd, err := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return upd
}

func TestUpdateRoleWithBase_NaturalKeyMutationRejected(t *testing.T) {
	var probeSQL string
	tx := &recTx{count: 1, queryFn: func(sql string, args []any) (Rows, error) {
		probeSQL = sql
		return &fakeRows{remaining: 1, scan: func(dest []any) error {
			if p, ok := dest[0].(*bool); ok {
				*p = false // stored ParentID does NOT match the request-derived base id
			}
			return nil
		}}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	_, err := be.Update(newBuilderCtx(), roleTestUpdatable(t, "D-CHANGED", uuid.NewString()), roleTestSchema(), firingHook)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("a mutated natural key must be a NotificationCarrier, got %T (%v)", err, err)
	}
	for _, want := range []string{"pessoa_id", "FROM aluno"} {
		if !strings.Contains(probeSQL, want) {
			t.Errorf("guard probe SQL must contain %q, got %q", want, probeSQL)
		}
	}
	if hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE aluno") }) {
		t.Errorf("no role UPDATE may run after the guard rejects, got %v", tx.execs)
	}
	if tx.committed {
		t.Errorf("the rejected update must not commit")
	}
}

func TestUpdateRoleWithBase_SharedPK_NaturalKeyMutationRejected(t *testing.T) {
	// The row id is NOT UUIDv5(D-CHANGED) — the request smuggled another key.
	tx := &recTx{count: 1, queryFn: func(string, []any) (Rows, error) {
		t.Fatal("the shared-ID guard is arithmetic — no probe may run")
		return nil, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	_, err := be.Update(newBuilderCtx(), roleTestUpdatable(t, "D-CHANGED", uuid.NewString()), roleTestSchemaSharedPK(), firingHook)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("a mutated natural key must be a NotificationCarrier, got %T (%v)", err, err)
	}
}

func TestUpdateRoleWithBase_SharedPK_MatchingNaturalKeyProceeds(t *testing.T) {
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	// id == UUIDv5(D1): the canonical shared-ID state.
	if _, err := be.Update(newBuilderCtx(), roleTestUpdatable(t, "D1", deterministicBaseID("D1")), roleTestSchemaSharedPK(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.HasPrefix(tx.execs[0], "UPDATE aluno") {
		t.Errorf("stmt[0] must update the role, got %q", tx.execs[0])
	}
}

func TestUpdateRoleWithBase_GuardRowMissingSkips(t *testing.T) {
	// No role row → the guard steps aside; the role UPDATE right after owns
	// the not-found semantics (count=1 here keeps the write green).
	tx := &recTx{count: 1, queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), roleTestUpdatable(t, "D1", uuid.NewString()), roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestUpdateRoleWithBase_GuardProbeErrorPropagates(t *testing.T) {
	probeErr := fmt.Errorf("guard boom")
	tx := &recTx{count: 1, queryFn: func(string, []any) (Rows, error) { return nil, probeErr }}
	be := newFlatBE(&recBeginner{tx: tx})
	_, err := be.Update(newBuilderCtx(), roleTestUpdatable(t, "D1", uuid.NewString()), roleTestSchema(), firingHook)
	if err == nil || !strings.Contains(err.Error(), "guard boom") {
		t.Fatalf("the guard probe error must propagate, got %v", err)
	}
}

func TestUpdateRoleWithBase_GuardScanErrorPropagates(t *testing.T) {
	scanErr := fmt.Errorf("scan boom")
	tx := &recTx{count: 1, queryFn: func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func([]any) error { return scanErr }}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	_, err := be.Update(newBuilderCtx(), roleTestUpdatable(t, "D1", uuid.NewString()), roleTestSchema(), firingHook)
	if err == nil || !strings.Contains(err.Error(), "scan boom") {
		t.Fatalf("the guard scan error must propagate, got %v", err)
	}
}
