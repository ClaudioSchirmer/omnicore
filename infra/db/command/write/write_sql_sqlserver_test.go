package write

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// testSQLServerDialect is the @pN-placeholder, bracket-quoting, BINARY(16)
// sibling of testPGDialect/testMySQLDialect for the shared write builders.
// These tests lock the third rendering the one set of builders must produce:
// bracket identifiers, numbered @pN placeholders, CURRENT_TIMESTAMP as the
// dialect's now expression (the archive stamp and the managed timestamp columns
// must ride Dialect.NowExpr — a baked-in NOW() would not parse on SQL Server),
// and the BINARY(16) id encoding.
type testSQLServerDialect struct{}

func (testSQLServerDialect) Placeholder(n int) string { return fmt.Sprintf("@p%d", n) }
func (testSQLServerDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return "[" + name + "]"
}
func (testSQLServerDialect) EncodeArg(val any) any {
	switch v := val.(type) {
	case domain.ID:
		if u, err := uuid.Parse(v.Value()); err == nil {
			return u[:]
		}
		return v.Value()
	case *domain.ID:
		if v == nil {
			return nil
		}
		if u, err := uuid.Parse(v.Value()); err == nil {
			return u[:]
		}
		return v.Value()
	case uuid.UUID:
		return v[:]
	default:
		return val
	}
}
func (testSQLServerDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (testSQLServerDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}
func (testSQLServerDialect) NowExpr() string { return "CURRENT_TIMESTAMP" }
func (testSQLServerDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("SELECT TOP %d %s", n, sql[len("SELECT "):])
}
func (testSQLServerDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (testSQLServerDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (testSQLServerDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "MERGE " + table
}

// TestBuildInsert_SQLServer proves the managed timestamp columns render the
// DIALECT's now expression — the whole point of the NowExpr seam: the same
// builder that emits NOW() on PG/MySQL must emit CURRENT_TIMESTAMP here.
func TestBuildInsert_SQLServer(t *testing.T) {
	fields := domain.Fields{"name": "alice", "email": "a@x"}
	id := "11111111-1111-1111-1111-111111111111"
	sql, args := buildInsert(testSQLServerDialect{}, "users", "id", id, fields, []string{"created_at", "updated_at"})

	want := "INSERT INTO [users] ([id], [email], [name], [created_at], [updated_at]) VALUES (@p1, @p2, @p3, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	if len(args) != 3 {
		t.Fatalf("args = %v, want 3 (id + email + name)", args)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("args[0] = %v (%T), want the id as a 16-byte BINARY(16) form", args[0], args[0])
	}
	if args[1] != "a@x" || args[2] != "alice" {
		t.Errorf("field args = %v, want [a@x alice] in that exact order", args[1:])
	}
}

func TestBuildUpdate_SQLServer(t *testing.T) {
	fields := domain.Fields{"name": "bob", "email": "b@x"}
	id := "22222222-2222-2222-2222-222222222222"
	sql, args := buildUpdate(testSQLServerDialect{}, "users", "id", id, fields, []string{"updated_at"})

	want := "UPDATE [users] SET [email] = @p1, [name] = @p2, [updated_at] = CURRENT_TIMESTAMP WHERE [id] = @p3"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	if len(args) != 3 || args[0] != "b@x" || args[1] != "bob" {
		t.Fatalf("SET args = %v, want [b@x bob ...] in that order", args)
	}
	b, ok := args[2].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("WHERE id arg = %v (%T), want a 16-byte BINARY(16) form", args[2], args[2])
	}
}

// TestArchiveUnarchiveDelete_SQLServer: the archive stamp is the dialect's now
// expression (CURRENT_TIMESTAMP), never a baked-in NOW().
func TestArchiveUnarchiveDelete_SQLServer(t *testing.T) {
	d := testSQLServerDialect{}
	if got := archiveSQL(d, "users", "deleted_at", "id"); got != "UPDATE [users] SET [deleted_at] = CURRENT_TIMESTAMP WHERE [id] = @p1" {
		t.Errorf("archiveSQL = %q", got)
	}
	if got := unarchiveSQL(d, "users", "deleted_at", "id"); got != "UPDATE [users] SET [deleted_at] = NULL WHERE [id] = @p1" {
		t.Errorf("unarchiveSQL = %q", got)
	}
	if got := deleteSQL(d, "users", "id"); got != "DELETE FROM [users] WHERE [id] = @p1" {
		t.Errorf("deleteSQL = %q", got)
	}
	if got := childDeleteSQL(d, "addresses", "user_id"); got != "DELETE FROM [addresses] WHERE [user_id] = @p1" {
		t.Errorf("childDeleteSQL = %q", got)
	}
}

// TestChildCascadeSQL_SQLServer: the cascade's setExpr arrives from the caller
// already resolved against the dialect (nowSetExpr → CURRENT_TIMESTAMP here).
func TestChildCascadeSQL_SQLServer(t *testing.T) {
	d := testSQLServerDialect{}
	archive := childCascadeSQL(d, "addresses", "deleted_at", "user_id", nowSetExpr(d), " IS NULL")
	if archive != "UPDATE [addresses] SET [deleted_at] = CURRENT_TIMESTAMP WHERE [user_id] = @p1 AND [deleted_at] IS NULL" {
		t.Errorf("archive cascade = %q", archive)
	}
	unarchive := childCascadeSQL(d, "addresses", "deleted_at", "user_id", nullSetExpr(d), " IS NOT NULL")
	if unarchive != "UPDATE [addresses] SET [deleted_at] = NULL WHERE [user_id] = @p1 AND [deleted_at] IS NOT NULL" {
		t.Errorf("unarchive cascade = %q", unarchive)
	}
}

// TestBuildInsert_SQLServer_TypedIDFields proves the write half of the
// type-driven identity contract on the third dialect: a domain.ID field value
// binds as its 16-byte form, a nil *domain.ID binds SQL NULL, and a plain
// string — canonical uuid shape included — binds as text (a string field pairs
// with an NVARCHAR column).
func TestBuildInsert_SQLServer_TypedIDFields(t *testing.T) {
	buyer := domain.NewID("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	var absent *domain.ID
	fields := domain.Fields{
		"buyer_id":   buyer,
		"absent_id":  absent,
		"legacy_ref": "018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d",
	}
	id := "11111111-1111-1111-1111-111111111111"
	_, args := buildInsert(testSQLServerDialect{}, "orders", "id", id, fields, nil)

	// Bind order: PK, then SortedKeys (absent_id, buyer_id, legacy_ref).
	if len(args) != 4 {
		t.Fatalf("args = %v, want 4", args)
	}
	if args[1] != nil {
		t.Errorf("absent_id arg = %v (%T), want nil (SQL NULL)", args[1], args[1])
	}
	if b, ok := args[2].([]byte); !ok || len(b) != 16 {
		t.Errorf("buyer_id arg = %v (%T), want 16 bytes (domain.ID)", args[2], args[2])
	}
	if got, ok := args[3].(string); !ok || got != "018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d" {
		t.Errorf("legacy_ref arg = %v (%T), want the untouched text (string field)", args[3], args[3])
	}
}
