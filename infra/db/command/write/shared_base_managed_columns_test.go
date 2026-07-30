package write

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// A shared base that DECLARES managed columns must have them honored by the
// base write: CreatedAt(+UpdatedAt) stamped on the identity's creation (cold
// insert), UpdatedAt stamped on the warm role-driven update of shared fields —
// both bound as ordinary args carrying the operation's writeNow() stamp (the
// app clock authors managed timestamps; no dialect NOW() in the data DML).
// A base that declares none keeps byte-identical SQL (covered by the existing
// upsert-scoped tests, whose base declares no managed columns).

func roleTestSchemaManagedBase() *TableSchema {
	base := NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
	return NewTableSchema[*roleTestEntity]("aluno").
		ID("id").
		Field("Matricula", "matricula").
		DeletedAt("deleted_at").
		SharedBase(base, "id")
}

func TestUpsertSharedBase_ColdInsertStampsDeclaredManagedColumns(t *testing.T) {
	ins, err := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{queryFn: rowsNone()} // base absent → cold path
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchemaManagedBase(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	i := indexOfPrefix(tx.execs, "INSERT INTO pessoa")
	if i < 0 {
		t.Fatalf("expected a plain INSERT INTO pessoa, got %v", tx.execs)
	}
	sql := tx.execs[i]
	if !strings.Contains(sql, "created_at") || !strings.Contains(sql, "updated_at") {
		t.Errorf("declared managed columns must be stamped on the base's creation, got %q", sql)
	}
	if strings.Contains(strings.ToUpper(sql), "NOW()") {
		t.Errorf("managed columns must bind the app-clock stamp, never a dialect NOW(), got %q", sql)
	}
}

func TestUpsertSharedBase_WarmUpdateStampsDeclaredUpdatedAt(t *testing.T) {
	ins, err := domain.GetInsertable(&roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}, nil, "GetUpsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{queryFn: scriptedQuery(nil, []string{"FROM pessoa"})} // base exists, no active role
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchemaManagedBase(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	var baseUpdate string
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE pessoa SET") {
			baseUpdate = s
			break
		}
	}
	if baseUpdate == "" {
		t.Fatalf("warm path must UPDATE pessoa, got %v", tx.execs)
	}
	if !strings.Contains(baseUpdate, "updated_at") || strings.Contains(strings.ToUpper(baseUpdate), "NOW()") {
		t.Errorf("a declared UpdatedAt must bind the app-clock stamp (no dialect NOW()), got %q", baseUpdate)
	}
	if strings.Contains(baseUpdate, "created_at") {
		t.Errorf("CreatedAt must never be re-stamped on update, got %q", baseUpdate)
	}
}
