package read

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// mysqlLikeDialect reuses the passthrough test dialect but decodes a 16-byte
// value into a canonical uuid string — the MySQL BINARY(16) behavior — so the
// managed carrier's id normalization can be asserted without the engine. Only
// DecodeID is overridden; the rest is inherited.
type mysqlLikeDialect struct{ testPGDialect }

func (mysqlLikeDialect) DecodeID(raw string) (string, error) {
	if len(raw) == 16 {
		u, err := uuid.FromBytes([]byte(raw))
		if err != nil {
			return "", err
		}
		return u.String(), nil
	}
	return raw, nil
}

func covChildSchemaForDecode() *TableSchema {
	return NewTableSchema[covChild]("cov_children").
		ID("id").ParentID("agg_id").Field("Label", "label").DeletedAt("deleted_at")
}

// A child's own id now lives in the unexported domain.Managed carrier, read as a
// trailing column and stamped by managedScan.apply, which decodes it through the
// dialect. On a MySQL-style backend the id arrives as raw BINARY(16) bytes and
// must normalize to the canonical uuid before SetID.
func TestManagedScanApply_MySQLBinaryIDNormalized(t *testing.T) {
	child := covChildSchemaForDecode()
	id := uuid.New()
	ms := newChildManagedScan(child)
	ms.id = &sql.NullString{String: string(id[:]), Valid: true} // 16 raw bytes, as scanned

	vp := reflect.New(child.GoType())
	if err := ms.apply(vp.Interface(), mysqlLikeDialect{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := vp.Elem().Interface().(covChild).GetID(); got.Value() != id.String() {
		t.Errorf("child id not decoded: got %q want %q", got, id.String())
	}
}

// On Postgres the id is already canonical text; DecodeID is a passthrough (the
// value is not 16 bytes), so it must survive SetID unchanged.
func TestManagedScanApply_PostgresIDPassthrough(t *testing.T) {
	child := covChildSchemaForDecode()
	canonical := uuid.New().String()
	ms := newChildManagedScan(child)
	ms.id = &sql.NullString{String: canonical, Valid: true}

	vp := reflect.New(child.GoType())
	if err := ms.apply(vp.Interface(), testPGDialect{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := vp.Elem().Interface().(covChild).GetID(); got.Value() != canonical {
		t.Errorf("passthrough drifted: got %q want %q", got, canonical)
	}
}
