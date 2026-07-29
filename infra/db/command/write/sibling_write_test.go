package write

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for owner-level sibling write partitioning (A2), driven
// through the recording fake WriteTx. The fixture is a FLAT entity (not an
// aggregate) split across an anchor table "pessoa" and a sibling table "usuario"
// holding a pointer field, so the nil/non-nil materialization branches are
// exercisable. The sibling upsert SQL is the Dialect's contract; here we assert
// WHICH statements are emitted and in what order.

type sibTestEntity struct {
	domain.BaseEntity
	Name     string
	UserName *string // sibling field — pointer so it can be genuinely nil
}

func (e *sibTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *sibTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func sibTestSchema() *TableSchema {
	return NewTableSchema[*sibTestEntity]("pessoa").
		ID("id").
		Field("Name", "name").
		Sibling(NewSiblingSchema[*sibTestEntity]("usuario").Field("UserName", "user_name"))
}

func strptr(s string) *string { return &s }

func TestBaseEngine_FlatInsert_MaterializesSibling(t *testing.T) {
	ins, err := domain.GetInsertable(&sibTestEntity{Name: "a", UserName: strptr("alice")}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, sibTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// anchor INSERT + sibling INSERT + outbox + audit = 4.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements (anchor+sibling+outbox+audit), got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "INSERT INTO pessoa") {
		t.Errorf("stmt[0]: want anchor INSERT, got %q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "INSERT INTO usuario") {
		t.Errorf("stmt[1]: want sibling INSERT, got %q", tx.execs[1])
	}
}

func TestBaseEngine_FlatInsert_SkipsAllNilSibling(t *testing.T) {
	ins, err := domain.GetInsertable(&sibTestEntity{Name: "a", UserName: nil}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, sibTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// All-nil sibling is not materialized: anchor + outbox + audit = 3.
	if len(tx.execs) != 3 {
		t.Fatalf("expected 3 statements (no sibling), got %d: %v", len(tx.execs), tx.execs)
	}
	for _, s := range tx.execs {
		if strings.Contains(s, "usuario") {
			t.Errorf("an all-nil sibling must not be written, got %q", s)
		}
	}
}

func TestBaseEngine_FlatUpdate_UpsertsSibling(t *testing.T) {
	e := &sibTestEntity{Name: "a", UserName: strptr("bob")}
	e.SetID(domain.NewID(uuid.NewString()))
	upd, err := domain.GetUpdatable(e, func(*sibTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1} // anchor UPDATE matches one row
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, sibTestSchema(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// anchor UPDATE + sibling UPSERT + outbox + audit = 4.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.Contains(tx.execs[1], "usuario") {
		t.Errorf("stmt[1]: want sibling upsert on usuario, got %q", tx.execs[1])
	}
}

// A PATCH (partial) leaves an all-nil sibling untouched; a PUT (full) that
// cleared it removes the row.
func TestBaseEngine_FlatUpdate_AllNilSibling_PatchVsPut(t *testing.T) {
	mk := func(partial bool) *recTx {
		e := &sibTestEntity{Name: "a", UserName: nil}
		e.SetID(domain.NewID(uuid.NewString()))
		var upd domain.Updatable
		var err error
		if partial {
			upd, err = domain.GetPartialUpdatable(e, func(*sibTestEntity) error { return nil }, nil, "GetPartialUpdatable")
		} else {
			upd, err = domain.GetUpdatable(e, func(*sibTestEntity) error { return nil }, nil, "GetUpdatable")
		}
		if err != nil {
			t.Fatalf("GetUpdatable(partial=%v): %v", partial, err)
		}
		tx := &recTx{count: 1}
		be := newFlatBE(&recBeginner{tx: tx})
		if _, err := be.Update(newBuilderCtx(), upd, sibTestSchema(), firingHook); err != nil {
			t.Fatalf("Update(partial=%v): %v", partial, err)
		}
		return tx
	}

	// PATCH: sibling untouched → anchor UPDATE + outbox + audit = 3, no usuario stmt.
	patch := mk(true)
	if len(patch.execs) != 3 {
		t.Fatalf("PATCH: expected 3 statements (sibling untouched), got %d: %v", len(patch.execs), patch.execs)
	}
	for _, s := range patch.execs {
		if strings.Contains(s, "usuario") {
			t.Errorf("PATCH must not touch an all-nil sibling, got %q", s)
		}
	}

	// PUT: cleared slice → sibling DELETE emitted (anchor UPDATE + DELETE + outbox + audit = 4).
	put := mk(false)
	if len(put.execs) != 4 {
		t.Fatalf("PUT: expected 4 statements (sibling delete), got %d: %v", len(put.execs), put.execs)
	}
	if !strings.HasPrefix(put.execs[1], "DELETE FROM usuario") {
		t.Errorf("PUT stmt[1]: want sibling DELETE, got %q", put.execs[1])
	}
}

func TestBaseEngine_FlatDelete_DeletesSiblingThenRoot(t *testing.T) {
	e := &sibTestEntity{Name: "a", UserName: strptr("c")}
	e.SetID(domain.NewID(uuid.NewString()))
	del, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), del, sibTestSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// sibling DELETE + root DELETE + outbox + audit = 4, sibling before root.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "DELETE FROM usuario") {
		t.Errorf("stmt[0]: want sibling DELETE first, got %q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "DELETE FROM pessoa") {
		t.Errorf("stmt[1]: want root DELETE after sibling, got %q", tx.execs[1])
	}
}
