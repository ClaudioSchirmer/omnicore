package write

import (
	"strings"
	"testing"
)

// A shared base is persisted via an explicit INSERT (cold identity) / UPDATE
// (warm identity) keyed on the deterministic PK — NEVER a DB-native upsert.
//
// Why this matters: Postgres' `ON CONFLICT (pk)` is scoped to the primary key,
// but MySQL's `ON DUPLICATE KEY UPDATE` fires on ANY unique key. With a blind
// upsert, a SECOND unique column on the shared base (e.g. a unique email beside
// the natural-key PK) hijacked the write on MySQL: a new-identity POST whose
// email already existed matched the email unique key, ran the UPDATE branch on
// the WRONG persons row, never inserted the new base, and the role FK failed →
// 500 instead of a clean 409. A plain INSERT makes that second unique column
// raise a normal unique violation the repository's ConstraintBinding maps to 409
// on BOTH dialects. This is the regression guard.
func TestUpsertSharedBase_ColdInsertIsPlainInsert_NotBlindUpsert(t *testing.T) {
	ins := roleInsertable(t, "GetUpsertable")
	tx := &recTx{queryFn: rowsNone()} // base absent → cold path (identity is new)
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	i := indexOfPrefix(tx.execs, "INSERT INTO pessoa")
	if i < 0 {
		t.Fatalf("cold shared-base write must be a plain INSERT INTO pessoa, got %v", tx.execs)
	}
	baseSQL := strings.ToUpper(tx.execs[i])
	if strings.Contains(baseSQL, "ON CONFLICT") || strings.Contains(baseSQL, "ON DUPLICATE KEY") {
		t.Errorf("shared-base cold write must NOT be a DB upsert — a second unique column has to raise a clean violation: %q", tx.execs[i])
	}
}

// The warm path (identity already exists) updates the shared fields by PK — an
// UPDATE, not an INSERT and not an upsert. Keyed on the deterministic base id,
// so it never rewrites the immutable natural key.
func TestUpsertSharedBase_WarmPathUpdatesByPK(t *testing.T) {
	ins := roleInsertable(t, "GetUpsertable")
	// base present (rowsFound) → warm path; but an active role must NOT already
	// exist or the POST 409s before the base write, so alternate: base-exists
	// probe true, active-role probe false. scriptedQuery drives that by table.
	tx := &recTx{queryFn: scriptedQuery(nil, []string{"FROM pessoa"})} // base exists, role probe returns none
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "UPDATE pessoa SET") }) {
		t.Errorf("warm shared-base write must be an UPDATE pessoa SET, got %v", tx.execs)
	}
	if hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "INSERT INTO pessoa") }) {
		t.Errorf("warm path must not INSERT the base, got %v", tx.execs)
	}
}
