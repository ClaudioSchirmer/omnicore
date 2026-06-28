package pg

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

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
