package write

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The Direct write path: one table, one statement, no outbox and no audit. These
// tests pin the rendered SQL (including the placeholder numbering an UPDATE's
// WHERE has to continue), the refusals, and the exactly-one verbs.

type directJobRow struct {
	ID     domain.ID
	Status string
	Source string
	Owner  domain.ID
}

func directJobSchema() *TableSchema {
	return core.NewDirectSchema[directJobRow]("job_queue").
		ID("id").
		Field("Status", "status").
		Field("Source", "source").
		Field("Owner", "owner_id").
		DeletedAt("deleted_at").
		UpdatedAt("updated_at")
}

// directTestEngine is a fakeRelEngine that can also open a transaction, which is
// what every Direct write runs inside.
type directTestEngine struct {
	fakeRelEngine
	tx *fakeWriteTx
}

func (e *directTestEngine) Begin(context.Context) (WriteTx, error) { return e.tx, nil }

func newDirectWriter(t *testing.T, n int64) (*DirectWriter, *fakeWriteTx) {
	t.Helper()
	tx := &fakeWriteTx{n: n}
	eng := &directTestEngine{tx: tx}
	return NewDirectWriter(eng, directJobSchema(), "ImportJob"), tx
}

func TestDirectWrite_Insert(t *testing.T) {
	w, tx := newDirectWriter(t, 1)
	id, err := w.Insert(context.Background(), Values{"Status": "pending", "Source": "s3://x"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id.Value() == "" {
		t.Fatal("Insert must return the identity the framework minted")
	}
	want := "INSERT INTO job_queue (id, source, status, updated_at) VALUES ($1, $2, $3, $4)"
	if tx.lastSQL != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", tx.lastSQL, want)
	}
}

// The placeholder numbering is the one thing the criteria unification could break
// silently: an UPDATE binds its SET list first, so the WHERE must continue at the
// next number instead of restarting at $1.
func TestDirectWrite_UpdateContinuesThePlaceholderNumbering(t *testing.T) {
	w, tx := newDirectWriter(t, 3)
	n, err := w.Update(context.Background(), Values{"Status": "done"},
		criteria.Where(criteria.Eq("Source", "s3://x")))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n != 3 {
		t.Fatalf("affected = %d, want 3", n)
	}
	want := "UPDATE job_queue SET status = $1, updated_at = $2 WHERE (source = $3 AND deleted_at IS NULL)"
	if tx.lastSQL != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", tx.lastSQL, want)
	}
}

// The archived scope gates a write exactly as it gates a read — and it is a
// criteria node, compiled by the same walk, not a clause spliced in beside it.
func TestDirectWrite_ScopeGate(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		q    *criteria.Query
		want string
	}{
		{"active by default", criteria.Where(criteria.Eq("Status", "x")),
			"DELETE FROM job_queue WHERE (status = $1 AND deleted_at IS NULL)"},
		{"include archived", criteria.Where(criteria.Eq("Status", "x")).IncludeArchived(),
			"DELETE FROM job_queue WHERE status = $1"},
		{"only archived", criteria.Where(criteria.Eq("Status", "x")).OnlyArchived(),
			"DELETE FROM job_queue WHERE (status = $1 AND deleted_at IS NOT NULL)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, tx := newDirectWriter(t, 1)
			if _, err := w.Delete(ctx, c.q); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if tx.lastSQL != c.want {
				t.Fatalf("sql =\n  %q\nwant\n  %q", tx.lastSQL, c.want)
			}
		})
	}
}

// A schema with no DeletedAt is never gated — the same "no column, no gate" rule
// the read path follows.
func TestDirectWrite_NoDeletedAtNoGate(t *testing.T) {
	tx := &fakeWriteTx{n: 1}
	schema := core.NewDirectSchema[directJobRow]("job_queue").ID("id").Field("Status", "status")
	w := NewDirectWriter(&directTestEngine{tx: tx}, schema, "ImportJob")
	if _, err := w.Delete(context.Background(), criteria.Where(criteria.Eq("Status", "x"))); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if tx.lastSQL != "DELETE FROM job_queue WHERE status = $1" {
		t.Fatalf("sql = %q", tx.lastSQL)
	}
}

func TestDirectWrite_ArchiveAndUnarchive(t *testing.T) {
	ctx := context.Background()
	w, tx := newDirectWriter(t, 1)
	if _, err := w.Archive(ctx, criteria.Where(criteria.Eq("Status", "x"))); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if want := "UPDATE job_queue SET deleted_at = $1, updated_at = $2 WHERE (status = $3 AND deleted_at IS NULL)"; tx.lastSQL != want {
		t.Fatalf("archive sql =\n  %q\nwant\n  %q", tx.lastSQL, want)
	}
	if _, err := w.Unarchive(ctx, criteria.Where(criteria.Eq("Status", "x"))); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if want := "UPDATE job_queue SET deleted_at = $1, updated_at = $2 WHERE (status = $3 AND deleted_at IS NOT NULL)"; tx.lastSQL != want {
		t.Fatalf("unarchive sql =\n  %q\nwant\n  %q", tx.lastSQL, want)
	}
	// The verb IS the scope: a criteria that declares one is refused rather than
	// silently overridden.
	if _, err := w.Archive(ctx, criteria.Where(criteria.Eq("Status", "x")).OnlyArchived()); err == nil {
		t.Fatal("Archive must refuse a criteria that declares a scope")
	}
}

func TestDirectWrite_EmptyPredicateIsRefusedOnEveryVerb(t *testing.T) {
	ctx := context.Background()
	w, _ := newDirectWriter(t, 1)
	empty := criteria.Where(nil)
	if _, err := w.Update(ctx, Values{"Status": "x"}, empty); err == nil {
		t.Error("Update with no predicate must be refused")
	}
	if _, err := w.Delete(ctx, empty); err == nil {
		t.Error("Delete with no predicate must be refused")
	}
	if _, err := w.Archive(ctx, empty); err == nil {
		t.Error("Archive with no predicate must be refused")
	}
	if _, err := w.Unarchive(ctx, empty); err == nil {
		t.Error("Unarchive with no predicate must be refused")
	}
	if _, err := w.Delete(ctx, nil); err == nil {
		t.Error("a nil query must be refused too")
	}
}

// The deliberate sweep is a different verb, and it says so at the call site.
func TestDirectWrite_SweepVerbs(t *testing.T) {
	ctx := context.Background()
	w, tx := newDirectWriter(t, 7)
	n, err := w.DeleteAll(ctx)
	if err != nil || n != 7 {
		t.Fatalf("DeleteAll = %d, %v", n, err)
	}
	if tx.lastSQL != "DELETE FROM job_queue" {
		t.Fatalf("sql = %q", tx.lastSQL)
	}
	if _, err := w.UpdateAll(ctx, Values{"Status": "cancelled"}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}
	if want := "UPDATE job_queue SET status = $1, updated_at = $2"; tx.lastSQL != want {
		t.Fatalf("sql = %q, want %q", tx.lastSQL, want)
	}
}

func TestDirectWrite_ExactlyOneVerbs(t *testing.T) {
	ctx := context.Background()
	q := criteria.Where(criteria.Eq("Status", "x"))

	// One row → nil.
	w, _ := newDirectWriter(t, 1)
	if err := w.UpdateOne(ctx, Values{"Status": "y"}, q); err != nil {
		t.Fatalf("one row must succeed: %v", err)
	}

	// Zero rows → the framework's typed RecordNotFound, exactly as an entity
	// write raises when its row is gone.
	w, _ = newDirectWriter(t, 0)
	err := w.UpdateOne(ctx, Values{"Status": "y"}, q)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("zero rows must map to a NotificationCarrier, got %T (%v)", err, err)
	}
	w, _ = newDirectWriter(t, 0)
	if err := w.DeleteOne(ctx, q); !errors.As(err, &carrier) {
		t.Fatalf("DeleteOne zero rows = %T (%v)", err, err)
	}

	// More than one → refused, naming the count, and — the part that matters —
	// the transaction is NEVER committed, so a criteria wider than the caller
	// believed leaves the table exactly as it was.
	w, tx := newDirectWriter(t, 4)
	if err := w.UpdateOne(ctx, Values{"Status": "y"}, q); err == nil || !strings.Contains(err.Error(), "4 rows") {
		t.Fatalf("N rows must be refused naming the count, got %v", err)
	}
	if tx.committed {
		t.Error("a too-wide UpdateOne must NOT commit — the rows it touched have to roll back")
	}
	w, tx = newDirectWriter(t, 4)
	if err := w.DeleteOne(ctx, q); err == nil {
		t.Fatal("a too-wide DeleteOne must be refused")
	}
	if tx.committed {
		t.Error("a too-wide DeleteOne must NOT commit")
	}
	// Zero rows commits nothing either.
	w, tx = newDirectWriter(t, 0)
	_ = w.DeleteOne(ctx, q)
	if tx.committed {
		t.Error("a DeleteOne that matched nothing must not commit")
	}
	// Exactly one DOES commit.
	w, tx = newDirectWriter(t, 1)
	if err := w.DeleteOne(ctx, q); err != nil {
		t.Fatalf("one row: %v", err)
	}
	if !tx.committed {
		t.Error("exactly one row must commit")
	}
}

func TestDirectWrite_ValuesRefusals(t *testing.T) {
	schema := directJobSchema()
	cases := []struct {
		name string
		v    Values
		want string
	}{
		{"empty", Values{}, "at least one value"},
		{"id", Values{"ID": "x"}, "minted by the framework"},
		{"created", Values{"CreatedAt": "x"}, "stamps it"},
		{"updated", Values{"UpdatedAt": "x"}, "stamps it"},
		{"deleted", Values{"DeletedAt": "x"}, "Archive and Unarchive"},
		{"unknown", Values{"Nope": "x"}, "unknown field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolveValues(schema, c.v)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// Values are keyed by GO field name and land on the physical column — the
// three-name model holds on the write side too.
func TestDirectWrite_ValuesResolveGoNamesToColumns(t *testing.T) {
	got, err := resolveValues(directJobSchema(), Values{"Status": "pending", "Owner": "u1"})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	if got["status"] != "pending" || got["owner_id"] != "u1" {
		t.Fatalf("fields = %v, want them keyed by column", got)
	}
}

// Bound to the caller's transaction, a write runs THERE — no transaction of its
// own is opened, so it commits or rolls back with the write that owns it.
func TestDirectWrite_InTxRunsOnTheCallersTransaction(t *testing.T) {
	own := &fakeWriteTx{n: 1}
	callers := &fakeWriteTx{n: 1}
	w := NewDirectWriter(&directTestEngine{tx: own}, directJobSchema(), "ImportJob").BindTx(callers)
	if _, err := w.Insert(context.Background(), Values{"Status": "pending"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if callers.lastSQL == "" {
		t.Error("the statement must run on the caller's transaction")
	}
	if own.lastSQL != "" {
		t.Error("no transaction of its own may be opened when one is bound")
	}
}

// ---------------------------------------------------------------------------
// Branches: refusals, configuration, and what a driver error does
// ---------------------------------------------------------------------------

// An engine that cannot open a transaction fails at CONSTRUCTION: every Direct
// write runs in one, so discovering it on the first request would be too late.
func TestDirectWrite_RefusesAnEngineThatCannotBegin(t *testing.T) {
	defer func() {
		r := recover()
		msg, _ := r.(string)
		if r == nil || !strings.Contains(msg, "cannot open a transaction") {
			t.Fatalf("panic = %v, want it to name the missing capability", r)
		}
	}()
	NewDirectWriter(fakeRelEngine{}, directJobSchema(), "ImportJob")
}

func TestDirectWrite_Configuration(t *testing.T) {
	w, _ := newDirectWriter(t, 1)
	if w.Schema().Table() != "job_queue" {
		t.Errorf("Schema() = %q", w.Schema().Table())
	}
	w.SetContextName("Job")
	w.SetConstraints(map[string]ConstraintBinding{"job_source_uq": {Field: "source"}})
	// A driver error that is not a unique violation passes through untouched.
	boom := errors.New("conn reset")
	if got := w.mapErr(boom); !errors.Is(got, boom) {
		t.Errorf("mapErr = %v, want the driver error unchanged", got)
	}
	if w.mapErr(nil) != nil {
		t.Error("mapErr(nil) must stay nil")
	}
}

// A bad Values map is refused before any statement exists — no transaction is
// opened for a write that cannot be rendered.
func TestDirectWrite_RefusesBadValuesBeforeTouchingTheDatabase(t *testing.T) {
	ctx := context.Background()
	w, tx := newDirectWriter(t, 1)
	if _, err := w.Insert(ctx, Values{"Nope": 1}); err == nil {
		t.Error("Insert with an unknown field must be refused")
	}
	if _, err := w.Update(ctx, Values{"Nope": 1}, criteria.Where(criteria.Eq("Status", "x"))); err == nil {
		t.Error("Update with an unknown field must be refused")
	}
	if _, err := w.UpdateAll(ctx, Values{"Nope": 1}); err == nil {
		t.Error("UpdateAll with an unknown field must be refused")
	}
	if tx.lastSQL != "" {
		t.Errorf("no statement may reach the database, got %q", tx.lastSQL)
	}
}

// A criteria naming a field the schema does not map fails at compilation, with
// the compiler's own message.
func TestDirectWrite_RefusesAnUnknownPredicateField(t *testing.T) {
	w, _ := newDirectWriter(t, 1)
	_, err := w.Delete(context.Background(), criteria.Where(criteria.Eq("Nope", 1)))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want the compiler's unknown-field refusal", err)
	}
}

// Archive on a schema with no DeletedAt is refused: there is no column to stamp.
func TestDirectWrite_ArchiveNeedsTheColumn(t *testing.T) {
	tx := &fakeWriteTx{n: 1}
	schema := core.NewDirectSchema[directJobRow]("job_queue").ID("id").Field("Status", "status")
	w := NewDirectWriter(&directTestEngine{tx: tx}, schema, "ImportJob")
	if _, err := w.Archive(context.Background(), criteria.Where(criteria.Eq("Status", "x"))); err == nil {
		t.Fatal("Archive without a DeletedAt column must be refused")
	}
}

// A driver error rolls the statement back and reaches the caller.
func TestDirectWrite_DriverErrorPropagates(t *testing.T) {
	boom := errors.New("deadlock")
	tx := &fakeWriteTx{execErr: boom}
	w := NewDirectWriter(&directTestEngine{tx: tx}, directJobSchema(), "ImportJob")
	if _, err := w.Delete(context.Background(), criteria.Where(criteria.Eq("Status", "x"))); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver error", err)
	}
	// Bound to a caller's transaction, the same error path applies.
	if _, err := w.BindTx(tx).Delete(context.Background(), criteria.Where(criteria.Eq("Status", "x"))); !errors.Is(err, boom) {
		t.Fatalf("bound err = %v, want the driver error", err)
	}
}

// A transaction that cannot be opened, or cannot commit, reaches the caller —
// nothing is reported as written that was not.
func TestDirectWrite_TransactionFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("pool exhausted")

	w := NewDirectWriter(&directTestEngine{tx: &fakeWriteTx{n: 1}}, directJobSchema(), "ImportJob")
	w.beginner = failingBeginner{err: boom}
	if _, err := w.Delete(ctx, criteria.Where(criteria.Eq("Status", "x"))); !errors.Is(err, boom) {
		t.Fatalf("begin error = %v, want it propagated", err)
	}
	if err := w.DeleteOne(ctx, criteria.Where(criteria.Eq("Status", "x"))); !errors.Is(err, boom) {
		t.Fatalf("DeleteOne begin error = %v, want it propagated", err)
	}
}

type failingBeginner struct{ err error }

func (b failingBeginner) Begin(context.Context) (WriteTx, error) { return nil, b.err }
