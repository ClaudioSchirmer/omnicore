package write

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// testOracleDialect is the :n-placeholder, quoted-uppercase, RAW(16) sibling
// of testPGDialect/testMySQLDialect/testSQLServerDialect for the shared write
// builders. These tests lock the fourth rendering the one set of builders must
// produce: QUOTED-UPPERCASE identifiers (equivalent to the catalog's unquoted
// uppercase folding, and reserved-word safe), numbered :n placeholders,
// SYSTIMESTAMP as the dialect's now expression (the archive stamp and the
// managed timestamp columns must ride Dialect.NowExpr — a baked-in NOW()
// would not parse on Oracle), and the RAW(16) id encoding.
type testOracleDialect struct{}

func (testOracleDialect) Placeholder(n int) string { return fmt.Sprintf(":%d", n) }
func (testOracleDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return `"` + strings.ToUpper(name) + `"`
}
func (testOracleDialect) EncodeArg(val any) any {
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
func (testOracleDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (testOracleDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}
func (testOracleDialect) LikeClause(col, ph string) string {
	return col + " LIKE " + ph
}
func (testOracleDialect) NowExpr() string { return "SYSTIMESTAMP" }
func (testOracleDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s FETCH FIRST %d ROWS ONLY", sql, n)
}
func (testOracleDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", sql, offset, limit)
}
func (testOracleDialect) Savepoint(name string) string { return "SAVEPOINT " + name }
func (testOracleDialect) RollbackToSavepoint(name string) string {
	return "ROLLBACK TO SAVEPOINT " + name
}
func (testOracleDialect) ReleaseSavepoint(string) string             { return "" }
func (testOracleDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (testOracleDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (testOracleDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "MERGE INTO " + table
}

// TestBuildInsert_Oracle proves the managed timestamp columns render the
// DIALECT's now expression — the whole point of the NowExpr seam: the same
// builder that emits NOW() on PG/MySQL must emit SYSTIMESTAMP here.
func TestBuildInsert_Oracle(t *testing.T) {
	fields := domain.Fields{"name": "alice", "email": "a@x"}
	id := "11111111-1111-1111-1111-111111111111"
	sql, args := buildInsert(testOracleDialect{}, "users", "id", id, fields, []string{"created_at", "updated_at"}, testNow, "")

	want := `INSERT INTO "USERS" ("ID", "EMAIL", "NAME", "CREATED_AT", "UPDATED_AT") VALUES (:1, :2, :3, :4, :5)`
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5 (id + email + name + created_at + updated_at)", args)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("args[0] = %v (%T), want the id as a 16-byte RAW(16) form", args[0], args[0])
	}
	if args[1] != "a@x" || args[2] != "alice" {
		t.Errorf("field args = %v, want [a@x alice] in that exact order", args[1:])
	}
}

func TestBuildUpdate_Oracle(t *testing.T) {
	fields := domain.Fields{"name": "bob", "email": "b@x"}
	id := "22222222-2222-2222-2222-222222222222"
	sql, args := buildUpdate(testOracleDialect{}, "users", "id", id, fields, []string{"updated_at"}, testNow, "", 0)

	want := `UPDATE "USERS" SET "EMAIL" = :1, "NAME" = :2, "UPDATED_AT" = :3 WHERE "ID" = :4`
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	if len(args) != 4 || args[0] != "b@x" || args[1] != "bob" || args[2] != testNow {
		t.Fatalf("SET args = %v, want [b@x bob ...] in that order", args)
	}
	b, ok := args[3].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("WHERE id arg = %v (%T), want a 16-byte RAW(16) form", args[3], args[3])
	}
}

// TestArchiveUnarchiveDelete_Oracle: the archive stamp is the dialect's now
// expression (SYSTIMESTAMP), never a baked-in NOW().
func TestArchiveUnarchiveDelete_Oracle(t *testing.T) {
	d := testOracleDialect{}
	if got := archiveSQL(d, "users", "deleted_at", "id", ""); got != `UPDATE "USERS" SET "DELETED_AT" = :1 WHERE "ID" = :2` {
		t.Errorf("archiveSQL = %q", got)
	}
	if got := deleteSQL(d, "users", "id"); got != `DELETE FROM "USERS" WHERE "ID" = :1` {
		t.Errorf("deleteSQL = %q", got)
	}
	if got := childDeleteSQL(d, "addresses", "user_id"); got != `DELETE FROM "ADDRESSES" WHERE "USER_ID" = :1` {
		t.Errorf("childDeleteSQL = %q", got)
	}
}

// TestChildCascadeSQL_Oracle: both directions of the symmetric cascade, each
// binding the operation's own instant — written by the archive, matched by the
// restore.
func TestChildCascadeSQL_Oracle(t *testing.T) {
	d := testOracleDialect{}
	archive := archiveCascadeSQL(d, "addresses", "deleted_at", "user_id")
	if archive != `UPDATE "ADDRESSES" SET "DELETED_AT" = :1 WHERE "USER_ID" = :2 AND "DELETED_AT" IS NULL` {
		t.Errorf("archive cascade = %q", archive)
	}
	unarchive := unarchiveCascadeSQL(d, "addresses", "deleted_at", "user_id")
	if unarchive != `UPDATE "ADDRESSES" SET "DELETED_AT" = NULL WHERE "USER_ID" = :1 AND "DELETED_AT" = :2` {
		t.Errorf("unarchive cascade = %q", unarchive)
	}
}

// TestBuildInsert_Oracle_TypedIDFields proves the write half of the
// type-driven identity contract on the fourth dialect: a domain.ID field value
// binds as its 16-byte form, a nil *domain.ID binds SQL NULL, and a plain
// string — canonical uuid shape included — binds as text (a string field pairs
// with a VARCHAR2 column).
func TestBuildInsert_Oracle_TypedIDFields(t *testing.T) {
	buyer := domain.NewID("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	var absent *domain.ID
	fields := domain.Fields{
		"buyer_id":   buyer,
		"absent_id":  absent,
		"legacy_ref": "018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d",
	}
	id := "11111111-1111-1111-1111-111111111111"
	_, args := buildInsert(testOracleDialect{}, "orders", "id", id, fields, nil, testNow, "")

	// Bind order: ID, then SortedKeys (absent_id, buyer_id, legacy_ref).
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
