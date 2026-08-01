//go:build integration && sqlite

package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// newTestEngine opens a SQLite engine against a throwaway .db in the test's temp
// dir — no bench container, which is the whole point of the engine. It returns
// the neutral RelationalEngine plus a table ready for the affinity/operator
// probes.
func newTestEngine(t *testing.T) core.RelationalEngine {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "t.db")
	eng, err := New(context.Background(), core.EngineConfig{DSN: "file:" + dbPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(eng.Close)
	return eng
}

// TestAffinityRoundTrip proves the scan wrapper (read.go) and encodeArg
// (dialect.go) round-trip every affinity class the framework persists: domain.ID
// as TEXT, bool as INTEGER (modernc-native), time.Time as RFC3339 TEXT (via the
// wrapper), a NULLABLE timestamp as sql.NullTime, decimal-as-string as exact
// TEXT, and int/float.
func TestAffinityRoundTrip(t *testing.T) {
	eng := newTestEngine(t)
	q := eng.Querier()
	ctx := context.Background()
	d := eng.Dialect()

	if err := q.Exec(ctx, `CREATE TABLE t (
		id TEXT PRIMARY KEY, flag INTEGER, created_at TEXT, deleted_at TEXT,
		amount TEXT, n INTEGER, f REAL)`); err != nil {
		t.Fatal(err)
	}

	id := domain.NewID("a1b2c3d4-0000-7000-8000-000000000000")
	when := time.Date(2026, 7, 31, 21, 10, 0, 123000000, time.UTC)
	// Bind through EncodeArg exactly as the write path does.
	err := q.Exec(ctx,
		`INSERT INTO t (id, flag, created_at, deleted_at, amount, n, f) VALUES (?,?,?,?,?,?,?)`,
		d.EncodeArg(id), d.EncodeArg(true), d.EncodeArg(when), d.EncodeArg((*time.Time)(nil)),
		d.EncodeArg("10.10"), d.EncodeArg(42), d.EncodeArg(3.14))
	if err != nil {
		t.Fatal(err)
	}

	rows, err := q.Query(ctx, `SELECT id, flag, created_at, deleted_at, amount, n, f FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var (
		gotID              string
		flag               bool
		created            time.Time
		deleted            sql.NullTime
		amount             string
		n                  int64
		f                  float64
	)
	if err := rows.Scan(&gotID, &flag, &created, &deleted, &amount, &n, &f); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotID != id.Value() {
		t.Errorf("id = %q, want %q", gotID, id.Value())
	}
	if !flag {
		t.Error("flag should scan INTEGER 1 → true")
	}
	if !created.Equal(when) {
		t.Errorf("created_at = %v, want %v", created, when)
	}
	if deleted.Valid {
		t.Error("deleted_at NULL should scan into an invalid sql.NullTime")
	}
	if amount != "10.10" {
		t.Errorf("amount = %q, want exact 10.10 (no float coercion)", amount)
	}
	if n != 42 || f != 3.14 {
		t.Errorf("n=%d f=%v", n, f)
	}
}

// TestViolationClassifiers_Real drives real unique / primary-key / foreign-key
// violations through the engine and proves the dialect classifies them (the
// paths a unit test cannot reach without a live *sqlite.Error). foreign_keys is
// ON because the factory forces it (D11) — the veto would be a no-op otherwise.
func TestViolationClassifiers_Real(t *testing.T) {
	eng := newTestEngine(t)
	q := eng.Querier()
	ctx := context.Background()
	d := eng.Dialect()

	if err := q.Exec(ctx, `CREATE TABLE parent (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := q.Exec(ctx, `CREATE TABLE t (id TEXT PRIMARY KEY, email TEXT UNIQUE, pid TEXT REFERENCES parent(id))`); err != nil {
		t.Fatal(err)
	}
	_ = q.Exec(ctx, `INSERT INTO parent VALUES ('p1')`)
	_ = q.Exec(ctx, `INSERT INTO t VALUES ('a','e@x.com','p1')`)

	// Unique violation on email → column list "t.email".
	err := q.Exec(ctx, `INSERT INTO t VALUES ('b','e@x.com','p1')`)
	if col, ok := d.IsUniqueViolation(err); !ok || col != "t.email" {
		t.Errorf("IsUniqueViolation = (%q,%v), want (t.email,true)", col, ok)
	}
	// Primary-key violation → also unique, column list "t.id".
	err = q.Exec(ctx, `INSERT INTO t VALUES ('a','other@x.com','p1')`)
	if col, ok := d.IsUniqueViolation(err); !ok || col != "t.id" {
		t.Errorf("IsUniqueViolation(PK) = (%q,%v), want (t.id,true)", col, ok)
	}
	// Foreign-key violation → ("", true) (SQLite reports no constraint name).
	err = q.Exec(ctx, `INSERT INTO t VALUES ('c','c@x.com','nope')`)
	if name, ok := d.IsForeignKeyViolation(err); !ok || name != "" {
		t.Errorf("IsForeignKeyViolation = (%q,%v), want (\"\",true)", name, ok)
	}
}

// TestUpsertExecutes proves the BuildUpsert SQL runs on SQLite: an insert then a
// conflicting insert that DO UPDATEs, with excluded.col and the table.col+1 bump.
func TestUpsertExecutes(t *testing.T) {
	eng := newTestEngine(t)
	q := eng.Querier()
	ctx := context.Background()
	d := eng.Dialect()

	if err := q.Exec(ctx, `CREATE TABLE u (id TEXT PRIMARY KEY, email TEXT, revision INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	sets := []core.UpsertSet{
		{Col: "email", Mode: core.UpsertSetNew},
		{Col: "revision", Mode: core.UpsertSetBump},
	}
	stmt := d.BuildUpsert("u", []string{"id", "email", "revision"}, []string{"id"}, sets)
	if err := q.Exec(ctx, stmt, "x", "first@x.com", 0); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := q.Exec(ctx, stmt, "x", "second@x.com", 0); err != nil {
		t.Fatalf("conflicting upsert: %v", err)
	}
	var email string
	var rev int64
	if err := q.QueryRow(ctx, `SELECT email, revision FROM u WHERE id='x'`).Scan(&email, &rev); err != nil {
		t.Fatal(err)
	}
	if email != "second@x.com" {
		t.Errorf("email = %q, want the excluded (new) value second@x.com", email)
	}
	if rev != 1 {
		t.Errorf("revision = %d, want 1 (bumped)", rev)
	}
}

// TestSavepointTrip proves the WriteTx + savepoint trio: a savepoint, a write,
// a rollback-to leaves the pre-savepoint state, release/commit persists it.
func TestSavepointTrip(t *testing.T) {
	eng := newTestEngine(t)
	ctx := context.Background()
	d := eng.Dialect()
	if err := eng.Querier().Exec(ctx, `CREATE TABLE s (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	tx, err := eng.(core.WriteBeginner).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, `INSERT INTO s VALUES ('keep')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, d.Savepoint("sp1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, `INSERT INTO s VALUES ('drop')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, d.RollbackToSavepoint("sp1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, d.ReleaseSavepoint("sp1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var count int64
	if err := eng.Querier().QueryRow(ctx, `SELECT COUNT(*) FROM s`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1 (only 'keep' survives the rollback-to)", count)
	}
}

// TestOperatorBehavior_LikeVsILike is the behavior test that would have caught
// the OpLike bug: it inserts case-variant rows and proves that Dialect.LikeClause
// is case-SENSITIVE (the case_sensitive_like pragma is the mechanism on SQLite)
// while Dialect.ILikeClause is case-INSENSITIVE — the read-side contract the
// rendering tests alone could not verify.
func TestOperatorBehavior_LikeVsILike(t *testing.T) {
	eng := newTestEngine(t)
	q := eng.Querier()
	ctx := context.Background()
	d := eng.Dialect()

	if err := q.Exec(ctx, `CREATE TABLE people (name TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Alice", "alice", "Bob"} {
		if err := q.Exec(ctx, `INSERT INTO people (name) VALUES (?)`, n); err != nil {
			t.Fatal(err)
		}
	}

	count := func(clause, pattern string) int64 {
		var c int64
		sqlText := `SELECT COUNT(*) FROM people WHERE ` + clause
		if err := q.QueryRow(ctx, sqlText, pattern).Scan(&c); err != nil {
			t.Fatalf("query %q: %v", sqlText, err)
		}
		return c
	}

	// Case-sensitive LIKE: 'a%' matches only "alice", not "Alice".
	if got := count(d.LikeClause(`"name"`, "?"), "a%"); got != 1 {
		t.Errorf("LikeClause 'a%%' matched %d rows, want 1 (case-sensitive)", got)
	}
	// 'A%' matches only "Alice".
	if got := count(d.LikeClause(`"name"`, "?"), "A%"); got != 1 {
		t.Errorf("LikeClause 'A%%' matched %d rows, want 1 (case-sensitive)", got)
	}
	// Case-insensitive ILIKE: 'a%' matches both "Alice" and "alice".
	if got := count(d.ILikeClause(`"name"`, "?"), "a%"); got != 2 {
		t.Errorf("ILikeClause 'a%%' matched %d rows, want 2 (case-insensitive)", got)
	}
}

// TestRebuildLock_Degenerate proves the process-local rebuild lock: the first
// acquire holds it, a second concurrent acquire for the same view sees it held,
// and release frees it.
func TestRebuildLock_Degenerate(t *testing.T) {
	eng := newTestEngine(t)
	ctx := context.Background()

	l1, err := eng.AcquireRebuildLock(ctx, "view_x")
	if err != nil {
		t.Fatal(err)
	}
	if !l1.Acquired() {
		t.Fatal("first acquire should hold the lock")
	}
	l2, err := eng.AcquireRebuildLock(ctx, "view_x")
	if err != nil {
		t.Fatal(err)
	}
	if l2.Acquired() {
		t.Error("second concurrent acquire should NOT hold the lock")
	}
	if l2.Holder() == "" {
		t.Error("a contended lock should report a holder")
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatal(err)
	}
	// After release, a fresh acquire holds it again.
	l3, err := eng.AcquireRebuildLock(ctx, "view_x")
	if err != nil {
		t.Fatal(err)
	}
	if !l3.Acquired() {
		t.Error("acquire after release should hold the lock again")
	}
	_ = l3.Release(ctx)
}
