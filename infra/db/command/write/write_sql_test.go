package write

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// testNow is the fixed operation stamp the builder tests bind — asserting the
// managed timestamps travel as ordinary args (app-clock authored), never as a
// dialect NOW() expression.
var testNow = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

// The shared write builders render one statement for every backend via the
// Dialect (placeholders, identifier quoting, arg encoding). These white-box
// tests drive them with testPGDialect ($n placeholders, bare idents, domain.ID →
// string); the per-dialect encoding (BINARY(16) on MySQL) is the Dialect's own
// contract, exercised by the MySQL integration suite. The verb orchestration
// (flat_write.go / aggregate_write.go) is covered end-to-end by the integration
// + e2e suites against real databases.

func TestBuildInsert_Shared(t *testing.T) {
	fields := domain.Fields{"name": "alice", "email": "a@x"}
	id := "11111111-1111-1111-1111-111111111111"
	sql, args := buildInsert(testPGDialect{}, "users", "id", id, fields, []string{"created_at", "updated_at"}, testNow, "")

	want := "INSERT INTO users (id, email, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	// ID is prepended and encoded (domain.ID → string under testPGDialect);
	// remaining args follow SortedKeys order (email, name); the managed
	// timestamp columns bind the operation stamp.
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5 (id + email + name + created_at + updated_at)", args)
	}
	if args[0] != id {
		t.Errorf("args[0] = %v, want the encoded id %q", args[0], id)
	}
	if args[1] != "a@x" || args[2] != "alice" {
		t.Errorf("field args = %v, want [a@x alice]", args[1:3])
	}
	if args[3] != testNow || args[4] != testNow {
		t.Errorf("timestamp args = %v, want the operation stamp twice", args[3:])
	}
}

func TestBuildUpdate_Shared(t *testing.T) {
	fields := domain.Fields{"name": "bob", "email": "b@x"}
	id := "22222222-2222-2222-2222-222222222222"
	sql, args := buildUpdate(testPGDialect{}, "users", "id", id, fields, []string{"updated_at"}, testNow, "", 0)

	want := "UPDATE users SET email = $1, name = $2, updated_at = $3 WHERE id = $4"
	if sql != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", sql, want)
	}
	if len(args) != 4 || args[0] != "b@x" || args[1] != "bob" || args[2] != testNow || args[3] != id {
		t.Fatalf("args = %v, want [b@x bob %v %s]", args, testNow, id)
	}
}

func TestWriteNow_UTCMicrosecond(t *testing.T) {
	now := writeNow()
	if now.Location() != time.UTC {
		t.Errorf("writeNow() location = %v, want UTC", now.Location())
	}
	if now.Truncate(time.Microsecond) != now {
		t.Errorf("writeNow() = %v, want microsecond-truncated", now)
	}
}

func TestArchiveUnarchiveDelete_SQL(t *testing.T) {
	d := testPGDialect{}
	if got := archiveSQL(d, "users", "deleted_at", "id", ""); got != "UPDATE users SET deleted_at = $1 WHERE id = $2" {
		t.Errorf("archiveSQL = %q", got)
	}
	if got := deleteSQL(d, "users", "id"); got != "DELETE FROM users WHERE id = $1" {
		t.Errorf("deleteSQL = %q", got)
	}
	if got := childDeleteSQL(d, "addresses", "user_id"); got != "DELETE FROM addresses WHERE user_id = $1" {
		t.Errorf("childDeleteSQL = %q", got)
	}
}

// TestBuildSiblingUpsert_ArgsOrder_PG locks the positional bind order of the
// sibling upsert: the shared ID first, then field values in SortedKeys order.
func TestBuildSiblingUpsert_ArgsOrder_PG(t *testing.T) {
	sib := NewSiblingSchema[*sibTestEntity]("usuario").Field("UserName", "user_name")
	id := "33333333-3333-3333-3333-333333333333"
	_, args := buildSiblingUpsert(testPGDialect{}, sib, "id", id, domain.Fields{"user_name": "alice"})
	if len(args) != 2 || args[0] != id || args[1] != "alice" {
		t.Fatalf("args = %v, want [%s alice] (pk first, then SortedKeys)", args, id)
	}
}

func TestChildCascadeSQL_Shared(t *testing.T) {
	d := testPGDialect{}
	archive := archiveCascadeSQL(d, "addresses", "deleted_at", "user_id")
	if archive != "UPDATE addresses SET deleted_at = $1 WHERE user_id = $2 AND deleted_at IS NULL" {
		t.Errorf("archive cascade = %q", archive)
	}
	unarchive := unarchiveCascadeSQL(d, "addresses", "deleted_at", "user_id")
	if unarchive != "UPDATE addresses SET deleted_at = NULL WHERE user_id = $1 AND deleted_at = $2" {
		t.Errorf("unarchive cascade = %q", unarchive)
	}
}

// fakeWriteTx is a minimal db.WriteTx that records the last statement and returns
// a programmable rows-affected count — enough to drive execExpectingRow's 404
// mapping without a live database.
type fakeWriteTx struct {
	n       int64
	execErr error
	lastSQL string
}

func (t *fakeWriteTx) Exec(_ context.Context, sql string, _ ...any) error {
	t.lastSQL = sql
	return t.execErr
}
func (t *fakeWriteTx) ExecCount(_ context.Context, sql string, _ ...any) (int64, error) {
	t.lastSQL = sql
	if t.execErr != nil {
		return 0, t.execErr
	}
	return t.n, nil
}
func (t *fakeWriteTx) Query(context.Context, string, ...any) (Rows, error) { return nil, nil }
func (t *fakeWriteTx) QueryRow(context.Context, string, ...any) Row        { return nil }
func (t *fakeWriteTx) Commit(context.Context) error                        { return nil }
func (t *fakeWriteTx) Rollback(context.Context) error                      { return nil }
func (t *fakeWriteTx) Handle() persistence.TxHandle                        { return nil }
func (t *fakeWriteTx) Dialect() Dialect                                    { return testPGDialect{} }

func TestExecExpectingRow_Mapping(t *testing.T) {
	ctx := context.Background()

	// Matched row → nil.
	if err := execExpectingRow(ctx, &fakeWriteTx{n: 1}, testPGDialect{}, "UPDATE x", nil, "users", "User", "id", "v", 0); err != nil {
		t.Fatalf("matched row should be nil, got %v", err)
	}

	// Zero rows → RecordNotFoundNotification (a NotificationCarrier, 404).
	err := execExpectingRow(ctx, &fakeWriteTx{n: 0}, testPGDialect{}, "UPDATE x", nil, "users", "User", "id", "v", 0)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("zero rows should map to a NotificationCarrier, got %T (%v)", err, err)
	}

	// Driver error passes through unchanged.
	boom := errors.New("conn reset")
	if err := execExpectingRow(ctx, &fakeWriteTx{execErr: boom}, testPGDialect{}, "UPDATE x", nil, "users", "User", "id", "v", 0); !errors.Is(err, boom) {
		t.Fatalf("driver error should pass through, got %v", err)
	}
}
