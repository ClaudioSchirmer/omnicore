package read

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The Direct repository: one table, no aggregate. These tests pin what it
// refuses to be built on, the statement its reads issue (joins included), and
// the fact that FindOne means exactly one.

type directJob struct {
	ID      domain.ID
	Status  string
	OwnerID domain.ID

	// Filled by a declared join, never persisted.
	OwnerName string
}

type directOwner struct {
	ID   domain.ID
	Name string
}

func directJobTable() *TableSchema {
	return core.NewDirectSchema[directJob]("job_queue").
		ID("id").
		Field("Status", "status").
		Field("OwnerID", "owner_id").
		DeletedAt("deleted_at")
}

func directOwnerTable() *TableSchema {
	return core.NewTableSchema[directOwner]("users").ID("id").Field("Name", "name").AsDirectSchema()
}

type directReadEngine struct {
	fakeRelEngine
	tx *directNoopTx
}

func (e *directReadEngine) Begin(context.Context) (core.WriteTx, error) { return e.tx, nil }

type directNoopTx struct{}

func (directNoopTx) Exec(context.Context, string, ...any) error { return nil }
func (directNoopTx) ExecCount(context.Context, string, ...any) (int64, error) {
	return 1, nil
}
func (directNoopTx) Query(context.Context, string, ...any) (Rows, error) { return &fakeDBRows{}, nil }
func (directNoopTx) QueryRow(context.Context, string, ...any) Row        { return fakeDBRow{} }
func (directNoopTx) Commit(context.Context) error                        { return nil }
func (directNoopTx) Rollback(context.Context) error                      { return nil }
func (directNoopTx) Handle() persistence.TxHandle                        { return nil }
func (directNoopTx) Dialect() Dialect                                    { return testPGDialect{} }

func directRepo(t *testing.T, q Querier) *DirectRepository[directJob] {
	t.Helper()
	return NewDirectRepository[directJob](&directReadEngine{fakeRelEngine: fakeRelEngine{q: q}, tx: &directNoopTx{}}, directJobTable())
}

func TestDirectRepository_RefusesWhatItCannotAnchorOn(t *testing.T) {
	eng := &directReadEngine{tx: &directNoopTx{}}
	cases := []struct {
		name  string
		want  string
		build func()
	}{
		{"an entity schema", "was not built with core.NewDirectSchema", func() {
			// Built here rather than from directOwnerTable(), which now hands over
			// the reduced form a join target has to be.
			NewDirectRepository[directOwner](eng,
				core.NewTableSchema[directOwner]("users").ID("id").Field("Name", "name"))
		}},
		{"no primary key", "declares no primary key", func() {
			NewDirectRepository[directJob](eng, core.NewDirectSchema[directJob]("t").Field("Status", "status"))
		}},
		{"a schema for another row type", "is anchored to", func() {
			NewDirectRepository[directOwner](eng, directJobTable())
		}},
		{"a nil schema", "the schema is mandatory", func() {
			NewDirectRepository[directJob](eng, nil)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected a panic mentioning %q", c.want)
				}
				if msg, _ := r.(string); !strings.Contains(msg, c.want) {
					t.Fatalf("panic = %q, want it to mention %q", msg, c.want)
				}
			}()
			c.build()
		})
	}
}

func TestDirectRepository_FindAllStatement(t *testing.T) {
	var got string
	repo := directRepo(t, fakeQuerier{queryFn: func(sql string, _ []any) (Rows, error) {
		got = sql
		return &fakeDBRows{}, nil
	}})
	if _, err := repo.FindAll(context.Background(), criteria.
		Where(criteria.Eq("Status", "pending")).OrderBy("Status").Limit(10)); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	want := "SELECT id, status, owner_id FROM job_queue WHERE status = $1 AND deleted_at IS NULL ORDER BY status ASC LIMIT 10"
	if got != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", got, want)
	}
}

// A declared join is always in the FROM, its column rides the same SELECT, and
// every anchor-side column is qualified — a joined aggregate is a foreign
// namespace free to carry a "status" of its own.
func TestDirectRepository_FindAllWithAJoin(t *testing.T) {
	var got string
	repo := directRepo(t, fakeQuerier{queryFn: func(sql string, _ []any) (Rows, error) {
		got = sql
		return &fakeDBRows{}, nil
	}}).WithJoins(InnerJoin(directOwnerTable()).On("owner_id").Field("OwnerName", "name"))

	if _, err := repo.FindAll(context.Background(), criteria.Where(criteria.Eq("OwnerName", "acme"))); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	for _, want := range []string{
		"INNER JOIN users j_owner_id ON j_owner_id.id = job_queue.owner_id",
		"j_owner_id.name",
		"job_queue.status",
		"j_owner_id.name = $1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sql %q is missing %q", got, want)
		}
	}
}

func TestDirectRepository_FindOneMeansExactlyOne(t *testing.T) {
	ctx := context.Background()
	rowsFor := func(n int) Querier {
		return fakeQuerier{queryFn: func(string, []any) (Rows, error) {
			return &fakeDBRows{rows: n, scan: func(int, []any) error { return nil }}, nil
		}}
	}

	// Nothing → the framework's typed RecordNotFound.
	_, err := directRepo(t, rowsFor(0)).FindOne(ctx, criteria.ByID(domain.NewID("x")))
	var carrier domain.NotificationCarrier
	if err == nil || !errors.As(err, &carrier) {
		t.Fatalf("no match must map to a NotificationCarrier, got %T (%v)", err, err)
	}

	// One → the row.
	if _, err := directRepo(t, rowsFor(1)).FindOne(ctx, criteria.ByID(domain.NewID("x"))); err != nil {
		t.Fatalf("one row: %v", err)
	}

	// Two → refused loudly; the contract is "expected one".
	_, err = directRepo(t, rowsFor(2)).FindOne(ctx, criteria.ByID(domain.NewID("x")))
	if err == nil || !strings.Contains(err.Error(), "more than one row") {
		t.Fatalf("two rows must be refused, got %v", err)
	}
}

// The aggregate DSL is the SAME one — the repository does not redeclare it, it
// inherits it from the core both repositories share.
func TestDirectRepository_AggregateDSLIsTheSharedOne(t *testing.T) {
	var got string
	repo := directRepo(t, fakeQuerier{queryFn: func(sql string, _ []any) (Rows, error) {
		got = sql
		return &fakeDBRows{}, nil
	}})
	total := Count()
	if err := repo.Aggregate(context.Background(), criteria.Where(criteria.Eq("Status", "x")), total); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if want := "SELECT COUNT(*) FROM job_queue WHERE status = $1 AND deleted_at IS NULL"; got != want {
		t.Fatalf("sql =\n  %q\nwant\n  %q", got, want)
	}
	ok, err := repo.Exists(context.Background(), criteria.Where(criteria.Eq("Status", "x")))
	if err != nil || ok {
		t.Fatalf("Exists over an empty cursor = %v, %v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// Configuration, transaction binding, and the read's own refusals
// ---------------------------------------------------------------------------

func TestDirectRepository_Configuration(t *testing.T) {
	repo := directRepo(t, fakeQuerier{}).
		WithContextName("Job").
		WithConstraints(map[string]write.ConstraintBinding{"job_source_uq": {Field: "source"}})
	if repo.contextName() != "Job" {
		t.Fatalf("contextName = %q, want Job", repo.contextName())
	}
	if repo.Schema().Table() != "job_queue" {
		t.Fatalf("Schema() = %q", repo.Schema().Table())
	}
	if len(repo.Joins()) != 0 || repo.JoinFields() != nil {
		t.Error("a repository with no declared traversal exposes none")
	}
}

// InTx returns a COPY: the shared repository is never mutated by one request,
// and the copy's reads run on the caller's transaction.
func TestDirectRepository_InTxIsACopy(t *testing.T) {
	poolSeen, txSeen := false, false
	pool := fakeQuerier{queryFn: func(string, []any) (Rows, error) {
		poolSeen = true
		return &fakeDBRows{}, nil
	}}
	repo := directRepo(t, pool)
	bound := repo.InTx(&directRecordingTx{onQuery: func() { txSeen = true }})

	if _, err := bound.FindAll(context.Background(), criteria.Where(nil)); err != nil {
		t.Fatalf("bound FindAll: %v", err)
	}
	if !txSeen || poolSeen {
		t.Fatalf("the bound read must run on the transaction (tx=%v, pool=%v)", txSeen, poolSeen)
	}
	if _, err := repo.FindAll(context.Background(), criteria.Where(nil)); err != nil {
		t.Fatalf("pooled FindAll: %v", err)
	}
	if !poolSeen {
		t.Error("the original repository must still read from the pool")
	}
}

// directRecordingTx is a core.Tx that reports which surface a read ran through.
type directRecordingTx struct {
	directNoopTx
	onQuery func()
}

func (t *directRecordingTx) Query(context.Context, string, ...any) (Rows, error) {
	t.onQuery()
	return &fakeDBRows{}, nil
}

func TestDirectRepository_ReadRefusals(t *testing.T) {
	ctx := context.Background()
	repo := directRepo(t, fakeQuerier{})

	if _, err := repo.FindAll(ctx, criteria.Where(criteria.Eq("Nope", 1))); err == nil {
		t.Error("an unknown filter field must be refused")
	}
	if _, err := repo.FindAll(ctx, criteria.Where(nil).OrderBy("Nope")); err == nil {
		t.Error("an unknown order field must be refused")
	}
	// An offset without the bounded, ordered window it paginates is refused by
	// the shared window rule — the Direct read inherits it, it does not restate it.
	if _, err := repo.FindAll(ctx, criteria.Where(nil).Offset(10)); err == nil {
		t.Error("an offset with no limit/order must be refused")
	}
	// A driver error reaches the caller.
	boom := directRepo(t, fakeQuerier{queryFn: func(string, []any) (Rows, error) {
		return nil, errFakeDB
	}})
	if _, err := boom.FindAll(ctx, criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Errorf("driver error = %v, want it unchanged", err)
	}
}

// A Direct schema that declares no business field still reads: the id alone is a
// column. The empty-schema refusal is about a schema with NOTHING scannable.
func TestDirectRepository_ScansTheIDAsAColumn(t *testing.T) {
	var got string
	repo := NewDirectRepository[directJob](
		&directReadEngine{fakeRelEngine: fakeRelEngine{q: fakeQuerier{queryFn: func(sql string, _ []any) (Rows, error) {
			got = sql
			return &fakeDBRows{}, nil
		}}}, tx: &directNoopTx{}},
		core.NewDirectSchema[directJob]("job_queue").ID("id"))
	if _, err := repo.FindAll(context.Background(), criteria.Where(nil)); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got != "SELECT id FROM job_queue" {
		t.Fatalf("sql = %q", got)
	}
}

// FindOne propagates a read failure rather than reporting "not found".
func TestDirectRepository_FindOnePropagatesReadFailure(t *testing.T) {
	repo := directRepo(t, fakeQuerier{queryFn: func(string, []any) (Rows, error) { return nil, errFakeDB }})
	if _, err := repo.FindOne(context.Background(), criteria.ByID(domain.NewID("x"))); !errors.Is(err, errFakeDB) {
		t.Fatalf("err = %v, want the driver error", err)
	}
}

// Every read must honour the transaction binding — not just FindAll.
//
// This is the regression test for a real defect: Aggregate and AggregateBy
// reached for the engine's pooled Querier directly, so a fact asked from inside
// a write answered about the state BEFORE that write. It failed silently, which
// is the whole reason asking it in-transaction exists.
func TestDirectRepository_EveryReadHonoursTheTransactionBinding(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		run  func(*DirectRepository[directJob]) error
	}{
		{"FindAll", func(r *DirectRepository[directJob]) error {
			_, err := r.FindAll(ctx, criteria.Where(nil))
			return err
		}},
		{"FindOne", func(r *DirectRepository[directJob]) error {
			_, _ = r.FindOne(ctx, criteria.ByID(domain.NewID("x")))
			return nil
		}},
		{"Exists", func(r *DirectRepository[directJob]) error {
			_, err := r.Exists(ctx, criteria.Where(criteria.Eq("Status", "x")))
			return err
		}},
		{"Aggregate", func(r *DirectRepository[directJob]) error {
			return r.Aggregate(ctx, criteria.Where(nil), Count())
		}},
		{"AggregateBy", func(r *DirectRepository[directJob]) error {
			_, err := r.AggregateBy(ctx, criteria.Where(nil), By("Status"), Count())
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			pooled := false
			onTx := false
			repo := directRepo(t, fakeQuerier{queryFn: func(string, []any) (Rows, error) {
				pooled = true
				return &fakeDBRows{}, nil
			}})
			bound := repo.InTx(&directRecordingTx{onQuery: func() { onTx = true }})
			if err := c.run(bound); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !onTx {
				t.Errorf("%s ran outside the bound transaction", c.name)
			}
			if pooled {
				t.Errorf("%s reached for the pool while a transaction was bound", c.name)
			}
		})
	}
}
