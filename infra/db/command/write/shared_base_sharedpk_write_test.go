package write

import (
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for the shared-primary-key SharedBase model: the role's PK
// column IS the base link (fkCol == "id"), so role.id == base.id == UUIDv5(document).
// The write path must write the id column exactly once (as the PK) and use the base
// id as the role's own id — no separate FK column, no duplicate column.

// roleTestSchemaSharedPK mirrors roleTestSchema (shared_base_write_test.go) but points
// the SharedBase reference at the PK column instead of a separate person_id.
func roleTestSchemaSharedPK() *TableSchema {
	base := NewSharedBase("pessoa").Revision("revision").
		PK("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalKey("document")
	return NewTableSchema[*roleTestEntity]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		SharedBase(base, "id")
}

func TestInsertRoleWithBase_SharedPK_New(t *testing.T) {
	ins, err := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()}
	be := newFlatBE(&recBeginner{tx: tx})
	res, err := be.Insert(newBuilderCtx(), ins, roleTestSchemaSharedPK(), firingHook)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// The role's own PK IS the deterministic base id.
	if want := deterministicBaseID("D1"); res.ID.Value() != want {
		t.Errorf("shared-PK role id must equal the base id %q, got %q", want, res.ID)
	}
	// The role INSERT writes the id column once (as the PK) and no separate FK column.
	i := indexOfPrefix(tx.execs, "INSERT INTO aluno")
	if i < 0 {
		t.Fatalf("no role INSERT recorded: %v", tx.execs)
	}
	insertSQL := tx.execs[i]
	if strings.Contains(insertSQL, "id, id") {
		t.Errorf("shared-PK insert must not emit a duplicate id column: %q", insertSQL)
	}
	if strings.Contains(insertSQL, "pessoa_id") {
		t.Errorf("shared-PK insert must not emit a separate FK column: %q", insertSQL)
	}
}

// The existence matrix is unchanged under shared-PK: an active role still conflicts
// (409), found by the active-role probe keying WHERE id = baseID.
func TestInsertRoleWithBase_SharedPK_ActiveConflict409(t *testing.T) {
	ins, _ := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	tx := &recTx{queryFn: rowsFound()} // active role exists
	be := newFlatBE(&recBeginner{tx: tx})
	_, err := be.Insert(newBuilderCtx(), ins, roleTestSchemaSharedPK(), firingHook)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("an active existing role must conflict under shared-PK too, got %T (%v)", err, err)
	}
}

// Under shared-PK an ARCHIVED remnant is likewise invisible to the probe: the
// insert proceeds keyed on the deterministic base id and the PRIMARY KEY
// arbitrates the collision on a real backend (mapped by the repository's
// ConstraintBinding, e.g. to a 409). No revive UPDATE exists on this path.
func TestInsertRoleWithBase_SharedPK_ArchivedRemnantIsInvisible_InsertProceeds(t *testing.T) {
	ins, _ := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	tx := &recTx{queryFn: rowsNone()} // active-only probe: archived remnant → no row
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchemaSharedPK(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "INSERT INTO aluno") }) {
		t.Errorf("insert must proceed as a plain INSERT under shared-PK, got %v", tx.execs)
	}
	if hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE aluno SET deleted_at = NULL") }) {
		t.Errorf("no revive UPDATE may run on the insert path, got %v", tx.execs)
	}
}
