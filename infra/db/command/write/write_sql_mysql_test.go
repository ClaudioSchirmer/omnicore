package write

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// testMySQLDialect is the `?`-placeholder, backtick-quoting, BINARY(16) twin of
// testPGDialect for the shared write builders. The builders render one statement
// per backend through the Dialect; testPGDialect proves the `$n` form, but the
// MySQL `?` form is POSITIONAL — the args slice order is the only thing binding a
// value to a column, so a builder that emitted columns and args in different
// orders would silently misbind on MySQL while passing every PG test. These
// tests lock the MySQL rendering AND the positional arg order.
type testMySQLDialect struct{}

func (testMySQLDialect) Placeholder(int) string { return "?" } // positional — index unused
func (testMySQLDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return "`" + name + "`"
}
func (testMySQLDialect) EncodeArg(val any) any {
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
func (testMySQLDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (testMySQLDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}
func (testMySQLDialect) LikeClause(col, ph string) string {
	return "BINARY " + col + " LIKE " + ph
}
func (testMySQLDialect) NowExpr() string { return "NOW()" }
func (testMySQLDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}
func (testMySQLDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", sql, limit, offset)
}
func (testMySQLDialect) Savepoint(name string) string { return "SAVEPOINT " + name }
func (testMySQLDialect) RollbackToSavepoint(name string) string {
	return "ROLLBACK TO SAVEPOINT " + name
}
func (testMySQLDialect) ReleaseSavepoint(name string) string        { return "RELEASE SAVEPOINT " + name }
func (testMySQLDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (testMySQLDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (testMySQLDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "INSERT " + table
}

func TestBuildInsert_MySQL(t *testing.T) {
	fields := domain.Fields{"name": "alice", "email": "a@x"}
	id := "11111111-1111-1111-1111-111111111111"
	sql, args := buildInsert(testMySQLDialect{}, "users", "id", id, fields, []string{"created_at", "updated_at"}, testNow, "")

	want := "INSERT INTO `users` (`id`, `email`, `name`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?)"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	// Positional bind order: ID first (BINARY(16)-encoded on MySQL), then the
	// SortedKeys field order (email, name). NOW() columns bind no args.
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5 (id + email + name + created_at + updated_at)", args)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("args[0] = %v (%T), want the id as a 16-byte BINARY(16) form", args[0], args[0])
	}
	if args[1] != "a@x" || args[2] != "alice" {
		t.Errorf("field args = %v, want [a@x alice] in that exact order", args[1:3])
	}
	if args[3] != testNow || args[4] != testNow {
		t.Errorf("timestamp args = %v, want the operation stamp twice", args[3:])
	}
}

func TestBuildUpdate_MySQL(t *testing.T) {
	fields := domain.Fields{"name": "bob", "email": "b@x"}
	id := "22222222-2222-2222-2222-222222222222"
	sql, args := buildUpdate(testMySQLDialect{}, "users", "id", id, fields, []string{"updated_at"}, testNow, "", 0)

	want := "UPDATE `users` SET `email` = ?, `name` = ?, `updated_at` = ? WHERE `id` = ?"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	// SET args in SortedKeys order, then the stamp, then the WHERE id last
	// (BINARY(16)-encoded).
	if len(args) != 4 || args[0] != "b@x" || args[1] != "bob" || args[2] != testNow {
		t.Fatalf("SET args = %v, want [b@x bob %v ...] in that order", args, testNow)
	}
	b, ok := args[3].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("WHERE id arg = %v (%T), want a 16-byte BINARY(16) form", args[2], args[2])
	}
}

func TestArchiveUnarchiveDelete_MySQL(t *testing.T) {
	d := testMySQLDialect{}
	if got := archiveSQL(d, "users", "deleted_at", "id", ""); got != "UPDATE `users` SET `deleted_at` = ? WHERE `id` = ?" {
		t.Errorf("archiveSQL = %q", got)
	}
	if got := deleteSQL(d, "users", "id"); got != "DELETE FROM `users` WHERE `id` = ?" {
		t.Errorf("deleteSQL = %q", got)
	}
	if got := childDeleteSQL(d, "addresses", "user_id"); got != "DELETE FROM `addresses` WHERE `user_id` = ?" {
		t.Errorf("childDeleteSQL = %q", got)
	}
}

// TestBuildSiblingUpsert_ArgsOrder_MySQL locks the MySQL positional bind: the
// `?` placeholders mean arg ORDER is the only thing binding a value to a column,
// so the shared ID must be encoded BINARY(16) first, then field values.
func TestBuildSiblingUpsert_ArgsOrder_MySQL(t *testing.T) {
	sib := NewSiblingSchema[*sibTestEntity]("usuario").Field("UserName", "user_name")
	id := "33333333-3333-3333-3333-333333333333"
	_, args := buildSiblingUpsert(testMySQLDialect{}, sib, "id", id, domain.Fields{"user_name": "alice"})
	if len(args) != 2 {
		t.Fatalf("args = %v, want 2 (pk + user_name)", args)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("args[0] = %v (%T), want the shared ID as 16-byte BINARY(16)", args[0], args[0])
	}
	if args[1] != "alice" {
		t.Errorf("args[1] = %v, want \"alice\"", args[1])
	}
}

func TestChildCascadeSQL_MySQL(t *testing.T) {
	d := testMySQLDialect{}
	archive := archiveCascadeSQL(d, "addresses", "deleted_at", "user_id")
	if archive != "UPDATE `addresses` SET `deleted_at` = ? WHERE `user_id` = ? AND `deleted_at` IS NULL" {
		t.Errorf("archive cascade = %q", archive)
	}
	unarchive := unarchiveCascadeSQL(d, "addresses", "deleted_at", "user_id", "users", "deleted_at", "id")
	if unarchive != "UPDATE `addresses` SET `deleted_at` = NULL WHERE `user_id` = ? AND `deleted_at` = (SELECT `deleted_at` FROM `users` WHERE `id` = ?)" {
		t.Errorf("unarchive cascade = %q", unarchive)
	}
}

// TestBuildInsert_MySQL_TypedIDFields proves the write half of the type-driven
// identity contract: a domain.ID field value binds as its 16-byte form, a nil
// *domain.ID binds SQL NULL, a non-nil *domain.ID binds 16 bytes — and a plain
// string, canonical uuid shape included, binds as text (a string field pairs
// with a CHAR/VARCHAR column). The field TYPE is the declaration; buildInsert
// itself needs no schema knowledge because the VALUE carries the type.
func TestBuildInsert_MySQL_TypedIDFields(t *testing.T) {
	buyer := domain.NewID("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	partner := domain.NewID("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0e")
	var absent *domain.ID
	fields := domain.Fields{
		"buyer_id":   buyer,                                  // domain.ID → 16 bytes
		"partner_id": &partner,                               // *domain.ID → 16 bytes
		"absent_id":  absent,                                 // nil *domain.ID → SQL NULL
		"legacy_ref": "018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d", // string → text, always
	}
	id := "11111111-1111-1111-1111-111111111111"
	_, args := buildInsert(testMySQLDialect{}, "orders", "id", id, fields, nil, testNow, "")

	// Bind order: ID, then SortedKeys (absent_id, buyer_id, legacy_ref, partner_id).
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5", args)
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
	if b, ok := args[4].([]byte); !ok || len(b) != 16 {
		t.Errorf("partner_id arg = %v (%T), want 16 bytes (*domain.ID)", args[4], args[4])
	}
}
