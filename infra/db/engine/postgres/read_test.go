//go:build postgres

package postgres

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestDialectViolationClassifiers proves the SQLSTATE classification both
// write-path guards depend on: 23505 → unique (409 conflict binding), 23503 →
// foreign key (the shared-base orphan-purge veto). Anything else — other
// SQLSTATEs or non-pg errors — classifies as neither.
func TestDialectViolationClassifiers(t *testing.T) {
	d := pgDialect{}

	if name, ok := d.IsUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "uniq_email"}); !ok || name != "uniq_email" {
		t.Fatalf("IsUniqueViolation = (%q,%v), want (uniq_email,true)", name, ok)
	}
	if name, ok := d.IsForeignKeyViolation(&pgconn.PgError{Code: "23503", ConstraintName: "fk_aluno_pessoa"}); !ok || name != "fk_aluno_pessoa" {
		t.Fatalf("IsForeignKeyViolation = (%q,%v), want (fk_aluno_pessoa,true)", name, ok)
	}
	if _, ok := d.IsForeignKeyViolation(&pgconn.PgError{Code: "23505"}); ok {
		t.Fatal("a unique violation must not classify as a foreign-key violation")
	}
	if _, ok := d.IsUniqueViolation(&pgconn.PgError{Code: "23503"}); ok {
		t.Fatal("a foreign-key violation must not classify as a unique violation")
	}
	if _, ok := d.IsForeignKeyViolation(errFake); ok {
		t.Fatal("a non-pg error must not classify")
	}
}

// composerRows is a programmable pgx.Rows for pgxRowsToMaps — the dynamic-shape
// read the composer drives through pgQuerier.QueryMaps. It models the two methods
// pgxRowsToMaps consumes that the Scan-shaped fakes do not: FieldDescriptions()
// and Values().
type composerRows struct {
	pgx.Rows

	cols      []string
	data      [][]any
	pos       int
	valuesErr error
}

func (r *composerRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *composerRows) Values() ([]any, error) {
	if r.valuesErr != nil {
		return nil, r.valuesErr
	}
	return r.data[r.pos-1], nil
}

func (r *composerRows) FieldDescriptions() []pgconn.FieldDescription {
	fd := make([]pgconn.FieldDescription, len(r.cols))
	for i, c := range r.cols {
		fd[i].Name = c
	}
	return fd
}

func (r *composerRows) Close()     {}
func (r *composerRows) Err() error { return nil }

// TestNowExpr_ApplyLimit proves the two portability seams every generated
// statement rides: the current-timestamp literal comes from the dialect (never
// baked into shared code) and the row cap lands as Postgres' native tail clause.
func TestNowExpr_ApplyLimit(t *testing.T) {
	d := pgDialect{}
	if got := d.NowExpr(); got != "NOW()" {
		t.Fatalf("NowExpr = %q, want NOW()", got)
	}
	if got := d.ApplyLimit("SELECT 1 FROM t WHERE x = $1", 1); got != "SELECT 1 FROM t WHERE x = $1 LIMIT 1" {
		t.Fatalf("ApplyLimit = %q", got)
	}
	if got := d.ApplyLimit("SELECT id FROM t ORDER BY id", 25); got != "SELECT id FROM t ORDER BY id LIMIT 25" {
		t.Fatalf("ApplyLimit = %q", got)
	}
}

// TestApplyLimitOffset proves the windowed-page seam: Postgres appends the
// native `LIMIT n OFFSET m` tail after the (caller-guaranteed) ORDER BY.
func TestApplyLimitOffset(t *testing.T) {
	d := pgDialect{}
	if got := d.ApplyLimitOffset("SELECT id FROM t ORDER BY id", 25, 50); got != "SELECT id FROM t ORDER BY id LIMIT 25 OFFSET 50" {
		t.Fatalf("ApplyLimitOffset = %q", got)
	}
}

func TestPgxRowsToMaps_RowsAndValuesError(t *testing.T) {
	// Happy path: columns become map keys.
	maps, err := pgxRowsToMaps(&composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}})
	if err != nil {
		t.Fatalf("pgxRowsToMaps: %v", err)
	}
	if len(maps) != 1 || maps[0]["id"] != "o1" || maps[0]["name"] != "first" {
		t.Errorf("row→map drifted: %v", maps)
	}

	// Values() error surfaces.
	if _, err := pgxRowsToMaps(&composerRows{cols: []string{"id"}, data: [][]any{{"x"}}, valuesErr: errFake}); err == nil {
		t.Fatal("expected Values() error to surface")
	}
}

func TestNormalizeSQLValue_UUIDBytes(t *testing.T) {
	// A [16]byte UUID is restrung to its canonical form (the root-cause fix for
	// the SyncEngine read-path bug where pgx hands UUID columns back as [16]byte
	// and "%v" formatting produced a literal "[16 86 …]" string PG then rejected).
	u := uuid.MustParse("105663e1-dbb8-4ae9-a60b-0b2b66ac5c2a")
	if got := normalizeSQLValue([16]byte(u)); got != u.String() {
		t.Errorf("normalizeSQLValue([16]byte) = %v (%T), want %v", got, got, u.String())
	}
	if got := NormalizeSQLValue([16]byte(u)); got != u.String() {
		t.Errorf("NormalizeSQLValue([16]byte) = %v, want %v", got, u.String())
	}
}

// TestNormalizeSQLValue_PassThrough verifies non-[16]byte values are returned
// verbatim so existing fields (strings, ints, timestamps) are untouched.
func TestNormalizeSQLValue_PassThrough(t *testing.T) {
	scalars := []any{"already a string", 42, int64(1234567890), 3.14, true}
	for _, c := range scalars {
		if got := normalizeSQLValue(c); got != c {
			t.Errorf("expected scalar %v (%T) to pass through, got %v (%T)", c, c, got, got)
		}
	}
	if got := normalizeSQLValue(nil); got != nil {
		t.Errorf("expected nil to pass through, got %v", got)
	}
	// A 17-byte slice must NOT be reinterpreted as a UUID (only [16]byte arrays).
	raw := []byte("seventeen bytes!!")
	if _, ok := normalizeSQLValue(raw).([]byte); !ok {
		t.Errorf("expected []byte to pass through unchanged, got %T", normalizeSQLValue(raw))
	}
}

// TestSavepointStmts locks the savepoint trio the shared-base orphan purge
// renders through the dialect (standard forms).
func TestSavepointStmts(t *testing.T) {
	d := pgDialect{}
	if got := d.Savepoint("sp"); got != "SAVEPOINT sp" {
		t.Errorf("Savepoint = %q", got)
	}
	if got := d.RollbackToSavepoint("sp"); got != "ROLLBACK TO SAVEPOINT sp" {
		t.Errorf("RollbackToSavepoint = %q", got)
	}
	if got := d.ReleaseSavepoint("sp"); got != "RELEASE SAVEPOINT sp" {
		t.Errorf("ReleaseSavepoint = %q", got)
	}
}

// TestLikeClauses proves Postgres renders LIKE/ILIKE natively: ILIKE is
// unconditionally case-insensitive, and a bare LIKE is natively case-sensitive
// (honoring criteria.OpLike's contract with no forced COLLATE).
func TestLikeClauses(t *testing.T) {
	d := pgDialect{}
	if got := d.ILikeClause("name", "$1"); got != "name ILIKE $1" {
		t.Errorf("ILikeClause = %q", got)
	}
	if got := d.LikeClause("name", "$1"); got != "name LIKE $1" {
		t.Errorf("LikeClause = %q", got)
	}
}
