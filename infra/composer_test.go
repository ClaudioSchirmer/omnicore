package infra

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestNormalizeSQLValue_UUIDByteArray covers the root-cause fix for the
// SyncEngine read-path bug: pgx returns UUID columns from rows.Values() as
// [16]byte, which downstream code (applyEmbeds) was formatting via "%v" into
// a literal byte-array string "[16 86 99 …]" that Postgres rejected with
// SQLSTATE 22P02. Normalizing at the row-decode boundary fixes both the SQL
// placeholder path and the BSON serialization path in one place.
func TestNormalizeSQLValue_UUIDByteArray(t *testing.T) {
	u := uuid.MustParse("105663e1-dbb8-4ae9-a60b-0b2b66ac5c2a")
	got := normalizeSQLValue([16]byte(u))
	want := u.String()
	if got != want {
		t.Fatalf("expected canonical UUID string %q, got %v (%T)", want, got, got)
	}
}

// TestNormalizeSQLValue_PassThrough verifies non-[16]byte values are returned
// verbatim so existing fields (strings, ints, timestamps) are untouched.
func TestNormalizeSQLValue_PassThrough(t *testing.T) {
	// Comparable scalars go through == directly.
	scalars := []any{"already a string", 42, int64(1234567890), 3.14, true}
	for _, c := range scalars {
		if got := normalizeSQLValue(c); got != c {
			t.Errorf("expected scalar %v (%T) to pass through, got %v (%T)", c, c, got, got)
		}
	}
	if got := normalizeSQLValue(nil); got != nil {
		t.Errorf("expected nil to pass through, got %v", got)
	}
	// []byte is not directly comparable; assert type only — a 17-byte slice
	// must not be reinterpreted as a UUID (only [16]byte arrays are).
	raw := []byte("seventeen bytes!!")
	got := normalizeSQLValue(raw)
	if _, ok := got.([]byte); !ok {
		t.Errorf("expected []byte to pass through unchanged, got %T", got)
	}
}

// TestBuildFetchSQL_IncludeArchivedOmitsFilter locks the SQL shape used by
// the canonical default (keep-archived): Compose passes includeArchived=true
// down to every fetch (root + cascading through embeds), and the WHERE
// deleted_at IS NULL clause is omitted. Archived rows reach the Mongo
// projection symmetrically with PostgreSQL; consumers that pass
// IncludeArchived=true on the reader path (e.g. ?includeArchived=true) can see them.
func TestBuildFetchSQL_IncludeArchivedOmitsFilter(t *testing.T) {
	cases := []struct {
		name, verb, table, keyCol, want string
	}{
		{"row", "row", "users", "id", "SELECT * FROM users WHERE id = $1 LIMIT 1"},
		{"where", "where", "addresses", "user_id", "SELECT * FROM addresses WHERE user_id = $1"},
		{"all", "all", "users", "", "SELECT * FROM users"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildFetchSQL(c.verb, c.table, c.keyCol, "deleted_at", true)
			if got != c.want {
				t.Fatalf("buildFetchSQL(%q, %q, %q, true) = %q, want %q",
					c.verb, c.table, c.keyCol, got, c.want)
			}
			if strings.Contains(got, "deleted_at") {
				t.Errorf("includeArchived=true must not emit deleted_at: %q", got)
			}
		})
	}
}

// TestBuildFetchSQL_DeleteOnArchiveAppliesFilter locks the SQL shape used by
// the opt-in (hot-tier) view: when DeleteOnArchive is set, Compose passes
// includeArchived=false down to every fetch (row, where, all) and the WHERE
// deleted_at IS NULL clause is appended (or used as the bare WHERE for
// fetchAll). The Mongo projection mirrors only active data — the explicit
// consumer choice when the view opts in.
func TestBuildFetchSQL_DeleteOnArchiveAppliesFilter(t *testing.T) {
	cases := []struct {
		name, verb, table, keyCol, want string
	}{
		{"row", "row", "users", "id", "SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL LIMIT 1"},
		{"where", "where", "addresses", "user_id", "SELECT * FROM addresses WHERE user_id = $1 AND deleted_at IS NULL"},
		{"all", "all", "users", "", "SELECT * FROM users WHERE deleted_at IS NULL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildFetchSQL(c.verb, c.table, c.keyCol, "deleted_at", false)
			if got != c.want {
				t.Fatalf("buildFetchSQL(%q, %q, %q, false) = %q, want %q",
					c.verb, c.table, c.keyCol, got, c.want)
			}
		})
	}
}

// TestCompose_CascadeFromViewFlag_DefaultKeep_Root documents the structural
// cascade of the default (keep) policy on a root-only view: ViewDefinition
// reports DeletesOnArchive()=false → Compose computes
// includeArchived=!false=true → buildFetchSQL on the root SELECT omits the
// deleted_at filter. The relationship between the public flag and the SQL
// form is the contract this test protects against silent inversion.
func TestCompose_CascadeFromViewFlag_DefaultKeep_Root(t *testing.T) {
	v := View("things").Root("things")
	if v.DeletesOnArchive() {
		t.Fatal("default view must report DeletesOnArchive()=false")
	}
	include := !v.DeletesOnArchive()
	sql := buildFetchSQL("row", v.RootTable(), "id", "deleted_at", include)
	if strings.Contains(sql, "deleted_at") {
		t.Fatalf("default view must omit deleted_at filter on root SELECT, got %q", sql)
	}
}

// TestCompose_CascadeFromViewFlag_DefaultKeep_Aggregate verifies the same
// cascade applies symmetrically to root + every embed source on a default
// aggregate view: the single flag value drives root AND child fetches in
// applyEmbeds. Without this guarantee, archived rows would survive on the
// root but vanish from children (or vice-versa), producing an inconsistent
// projection.
func TestCompose_CascadeFromViewFlag_DefaultKeep_Aggregate(t *testing.T) {
	v := View("users").Root("users").
		EmbedMany("addresses", From("addresses").On("user_id"))
	if v.DeletesOnArchive() {
		t.Fatal("default aggregate view must report DeletesOnArchive()=false")
	}
	include := !v.DeletesOnArchive()
	rootSQL := buildFetchSQL("row", v.RootTable(), "id", "deleted_at", include)
	if strings.Contains(rootSQL, "deleted_at") {
		t.Fatalf("default aggregate view must omit deleted_at on root, got %q", rootSQL)
	}
	for _, e := range v.Embeds() {
		childSQL := buildFetchSQL("where", e.source.table, e.source.joinKey, "deleted_at", include)
		if strings.Contains(childSQL, "deleted_at") {
			t.Fatalf("default aggregate view must omit deleted_at on embed %q, got %q",
				e.field, childSQL)
		}
	}
}

// TestCompose_CascadeFromViewFlag_DeleteOnArchive_Root documents the
// structural cascade of the opt-in (hot-tier) policy on a root-only view:
// View("x").DeleteOnArchive() reports DeletesOnArchive()=true → Compose
// computes includeArchived=!true=false → buildFetchSQL on the root SELECT
// appends `AND deleted_at IS NULL`. Combined with the default test above,
// this fixes the direction of the inversion explicitly.
func TestCompose_CascadeFromViewFlag_DeleteOnArchive_Root(t *testing.T) {
	v := View("things").DeleteOnArchive().Root("things")
	if !v.DeletesOnArchive() {
		t.Fatal("DeleteOnArchive() view must report DeletesOnArchive()=true")
	}
	include := !v.DeletesOnArchive()
	sql := buildFetchSQL("row", v.RootTable(), "id", "deleted_at", include)
	if !strings.Contains(sql, "AND deleted_at IS NULL") {
		t.Fatalf("DeleteOnArchive view must apply deleted_at filter on root, got %q", sql)
	}
}

// TestCompose_CascadeFromViewFlag_DeleteOnArchive_Aggregate verifies the
// hot-tier cascade reaches every embed on an aggregate view: root + each
// child fetch applies the deleted_at filter (there is no per-embed override
// — the flag governs the whole projection symmetrically).
func TestCompose_CascadeFromViewFlag_DeleteOnArchive_Aggregate(t *testing.T) {
	v := View("users").DeleteOnArchive().Root("users").
		EmbedMany("addresses", From("addresses").On("user_id"))
	if !v.DeletesOnArchive() {
		t.Fatal("DeleteOnArchive() aggregate view must report DeletesOnArchive()=true")
	}
	include := !v.DeletesOnArchive()
	rootSQL := buildFetchSQL("row", v.RootTable(), "id", "deleted_at", include)
	if !strings.Contains(rootSQL, "AND deleted_at IS NULL") {
		t.Fatalf("DeleteOnArchive aggregate must apply filter on root, got %q", rootSQL)
	}
	for _, e := range v.Embeds() {
		childSQL := buildFetchSQL("where", e.source.table, e.source.joinKey, "deleted_at", include)
		if !strings.Contains(childSQL, "AND deleted_at IS NULL") {
			t.Fatalf("DeleteOnArchive aggregate must apply filter on embed %q, got %q",
				e.field, childSQL)
		}
	}
}
