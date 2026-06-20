package infra

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// These tests drive the aggregate write path of aggregate_persister.go against
// the in-process fakePool. covAgg / covChild / covAggSchema come from
// coverage_audit_children_test.go. The fakeTx handles the child INSERT
// QueryRow (RETURNING id) via scanID and the cascade/outbox/audit Exec calls.

func newCovAgg(t *testing.T, children ...covChild) *covAgg {
	t.Helper()
	root := &covAgg{Name: "agg"}
	root.SetID(domain.NewID(uuid.NewString()))
	avos := make([]domain.AggregateValueObject, 0, len(children))
	for _, c := range children {
		avos = append(avos, c)
	}
	root.AggregateConstructor(avos)
	return root
}

func TestInsertAggregate_RootAndChildren(t *testing.T) {
	root := &covAgg{Name: "agg"}
	domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
	domain.AddAggregateChild(root, covChild{ID: "c2", Label: "y"})
	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}

	pool := newFakePool()
	pg := auditedPostgres(pool)
	res, err := pg.Insert(newBuilderCtx(), ins, covAggSchema, writeHook{})
	if err != nil {
		t.Fatalf("insertAggregate: %v", err)
	}
	if res.ID != pool.tx.scanID {
		t.Errorf("root ID = %q, want %q", res.ID, pool.tx.scanID)
	}
	if !pool.tx.committed {
		t.Error("aggregate insert was not committed")
	}
	// outbox Exec + audit Exec at minimum (child INSERTs go through QueryRow).
	if len(pool.tx.execCalls) < 2 {
		t.Errorf("expected outbox + audit Exec, got %d", len(pool.tx.execCalls))
	}
}

func TestInsertAggregate_BeginError(t *testing.T) {
	root := &covAgg{Name: "agg"}
	domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")

	pool := newFakePool()
	pool.beginErr = errFake
	pg := newFakePostgres(pool)
	if _, err := pg.Insert(newBuilderCtx(), ins, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected Begin error")
	}
}

func TestInsertAggregate_RootQueryRowError_RollsBack(t *testing.T) {
	root := &covAgg{Name: "agg"}
	domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")

	pool := newFakePool()
	pool.tx.queryRowErr = errFake
	pg := newFakePostgres(pool)
	if _, err := pg.Insert(newBuilderCtx(), ins, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected root QueryRow error")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback after root QueryRow error")
	}
}

// childSchemaOrErr surfaces a loud error when a discovered aggregate child type
// has no TableSchema registered on the root schema.
func TestInsertAggregate_UndeclaredChildSchema_Errors(t *testing.T) {
	noChild := NewTableSchema[*covAgg]("cov_aggs").
		PK("ID", "id").
		Field("Name", "name").
		SoftDelete("deleted_at")

	root := &covAgg{Name: "agg"}
	domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")

	pool := newFakePool()
	pg := newFakePostgres(pool)
	if _, err := pg.Insert(newBuilderCtx(), ins, noChild, writeHook{}); err == nil {
		t.Fatal("expected error for undeclared child schema")
	}
}

func TestUpdateAggregate_AddedChangedRemoved(t *testing.T) {
	root := newCovAgg(t,
		covChild{ID: "c1", Label: "old"},
		covChild{ID: "c3", Label: "gone"},
	)
	u, err := domain.GetUpdatable(root, func(r *covAgg) {
		domain.AddAggregateChild(r, covChild{ID: "c2", Label: "new"})
		domain.ChangeAggregateChild(r, covChild{ID: "c1", Label: "old"}, covChild{ID: "c1", Label: "changed"})
		domain.RemoveAggregateChild(r, covChild{ID: "c3", Label: "gone"})
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	pool := newFakePool()
	pg := auditedPostgres(pool)
	if _, err := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{}); err != nil {
		t.Fatalf("updateAggregate: %v", err)
	}
	if !pool.tx.committed {
		t.Error("aggregate update was not committed")
	}
	// At least one Exec for the Removed child Archive, plus outbox + audit.
	if len(pool.tx.execCalls) < 2 {
		t.Errorf("expected child archive + outbox/audit Execs, got %d", len(pool.tx.execCalls))
	}
}

func TestUpdateAggregate_ChangedChildWithoutID_Errors(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "old"})
	u, err := domain.GetUpdatable(root, func(r *covAgg) {
		domain.ChangeAggregateChild(r, covChild{ID: "c1", Label: "old"}, covChild{ID: "", Label: "noid"})
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	pool := newFakePool()
	pg := newFakePostgres(pool)
	if _, err := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected error updating a Changed child without an id")
	}
}

func TestArchiveAggregate_CascadesToChildren(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	ar, err := domain.GetArchivable(root, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}

	pool := newFakePool()
	pg := auditedPostgres(pool)
	if err := pg.Archive(newBuilderCtx(), ar, covAggSchema, writeHook{}); err != nil {
		t.Fatalf("archiveAggregate: %v", err)
	}
	if !pool.tx.committed {
		t.Error("aggregate archive was not committed")
	}
	// root archive + child cascade + outbox + audit = >= 3 Execs.
	if len(pool.tx.execCalls) < 3 {
		t.Errorf("expected root + cascade + outbox/audit Execs, got %d", len(pool.tx.execCalls))
	}
}

func TestUnarchiveAggregate_CascadesToChildren(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	un, err := domain.GetUnarchivable(root, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}

	pool := newFakePool()
	pg := auditedPostgres(pool)
	if err := pg.Unarchive(newBuilderCtx(), un, covAggSchema, writeHook{}); err != nil {
		t.Fatalf("unarchiveAggregate: %v", err)
	}
	if !pool.tx.committed {
		t.Error("aggregate unarchive was not committed")
	}
}

func TestDeleteAggregate_RootOnly(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	d, err := domain.GetDeletable(root, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}

	pool := newFakePool()
	pg := auditedPostgres(pool)
	if err := pg.Delete(newBuilderCtx(), d, covAggSchema, writeHook{}); err != nil {
		t.Fatalf("deleteAggregate: %v", err)
	}
	if !pool.tx.committed {
		t.Error("aggregate delete was not committed")
	}
}

func TestAggregateVerbs_BeginError(t *testing.T) {
	type tc struct {
		name  string
		drive func(pg *Postgres) error
	}
	mkRoot := func() *covAgg { return newCovAgg(t, covChild{ID: "c1", Label: "x"}) }
	cases := []tc{
		{"Archive", func(pg *Postgres) error {
			ar, _ := domain.GetArchivable(mkRoot(), nil, "GetArchivable")
			return pg.Archive(newBuilderCtx(), ar, covAggSchema, writeHook{})
		}},
		{"Unarchive", func(pg *Postgres) error {
			un, _ := domain.GetUnarchivable(mkRoot(), nil, "GetUnarchivable")
			return pg.Unarchive(newBuilderCtx(), un, covAggSchema, writeHook{})
		}},
		{"Delete", func(pg *Postgres) error {
			d, _ := domain.GetDeletable(mkRoot(), nil, "GetDeletable")
			return pg.Delete(newBuilderCtx(), d, covAggSchema, writeHook{})
		}},
		{"Update", func(pg *Postgres) error {
			u, _ := domain.GetUpdatable(mkRoot(), func(*covAgg) {}, nil, "GetUpdatable")
			_, err := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := newFakePool()
			pool.beginErr = errFake
			pg := newFakePostgres(pool)
			if err := c.drive(pg); err == nil {
				t.Fatalf("%s: expected Begin error", c.name)
			}
		})
	}
}

func TestAggregateVerbs_BeforeCommitHookError_RollsBack(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	ins, _ := domain.GetInsertable(&covAgg{Name: "agg"}, nil, "GetInsertable")
	_ = root

	pool := newFakePool()
	pg := newFakePostgres(pool)
	hook := writeHook{
		BeforeCommit: func(_ persistence.RequestContext, _ domain.Entity, _ domain.ID, _ persistence.TxHandle) error {
			return errFake
		},
	}
	if _, err := pg.Insert(newBuilderCtx(), ins, covAggSchema, hook); err == nil {
		t.Fatal("expected BeforeCommit hook error")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback after BeforeCommit hook error")
	}
}

// buildAggregatePayload is exercised by the insert/update happy paths; this
// asserts its structural shape (root + active children, Removed excluded)
// directly so a payload regression is caught at unit granularity.
func TestBuildAggregatePayload_ShapeExcludesRemoved(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "keep"})
	// Mark one item Removed via a fresh aggregate then remove.
	domain.RemoveAggregateChild(root, covChild{ID: "c1", Label: "keep"})
	domain.AddAggregateChild(root, covChild{ID: "c2", Label: "active"})

	rootFields := covAggSchema.writeFields(root)
	payload := buildAggregatePayload(rootFields, &root.AggregateRoot, covAggSchema)
	if _, ok := payload["root"]; !ok {
		t.Error("payload must carry root fields")
	}
	children, ok := payload["children"].(map[string][]domain.Fields)
	if !ok {
		t.Fatalf("payload children shape = %T", payload["children"])
	}
	got := children["covChild"]
	if len(got) != 1 {
		t.Fatalf("expected 1 active child (Removed excluded), got %d", len(got))
	}
	if got[0]["label"] != "active" {
		t.Errorf("active child fields = %v", got[0])
	}
}
