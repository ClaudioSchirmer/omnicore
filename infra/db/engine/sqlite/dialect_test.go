//go:build sqlite

package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

func TestDialect_PlaceholderQuoteIdent(t *testing.T) {
	d := sqliteDialect{}
	if got := d.Placeholder(3); got != "?" {
		t.Errorf("Placeholder = %q, want ?", got)
	}
	if got := d.QuoteIdent("created_at"); got != `"created_at"` {
		t.Errorf("QuoteIdent = %q", got)
	}
}

func TestQuoteIdent_PanicsOnInvalid(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on an invalid identifier")
		}
	}()
	quoteIdent("bad name; DROP")
}

func TestDialect_LikeClauses(t *testing.T) {
	d := sqliteDialect{}
	// Fix #11: both clauses declare ESCAPE '\' so the backslash the criteria
	// pattern builder uses to escape %/_/\ is honored (SQLite LIKE has no default
	// escape character).
	// Case-insensitive: LOWER both sides (ASCII-only fold, D9).
	if got := d.ILikeClause(`"name"`, "?"); got != `LOWER("name") LIKE LOWER(?) ESCAPE '\'` {
		t.Errorf("ILikeClause = %q", got)
	}
	// Case-sensitive: bare LIKE (the case_sensitive_like pragma is the mechanism).
	if got := d.LikeClause(`"name"`, "?"); got != `"name" LIKE ? ESCAPE '\'` {
		t.Errorf("LikeClause = %q", got)
	}
}

func TestDialect_NowExprAndLimits(t *testing.T) {
	d := sqliteDialect{}
	if got := d.NowExpr(); got != "strftime('%Y-%m-%d %H:%M:%f','now')" {
		t.Errorf("NowExpr = %q", got)
	}
	if got := d.ApplyLimit("SELECT 1", 5); got != "SELECT 1 LIMIT 5" {
		t.Errorf("ApplyLimit = %q", got)
	}
	if got := d.ApplyLimitOffset("SELECT 1 ORDER BY x", 5, 10); got != "SELECT 1 ORDER BY x LIMIT 5 OFFSET 10" {
		t.Errorf("ApplyLimitOffset = %q", got)
	}
}

func TestDialect_SavepointTrio(t *testing.T) {
	d := sqliteDialect{}
	if got := d.Savepoint("sp"); got != "SAVEPOINT sp" {
		t.Errorf("Savepoint = %q", got)
	}
	if got := d.RollbackToSavepoint("sp"); got != "ROLLBACK TO SAVEPOINT sp" {
		t.Errorf("RollbackToSavepoint = %q", got)
	}
	// SQLite HAS RELEASE (unlike T-SQL), so no empty-string special case.
	if got := d.ReleaseSavepoint("sp"); got != "RELEASE SAVEPOINT sp" {
		t.Errorf("ReleaseSavepoint = %q", got)
	}
}

func TestDecodeID_Identity(t *testing.T) {
	got, err := sqliteDialect{}.DecodeID("a1b2c3d4-0000-7000-8000-000000000000")
	if err != nil || got != "a1b2c3d4-0000-7000-8000-000000000000" {
		t.Errorf("DecodeID = %q, %v", got, err)
	}
}

func TestEncodeArg(t *testing.T) {
	id := domain.NewID("a1b2c3d4-0000-7000-8000-000000000000")
	when := time.Date(2026, 7, 31, 21, 10, 0, 123000000, time.UTC)

	cases := []struct {
		name string
		in   any
		want any
	}{
		{"domain.ID", id, "a1b2c3d4-0000-7000-8000-000000000000"},
		{"*domain.ID nil", (*domain.ID)(nil), nil},
		{"*domain.ID val", &id, "a1b2c3d4-0000-7000-8000-000000000000"},
		{"time.Time RFC3339", when, "2026-07-31T21:10:00.123Z"},
		{"*time.Time nil", (*time.Time)(nil), nil},
		{"*time.Time val", &when, "2026-07-31T21:10:00.123Z"},
		{"string passthrough", "10.10", "10.10"},
		{"int passthrough", 42, 42},
		{"bool passthrough", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := encodeArg(c.in); got != c.want {
				t.Errorf("encodeArg(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestUniqueColumnList(t *testing.T) {
	cases := map[string]string{
		"constraint failed: UNIQUE constraint failed: t.email (2067)":  "t.email",
		"constraint failed: UNIQUE constraint failed: t.a, t.b (2067)": "t.a, t.b",
		"constraint failed: UNIQUE constraint failed: users.id (1555)": "users.id",
		"some other error":                   "",
		"UNIQUE constraint failed: bare.col": "bare.col",
	}
	for msg, want := range cases {
		if got := uniqueColumnList(msg); got != want {
			t.Errorf("uniqueColumnList(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestViolationClassifiers_NonSQLiteError(t *testing.T) {
	d := sqliteDialect{}
	// A non-sqlite error classifies as neither (the positive paths, which need a
	// real *sqlite.Error, are covered by the integration test).
	if _, ok := d.IsUniqueViolation(errors.New("boom")); ok {
		t.Error("IsUniqueViolation on a generic error should be false")
	}
	if _, ok := d.IsForeignKeyViolation(errors.New("boom")); ok {
		t.Error("IsForeignKeyViolation on a generic error should be false")
	}
}

func TestBuildUpsert(t *testing.T) {
	d := sqliteDialect{}

	// DO NOTHING when there are no sets.
	got := d.BuildUpsert("users", []string{"id", "email"}, []string{"id"}, nil)
	want := `INSERT INTO "users" ("id", "email") VALUES (?, ?) ON CONFLICT ("id") DO NOTHING`
	if got != want {
		t.Errorf("DO NOTHING upsert:\n got %q\nwant %q", got, want)
	}

	// DO UPDATE: excluded.col for a New assignment, table.col+1 for a Bump.
	sets := []core.UpsertSet{
		{Col: "email", Mode: core.UpsertSetNew},
		{Col: "revision", Mode: core.UpsertSetBump},
	}
	got = d.BuildUpsert("users", []string{"id", "email", "revision"}, []string{"id"}, sets)
	want = `INSERT INTO "users" ("id", "email", "revision") VALUES (?, ?, ?) ON CONFLICT ("id") DO UPDATE SET "email" = excluded."email", "revision" = "users"."revision" + 1`
	if got != want {
		t.Errorf("DO UPDATE upsert:\n got %q\nwant %q", got, want)
	}

	// A conflict-ONLY assignment (write.OnUpdate) binds a placeholder of its
	// own, after the inserted columns — the clause follows the VALUES list,
	// exactly where the caller appends its arguments.
	sets = []core.UpsertSet{
		{Col: "email", Mode: core.UpsertSetNew},
		{Col: "repeated_at", Mode: core.UpsertSetArg},
	}
	got = d.BuildUpsert("users", []string{"id", "email"}, []string{"id"}, sets)
	want = `INSERT INTO "users" ("id", "email") VALUES (?, ?) ON CONFLICT ("id") DO UPDATE SET "email" = excluded."email", "repeated_at" = ?`
	if got != want {
		t.Errorf("conflict-only upsert:\n got %q\nwant %q", got, want)
	}
}
