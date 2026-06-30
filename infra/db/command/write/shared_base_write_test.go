package write

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

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
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *roleTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func roleTestSchema() *TableSchema {
	base := NewSharedBase("pessoa").
		PK("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalKey("document")
	return NewTableSchema[*roleTestEntity]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		SharedBase(base, "pessoa_id")
}

// rowsNone scripts findRoleByFK to find no role; rowsActive/rowsArchived script
// one row with a null / non-null soft-delete.
func rowsNone() func(string, []any) (Rows, error) {
	return func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }
}
func rowsState(archived bool) func(string, []any) (Rows, error) {
	return func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func(dest []any) error {
			if p, ok := dest[0].(*string); ok {
				*p = "role-1"
			}
			if archived {
				if p, ok := dest[1].(*[]byte); ok {
					*p = []byte("2020-01-01T00:00:00Z")
				}
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
	// upsert pessoa + insert aluno + outbox(role) + outbox(base fan-out) + audit = 5.
	if len(tx.execs) != 5 {
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
	tx := &recTx{queryFn: rowsState(false)} // active role exists
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

func TestInsertRoleWithBase_ArchivedRevives(t *testing.T) {
	ins, _ := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	tx := &recTx{queryFn: rowsState(true)} // archived role exists
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert (revive): %v", err)
	}
	// upsert + revive + outbox(role) + outbox(base) + audit = 5.
	if len(tx.execs) != 5 {
		t.Fatalf("expected 5 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[1], "UPDATE aluno SET deleted_at = NULL") {
		t.Errorf("stmt[1] must revive the archived role, got %q", tx.execs[1])
	}
}

func TestDeleteRoleWithBase_OrphanedDeletesBase(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	del, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	// No other role references the base → the refcount probe finds nothing.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 0}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), del, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE role + DELETE base + outbox + audit = 4.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "DELETE FROM aluno") {
		t.Errorf("stmt[0] must delete the role, got %q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "DELETE FROM pessoa") {
		t.Errorf("stmt[1] must delete the orphaned base, got %q", tx.execs[1])
	}
}

func TestDeleteRoleWithBase_StillReferencedKeepsBase(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	del, _ := domain.GetDeletable(e, nil, "GetDeletable")
	// Another role still references the base → the probe finds a row.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return &fakeRows{remaining: 1}, nil }}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), del, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// DELETE role + outbox + audit = 3 (base kept).
	if len(tx.execs) != 3 {
		t.Fatalf("expected 3 statements (base kept), got %d: %v", len(tx.execs), tx.execs)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "DELETE FROM pessoa") {
			t.Errorf("a still-referenced base must not be deleted, got %q", s)
		}
	}
}

// An AGGREGATE role (children) that also declares a SharedBase: the write must
// upsert the base, insert the role row, AND insert the children — the case that
// previously fell into insertAggregate and silently dropped the base.
type aggRoleChild struct {
	ID    string
	Label string
}

func (c aggRoleChild) GetID() string                                    { return c.ID }
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
func (e *aggRoleEntity) GetAggregateRoot() *domain.AggregateRoot { return &e.AggregateRoot }
func (e *aggRoleEntity) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{aggRoleChild{}}
}

func aggRoleSchema() *TableSchema {
	base := NewSharedBase("pessoa").PK("id").Field("Name", "name").Field("Document", "document").NaturalKey("document")
	return NewTableSchema[*aggRoleEntity]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		Child(NewTableSchema[aggRoleChild]("aluno_disciplinas").PK("id").FK("aluno_id").Field("Label", "label")).
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
	// upsert pessoa + insert aluno + insert child + outbox(role) + outbox(base) + audit = 6.
	if len(tx.execs) != 6 {
		t.Fatalf("expected 6 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.Contains(tx.execs[0], "pessoa") {
		t.Errorf("the aggregate role's base MUST be upserted (was silently dropped before), got stmt[0]=%q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "INSERT INTO aluno ") && !strings.HasPrefix(tx.execs[1], "INSERT INTO aluno(") {
		t.Errorf("stmt[1] must insert the role, got %q", tx.execs[1])
	}
	if !strings.HasPrefix(tx.execs[2], "INSERT INTO aluno_disciplinas") {
		t.Errorf("stmt[2] must insert the aggregate child, got %q", tx.execs[2])
	}
}

func TestUpdateRoleWithBase(t *testing.T) {
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M2"}
	e.SetID(domain.NewID(uuid.NewString()))
	upd, err := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1} // role UPDATE matches one row
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// update aluno + upsert pessoa + outbox(role) + outbox(base) + audit = 5.
	if len(tx.execs) != 5 {
		t.Fatalf("expected 5 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "UPDATE aluno") {
		t.Errorf("stmt[0] must update the role, got %q", tx.execs[0])
	}
	if !strings.Contains(tx.execs[1], "pessoa") {
		t.Errorf("stmt[1] must upsert the shared base, got %q", tx.execs[1])
	}
}
