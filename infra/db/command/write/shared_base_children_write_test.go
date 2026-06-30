package write

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for SharedBase native children (base-children) on the write
// path: a base-child is routed to the base's deterministic id (FK column = the
// base FK, not the role FK), Removed archives (or hard-deletes when the base has
// no soft-delete), the orphan refcount removes base-children before the base, and
// an empty natural key is rejected. Driven through the recording fake WriteTx.

type bcAddr struct {
	ID     string
	Street string
}

func (c bcAddr) GetID() string                                    { return c.ID }
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
func (e *bcRole) GetAggregateRoot() *domain.AggregateRoot { return &e.AggregateRoot }
func (e *bcRole) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{bcAddr{}}
}

// bcRoleSchema declares endereco as a NATIVE CHILD OF THE BASE (pessoa), shared by
// every role. softDelete toggles the all-or-nothing lifecycle on base + base-child.
func bcRoleSchema(softDelete bool) *TableSchema {
	base := NewSharedBase("pessoa").PK("id").Field("Name", "name").Field("Document", "document").NaturalKey("document")
	addr := NewTableSchema[bcAddr]("endereco").PK("id").FK("pessoa_id").Field("Street", "street")
	if softDelete {
		base = base.SoftDelete("deleted_at")
		addr = addr.SoftDelete("deleted_at")
	}
	base = base.Child(addr)
	role := NewTableSchema[*bcRole]("aluno").PK("id").Field("Matricula", "matricula").SoftDelete("deleted_at")
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
	// upsert pessoa + insert aluno + insert endereco + outbox(role) + outbox(base) + audit = 6.
	if len(tx.execs) != 6 {
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
		t.Errorf("a base-child must take the BASE FK (pessoa_id), not the role FK; got %q", found)
	}
	if strings.Contains(found, "aluno_id") {
		t.Errorf("a base-child must NOT take the role FK (aluno_id); got %q", found)
	}
}

func TestBaseChild_RemovedArchivesWhenSoftDelete(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	e.AggregateConstructor([]domain.AggregateValueObject{bcAddr{ID: "addr-1", Street: "Old"}})
	upd, err := domain.GetUpdatable(e, func(r *bcRole) error {
		domain.RemoveAggregateChild(r, bcAddr{ID: "addr-1", Street: "Old"})
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
		return strings.HasPrefix(s, "UPDATE endereco SET deleted_at = NOW()")
	}) {
		t.Errorf("a Removed base-child WITH soft-delete must archive (UPDATE endereco SET deleted_at), got %v", tx.execs)
	}
}

func TestBaseChild_RemovedHardDeletesWhenNoSoftDelete(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	e.AggregateConstructor([]domain.AggregateValueObject{bcAddr{ID: "addr-1", Street: "Old"}})
	upd, err := domain.GetUpdatable(e, func(r *bcRole) error {
		domain.RemoveAggregateChild(r, bcAddr{ID: "addr-1", Street: "Old"})
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, bcRoleSchema(false), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "DELETE FROM endereco") }) {
		t.Errorf("a Removed base-child WITHOUT soft-delete must hard-delete (DELETE FROM endereco), got %v", tx.execs)
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
	// DELETE aluno + DELETE endereco + DELETE pessoa + outbox + audit = 5.
	idxAddr, idxBase := indexOfPrefix(tx.execs, "DELETE FROM endereco"), indexOfPrefix(tx.execs, "DELETE FROM pessoa")
	if idxAddr < 0 || idxBase < 0 {
		t.Fatalf("orphan delete must remove base-children AND the base, got %v", tx.execs)
	}
	if idxAddr > idxBase {
		t.Errorf("base-children must be deleted BEFORE the base (FK integrity), got endereco@%d after pessoa@%d", idxAddr, idxBase)
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

// rowsBaseArchived scripts baseIsArchived to find an archived base (one row with a
// non-null soft-delete marker).
func rowsBaseArchived() func(string, []any) (Rows, error) {
	return func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func(dest []any) error {
			if p, ok := dest[0].(*[]byte); ok {
				*p = []byte("2020-01-01T00:00:00Z")
			}
			return nil
		}}, nil
	}
}

func TestConvergeBase_ArchiveLastActiveRoleArchivesBase(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	arch, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()} // no role left active
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), arch, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE pessoa SET deleted_at = NOW()") }) {
		t.Errorf("archiving the last active role must archive the base (UPDATE pessoa SET deleted_at = NOW()), got %v", tx.execs)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE endereco SET deleted_at = NOW()") }) {
		t.Errorf("the base archive must cascade to the base-children (UPDATE endereco SET deleted_at = NOW()), got %v", tx.execs)
	}
}

func TestConvergeBase_ArchiveWithAnotherActiveRoleKeepsBase(t *testing.T) {
	e := &bcRole{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	arch, _ := domain.GetArchivable(e, nil, "GetArchivable")
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 1}, nil }} // another active role
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), arch, bcRoleSchema(true), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE pessoa SET deleted_at = NOW()") {
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
	tx := &recTx{queryFn: rowsBaseArchived()} // base currently archived
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
