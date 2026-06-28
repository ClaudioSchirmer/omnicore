package relational

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// mysqlLikeDialect reuses the passthrough test dialect but decodes a 16-byte
// leading value into a canonical uuid string — the MySQL BINARY(16) behavior —
// so decodeChildPK's per-backend normalization can be asserted without the
// engine. Only DecodeID is overridden; the rest is inherited.
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
		PK("id").FK("agg_id").Field("Label", "label").SoftDelete("deleted_at")
}

// On a MySQL-style backend the child's own PK is auto-scanned as raw BINARY(16)
// bytes into its string field; decodeChildPK must normalize it to the canonical
// uuid (the leading FK is decoded by the loader, but the child PK is not).
func TestDecodeChildPK_MySQLBinaryNormalized(t *testing.T) {
	child := covChildSchemaForDecode()
	id := uuid.New()
	vp := reflect.New(child.GoType())
	vp.Elem().FieldByName("ID").SetString(string(id[:])) // 16 raw bytes, as scanned

	if err := decodeChildPK(vp, child, mysqlLikeDialect{}); err != nil {
		t.Fatalf("decodeChildPK: %v", err)
	}
	if got := vp.Elem().Interface().(covChild).GetID(); got != id.String() {
		t.Errorf("child PK not decoded: got %q want %q", got, id.String())
	}
}

// On Postgres the field already holds canonical text; DecodeID is a passthrough
// (the value is not 16 bytes), so the id must survive unchanged.
func TestDecodeChildPK_PostgresPassthrough(t *testing.T) {
	child := covChildSchemaForDecode()
	canonical := uuid.New().String()
	vp := reflect.New(child.GoType())
	vp.Elem().FieldByName("ID").SetString(canonical)

	if err := decodeChildPK(vp, child, testPGDialect{}); err != nil {
		t.Fatalf("decodeChildPK: %v", err)
	}
	if got := vp.Elem().Interface().(covChild).GetID(); got != canonical {
		t.Errorf("passthrough drifted: got %q want %q", got, canonical)
	}
}
