package write

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for SharedBase native children (base-children) on the write
// path: a base-child is routed to the base's deterministic id (ParentID column = the
// base ParentID, not the role ParentID), Removed archives (or hard-deletes when the base has
// no DeletedAt), the orphan refcount removes base-children before the base, and
// an empty natural key is rejected. Driven through the recording fake WriteTx.

type bcAddr struct {
	domain.Managed
	Street string
}

func (c bcAddr) BuildRules(string, domain.Service, *domain.Rules) {}

type bcRole struct {
	domain.AggregateRoot
	Name      string // shared (base)
	Document  string // shared + natural key
	Matricula string // role-own
}

func (e *bcRole) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *bcRole) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *bcRole) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *bcRole) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{bcAddr{}}
}

// bcRoleSchema declares endereco as a NATIVE CHILD OF THE BASE (pessoa), shared by
// every role. deletedAt toggles the archive column on base + base-child alike.
// The purge policy is declared explicitly (the default is KeepOrphan) so the
// orphan-delete test below exercises the purge branch.
func bcRoleSchema(deletedAt bool) *TableSchema {
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").Field("Document", "document").
		NaturalID("document").OrphanPolicy(DeleteWhenUnreferenced)
	addr := NewTableSchema[bcAddr]("endereco").ID("id").ParentID("pessoa_id").Field("Street", "street")
	if deletedAt {
		base = base.DeletedAt("deleted_at")
		addr = addr.DeletedAt("deleted_at")
	}
	base = base.Child(addr)
	role := NewTableSchema[*bcRole]("aluno").ID("id").Revision("revision").Field("Matricula", "matricula").DeletedAt("deleted_at")
	return role.SharedBase(base, "pessoa_id")
}

func TestBaseChild_InsertRoutesToBaseFK(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	domain.AddAggregateChild(e, bcAddr{Street: "Main St"})
	ins, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// upsert pessoa + insert aluno + insert endereco + outbox(role, single row) + audit = 5.
	if len(tx.execs) != 5 {
		t.Fatalf("expected 6 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	var found string
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "INSERT INTO endereco") {
			found = s
		}
	}
	if found == "" {
		t.Fatalf("the base-child INSERT into endereco is missing: %v", tx.execs)
	}
	if !strings.Contains(found, "pessoa_id") {
		t.Errorf("a base-child must take the BASE ParentID (pessoa_id), not the role ParentID; got %q", found)
	}
	if strings.Contains(found, "aluno_id") {
		t.Errorf("a base-child must NOT take the role ParentID (aluno_id); got %q", found)
	}
}

func TestBaseChild_RemovedArchivesWhenDeletedAt(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	e.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(bcAddr{Street: "Old"}, domain.NewID("addr-1"))})
	upd, err := domain.GetUpdatable(e, func(r *bcRole) error {
		domain.RemoveAggregateChild(r, domain.WithID(bcAddr{Street: "Old"}, domain.NewID("addr-1")))
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1, queryFn: rowsNone()} // base not archived → reactivate no-op
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool {
		return strings.HasPrefix(s, "UPDATE endereco SET deleted_at = $1")
	}) {
		t.Errorf("a Removed base-child WITH DeletedAt must archive (UPDATE endereco SET deleted_at), got %v", tx.execs)
	}
}

func TestBaseChild_RemovedHardDeletesWhenNoDeletedAt(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	e.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(bcAddr{Street: "Old"}, domain.NewID("addr-1"))})
	upd, err := domain.GetUpdatable(e, func(r *bcRole) error {
		domain.RemoveAggregateChild(r, domain.WithID(bcAddr{Street: "Old"}, domain.NewID("addr-1")))
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1, queryFn: rowsFKMatch()} // natural-key guard: stored ParentID matches
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, bcRoleSchema(false), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "DELETE FROM endereco") }) {
		t.Errorf("a Removed base-child WITHOUT DeletedAt must hard-delete (DELETE FROM endereco), got %v", tx.execs)
	}
}

func TestBaseChild_OrphanDeletesBaseChildrenThenBase(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	del, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }} // orphaned
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), del, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE aluno + savepointed purge (SAVEPOINT, DELETE endereco, DELETE
	// pessoa, RELEASE) + outbox/audit for the base AND the role = 9.
	idxAddr, idxBase := indexOfPrefix(tx.execs, "DELETE FROM endereco"), indexOfPrefix(tx.execs, "DELETE FROM pessoa")
	if idxAddr < 0 || idxBase < 0 {
		t.Fatalf("orphan delete must remove base-children AND the base, got %v", tx.execs)
	}
	if idxAddr > idxBase {
		t.Errorf("base-children must be deleted BEFORE the base (ParentID integrity), got endereco@%d after pessoa@%d", idxAddr, idxBase)
	}
}

func TestInsertWithBase_EmptyNaturalKeyErrors(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "", Matricula: "M1"} // empty natural key
	ins, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, bcRoleSchema(true), firingHook); err == nil {
		t.Fatal("an empty natural key must be rejected (it collapses every record into one base id)")
	}
}

// --- unified lifecycle convergence (convergeBase) ----------------------------

// baseArchiveStamp is the instant the shared base (and the native children its
// cascade stamped) went down with — what the reactivation reads back and binds.
var baseArchiveStamp = time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

func TestConvergeBase_ArchiveLastActiveRoleArchivesBase(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	arch, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	tx := &recTx{count: 1, queryFn: rowsNone()} // no role left active
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), arch, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE pessoa SET deleted_at = $1") }) {
		t.Errorf("archiving the last active role must archive the base (UPDATE pessoa SET deleted_at = $1), got %v", tx.execs)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE endereco SET deleted_at = $1") }) {
		t.Errorf("the base archive must cascade to the base-children (UPDATE endereco SET deleted_at = $1), got %v", tx.execs)
	}
	// ONE writeNow() for the whole operation: the role row, the shared identity
	// it drove down and that identity's native children all carry the very same
	// instant — which is what lets the reactivation find them again.
	stamps := map[string]time.Time{}
	for i, sql := range tx.execs {
		for _, table := range []string{"aluno", "pessoa", "endereco"} {
			if strings.HasPrefix(sql, "UPDATE "+table+" SET deleted_at = $1") {
				stamps[table], _ = tx.execArgs[i][0].(time.Time)
			}
		}
	}
	if len(stamps) != 3 {
		t.Fatalf("expected a stamp on the role, the base and the base-children, got %v (%v)", stamps, tx.execs)
	}
	for table, got := range stamps {
		if got.IsZero() || !got.Equal(stamps["aluno"]) {
			t.Errorf("%s was stamped with %v, want the role operation's own instant %v", table, got, stamps["aluno"])
		}
	}
}

func TestConvergeBase_ArchiveWithAnotherActiveRoleKeepsBase(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	arch, _ := domain.GetArchivable(e, nil, "GetArchivable")
	tx := &recTx{count: 1, queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 1}, nil }} // another active role
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), arch, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE pessoa SET deleted_at = $1") {
			t.Errorf("the base must stay active while another role is active, got %q", s)
		}
	}
}

func TestConvergeBase_UnarchiveReactivatesBase(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	// The unarchive path probes three times: the active-sibling veto (SELECT 1
	// FROM aluno — no other active row, so the revive proceeds), the role's own
	// archive stamp, then the base's (FROM pessoa — non-zero, so the base is
	// archived and reactivates, carrying its native children with it).
	archived := rowsArchivedAt(baseArchiveStamp)
	tx := &recTx{count: 1, queryFn: func(sql string, args []any) (Rows, error) {
		if strings.HasPrefix(sql, "SELECT deleted_at FROM") {
			return archived(sql, args)
		}
		return &fakeRows{remaining: 0}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), un, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE pessoa SET deleted_at = NULL") }) {
		t.Errorf("unarchiving a role must reactivate the archived base (UPDATE pessoa SET deleted_at = NULL), got %v", tx.execs)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE endereco SET deleted_at = NULL") }) {
		t.Errorf("base reactivation must cascade to the base-children (UPDATE endereco SET deleted_at = NULL), got %v", tx.execs)
	}
	// And it reaches exactly the base-children the BASE'S archive stamped: the
	// statement reads that instant off the base row itself, not "every archived
	// row under the base", so an endereco archived on its own stays where it is.
	// Reading it means the children must move BEFORE the base row is cleared.
	childAt, baseAt := -1, -1
	for i, sql := range tx.execs {
		switch {
		case strings.HasPrefix(sql, "UPDATE endereco SET deleted_at = NULL"):
			childAt = i
			if !strings.Contains(sql, "AND deleted_at = (SELECT deleted_at FROM pessoa WHERE id = $2)") {
				t.Errorf("the base-children restore must read the base's own stamp, got %q", sql)
			}
			if args := tx.execArgs[i]; len(args) != 2 {
				t.Errorf("cascade args = %v, want [baseID baseID]", args)
			}
		case strings.HasPrefix(sql, "UPDATE pessoa SET deleted_at = NULL"):
			baseAt = i
		}
	}
	if childAt < 0 || baseAt < 0 {
		t.Fatalf("expected both the base-children cascade and the base UPDATE: %v", tx.execs)
	}
	if childAt > baseAt {
		t.Errorf("the base-children cascade reads the base's DeletedAt, so it must run BEFORE the UPDATE that clears it: %v", tx.execs)
	}
}

// A base that is NOT archived has no stamp to undo: the reactivation must stop
// at the probe, touching neither the base row nor its native children.
func TestConvergeBase_UnarchiveLeavesAnActiveBaseAlone(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	tx := &recTx{count: 1, queryFn: rowsNone()} // no sibling, and no stamp on the base
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Unarchive(newBuilderCtx(), un, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE pessoa SET deleted_at = NULL") }) {
		t.Errorf("an active base must not be re-activated, got %v", tx.execs)
	}
	if hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE endereco") }) {
		t.Errorf("no base transition → no base-children cascade, got %v", tx.execs)
	}
}

// --- §4.5 upsert insert: forgot-guard + Constructor not re-inserted ----------

// baseExistsQuery scripts: the base existence/archived probes (FROM pessoa) find a
// row; the role probe (FROM aluno) finds none.
func baseExistsQuery() func(string, []any) (Rows, error) {
	return func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "FROM pessoa") {
			return &fakeRows{remaining: 1}, nil // base exists (and not archived: no scan → nil)
		}
		return &fakeRows{remaining: 0}, nil // no role yet
	}
}

func TestSharedBaseInsert_ForgotGuardRefusesBlindInsert(t *testing.T) {
	// Identity exists, but actionName is the plain insert (a blind manual insert).
	ins, _ := domain.GetInsertable(&bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetInsertable")
	tx := &recTx{queryFn: baseExistsQuery()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, bcRoleSchema(true), firingHook); err == nil {
		t.Fatal("a blind insert against an existing shared identity must be refused (forgot-guard)")
	}
}

func TestSharedBaseInsert_UpsertActionPassesGuard(t *testing.T) {
	// Same existing identity, but the upsert actionName → guard passes, role inserts.
	ins, _ := domain.GetInsertable(&bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	tx := &recTx{queryFn: baseExistsQuery()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("the upsert insert (GetUpsertable) against an existing identity must pass: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "INSERT INTO aluno") }) {
		t.Errorf("the role row must be inserted, got %v", tx.execs)
	}
}

func TestSharedBaseInsert_ConstructorBaseChildNotReinserted(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(bcAddr{Street: "Existing"}, domain.NewID("addr-1"))}) // loaded
	domain.AddAggregateChild(e, bcAddr{Street: "New"})                                                                       // request-added
	ins, _ := domain.GetInsertable(e, nil, "GetUpsertable")
	tx := &recTx{queryFn: baseExistsQuery()}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	n := 0
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "INSERT INTO endereco") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("only the new (Added) base-child must be inserted, not the loaded Constructor; got %d INSERTs into endereco: %v", n, tx.execs)
	}
}

func hasStmt(execs []string, pred func(string) bool) bool {
	for _, s := range execs {
		if pred(s) {
			return true
		}
	}
	return false
}

func indexOfPrefix(execs []string, prefix string) int {
	for i, s := range execs {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}
