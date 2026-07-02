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
	case uuid.UUID:
		return v[:]
	case string:
		if len(v) == 36 {
			if u, err := uuid.Parse(v); err == nil {
				return u[:]
			}
		}
		return v
	default:
		return val
	}
}
func (testMySQLDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (testMySQLDialect) ILikeClause(col, ph string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + ph + ")"
}
func (testMySQLDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (testMySQLDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (testMySQLDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "INSERT " + table
}

func TestBuildInsert_MySQL(t *testing.T) {
	fields := domain.Fields{"name": "alice", "email": "a@x"}
	id := "11111111-1111-1111-1111-111111111111"
	sql, args := buildInsert(testMySQLDialect{}, "users", "id", id, fields, []string{"created_at", "updated_at"})

	want := "INSERT INTO `users` (`id`, `email`, `name`, `created_at`, `updated_at`) VALUES (?, ?, ?, NOW(), NOW())"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	// Positional bind order: PK first (BINARY(16)-encoded on MySQL), then the
	// SortedKeys field order (email, name). NOW() columns bind no args.
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

func TestBuildUpdate_MySQL(t *testing.T) {
	fields := domain.Fields{"name": "bob", "email": "b@x"}
	id := "22222222-2222-2222-2222-222222222222"
	sql, args := buildUpdate(testMySQLDialect{}, "users", "id", id, fields, []string{"updated_at"})

	want := "UPDATE `users` SET `email` = ?, `name` = ?, `updated_at` = NOW() WHERE `id` = ?"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	// SET args in SortedKeys order, then the WHERE id last (BINARY(16)-encoded).
	if len(args) != 3 || args[0] != "b@x" || args[1] != "bob" {
		t.Fatalf("SET args = %v, want [b@x bob ...] in that order", args)
	}
	b, ok := args[2].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("WHERE id arg = %v (%T), want a 16-byte BINARY(16) form", args[2], args[2])
	}
}

func TestArchiveUnarchiveDelete_MySQL(t *testing.T) {
	d := testMySQLDialect{}
	if got := archiveSQL(d, "users", "deleted_at", "id"); got != "UPDATE `users` SET `deleted_at` = NOW() WHERE `id` = ?" {
		t.Errorf("archiveSQL = %q", got)
	}
	if got := unarchiveSQL(d, "users", "deleted_at", "id"); got != "UPDATE `users` SET `deleted_at` = NULL WHERE `id` = ?" {
		t.Errorf("unarchiveSQL = %q", got)
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
// so the shared PK must be encoded BINARY(16) first, then field values.
func TestBuildSiblingUpsert_ArgsOrder_MySQL(t *testing.T) {
	sib := NewSiblingSchema[*sibTestEntity]("usuario").Field("UserName", "user_name")
	id := "33333333-3333-3333-3333-333333333333"
	_, args := buildSiblingUpsert(testMySQLDialect{}, sib, "id", id, domain.Fields{"user_name": "alice"})
	if len(args) != 2 {
		t.Fatalf("args = %v, want 2 (pk + user_name)", args)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Errorf("args[0] = %v (%T), want the shared PK as 16-byte BINARY(16)", args[0], args[0])
	}
	if args[1] != "alice" {
		t.Errorf("args[1] = %v, want \"alice\"", args[1])
	}
}

func TestChildCascadeSQL_MySQL(t *testing.T) {
	d := testMySQLDialect{}
	archive := childCascadeSQL(d, "addresses", "deleted_at", "user_id", "NOW()", " IS NULL")
	if archive != "UPDATE `addresses` SET `deleted_at` = NOW() WHERE `user_id` = ? AND `deleted_at` IS NULL" {
		t.Errorf("archive cascade = %q", archive)
	}
	unarchive := childCascadeSQL(d, "addresses", "deleted_at", "user_id", "NULL", " IS NOT NULL")
	if unarchive != "UPDATE `addresses` SET `deleted_at` = NULL WHERE `user_id` = ? AND `deleted_at` IS NOT NULL" {
		t.Errorf("unarchive cascade = %q", unarchive)
	}
}
