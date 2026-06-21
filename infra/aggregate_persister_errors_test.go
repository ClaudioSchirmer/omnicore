package infra

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// These drive the error/edge branches of aggregate_persister.go that the happy
// paths in aggregate_persister_unit_test.go do not reach: requireSoftDelete
// backstops, the cascade skip/error arms, and the outbox/audit error returns.
// Per-statement failures use fakeTx.execErrSubstr (additive seam) so a single
// Exec in the sequence can fail without disturbing the others.

// covAggNoSD: an aggregate root schema with NO soft-delete (archive/unarchive
// must fail at requireSoftDelete before opening a TX).
func covAggNoSDSchema() *TableSchema {
	return NewTableSchema[*covAgg]("cov_aggs").
		PK("id").
		Field("Name", "name")
}

// covAggNoChild: root has soft-delete but declares no Child, so the cascade
// loop hits the child==nil continue arm.
func covAggNoChildSchema() *TableSchema {
	return NewTableSchema[*covAgg]("cov_aggs").
		PK("id").
		Field("Name", "name").
		SoftDelete("deleted_at")
}

// covAggChildNoSD: the declared child has NO soft-delete, so the cascade loop
// hits the !ok continue arm.
func covAggChildNoSDSchema() *TableSchema {
	return NewTableSchema[*covAgg]("cov_aggs").
		PK("id").
		Field("Name", "name").
		SoftDelete("deleted_at").
		Child(NewTableSchema[covChild]("cov_children").
			PK("id").
			FK("cov_agg_id").
			Field("Label", "label"))
}

func TestUpdateAggregate_RootQueryRowError_RollsBack(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	u, _ := domain.GetUpdatable(root, func(*covAgg) {}, nil, "GetUpdatable")
	pool := newFakePool()
	pool.tx.queryRowErr = errFake
	pg := newFakePostgres(pool)
	if _, err := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{}); err == nil {
		t.Fatal("expected root QueryRow error")
	}
	if !pool.tx.rolledBack {
		t.Error("expected rollback")
	}
}

func TestArchiveAggregate_RequireSoftDelete(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	ar, _ := domain.GetArchivable(root, nil, "GetArchivable")
	pg := newFakePostgres(newFakePool())
	if err := pg.Archive(newBuilderCtx(), ar, covAggNoSDSchema(), writeHook{}); err == nil {
		t.Fatal("archive without SoftDelete must error")
	}
}

func TestUnarchiveAggregate_RequireSoftDelete(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	un, _ := domain.GetUnarchivable(root, nil, "GetUnarchivable")
	pg := newFakePostgres(newFakePool())
	if err := pg.Unarchive(newBuilderCtx(), un, covAggNoSDSchema(), writeHook{}); err == nil {
		t.Fatal("unarchive without SoftDelete must error")
	}
}

// Cascade skip arms: child without a schema (child==nil) and child without
// soft-delete (!ok). Both must succeed (the child is simply skipped).
func TestArchiveAggregate_CascadeSkipArms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema *TableSchema
	}{
		{"childNil", covAggNoChildSchema()},
		{"childNoSoftDelete", covAggChildNoSDSchema()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
			ar, _ := domain.GetArchivable(root, nil, "GetArchivable")
			pool := newFakePool()
			if err := newFakePostgres(pool).Archive(newBuilderCtx(), ar, tc.schema, writeHook{}); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if !pool.tx.committed {
				t.Error("expected commit")
			}
		})
	}
}

func TestUnarchiveAggregate_CascadeSkipArms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema *TableSchema
	}{
		{"childNil", covAggNoChildSchema()},
		{"childNoSoftDelete", covAggChildNoSDSchema()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
			un, _ := domain.GetUnarchivable(root, nil, "GetUnarchivable")
			pool := newFakePool()
			if err := newFakePostgres(pool).Unarchive(newBuilderCtx(), un, tc.schema, writeHook{}); err != nil {
				t.Fatalf("unarchive: %v", err)
			}
			if !pool.tx.committed {
				t.Error("expected commit")
			}
		})
	}
}

// Per-statement error coverage across the aggregate verbs: cascade child Exec,
// outbox Exec, and audit_events Exec each fail in isolation via execErrSubstr.
func TestArchiveAggregate_StatementErrors(t *testing.T) {
	cases := []struct {
		name    string
		substr  string
		audited bool
	}{
		{"cascadeChild", "cov_children", false},
		{"outbox", "outbox", false},
		{"audit", "audit_events", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
			ar, _ := domain.GetArchivable(root, nil, "GetArchivable")
			pool := newFakePool()
			pool.tx.execErrSubstr = c.substr
			pg := newFakePostgres(pool)
			if c.audited {
				pg = auditedPostgres(pool)
			}
			if err := pg.Archive(newBuilderCtx(), ar, covAggSchema, writeHook{}); err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if pool.tx.committed {
				t.Error("must not commit on a statement error")
			}
		})
	}
}

func TestUnarchiveAggregate_StatementErrors(t *testing.T) {
	cases := []struct {
		name    string
		substr  string
		audited bool
	}{
		{"cascadeChild", "cov_children", false},
		{"outbox", "outbox", false},
		{"audit", "audit_events", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
			un, _ := domain.GetUnarchivable(root, nil, "GetUnarchivable")
			pool := newFakePool()
			pool.tx.execErrSubstr = c.substr
			pg := newFakePostgres(pool)
			if c.audited {
				pg = auditedPostgres(pool)
			}
			if err := pg.Unarchive(newBuilderCtx(), un, covAggSchema, writeHook{}); err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
		})
	}
}

func TestDeleteAggregate_StatementErrors(t *testing.T) {
	cases := []struct {
		name    string
		substr  string
		audited bool
	}{
		{"outbox", "outbox", false},
		{"audit", "audit_events", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
			d, _ := domain.GetDeletable(root, nil, "GetDeletable")
			pool := newFakePool()
			pool.tx.execErrSubstr = c.substr
			pg := newFakePostgres(pool)
			if c.audited {
				pg = auditedPostgres(pool)
			}
			if err := pg.Delete(newBuilderCtx(), d, covAggSchema, writeHook{}); err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
		})
	}
}

func TestInsertUpdateAggregate_AuditError(t *testing.T) {
	t.Run("insert", func(t *testing.T) {
		root := &covAgg{Name: "agg"}
		domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
		ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
		pool := newFakePool()
		pool.tx.execErrSubstr = "audit_events"
		if _, err := auditedPostgres(pool).Insert(newBuilderCtx(), ins, covAggSchema, writeHook{}); err == nil {
			t.Fatal("expected audit error")
		}
	})
	t.Run("update", func(t *testing.T) {
		root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
		u, _ := domain.GetUpdatable(root, func(*covAgg) {}, nil, "GetUpdatable")
		pool := newFakePool()
		pool.tx.execErrSubstr = "audit_events"
		if _, err := auditedPostgres(pool).Update(newBuilderCtx(), u, covAggSchema, writeHook{}); err == nil {
			t.Fatal("expected audit error")
		}
	})
}

// Direct-call coverage for the helper skip/error arms not on the verb paths.
func TestBuildAggregatePayload_NilRootAndUndeclaredChild(t *testing.T) {
	fields := domain.Fields{"name": "x"}
	if p := buildAggregatePayload(fields, nil, covAggSchema); p["children"] != nil {
		t.Errorf("nil root must carry no children, got %v", p)
	}
	// Root carries a covChild but the schema declares no child → the child is
	// skipped (childSchema nil continue).
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	p := buildAggregatePayload(fields, &root.AggregateRoot, covAggNoChildSchema())
	if p["children"] != nil {
		t.Errorf("undeclared child must be skipped, got %v", p["children"])
	}
}

func TestInsertChildren_SkipsNonAddedConstructor(t *testing.T) {
	root := &covAgg{Name: "agg"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{covChild{ID: "c1", Label: "x"}})
	domain.RemoveAggregateChild(root, covChild{ID: "c1", Label: "x"}) // status → Removed
	tx := newFakeTx()
	if err := insertChildren(context.Background(), tx, &root.AggregateRoot, covAggSchema, "root-1"); err != nil {
		t.Fatalf("insertChildren: %v", err)
	}
}

func TestApplyChildChanges_UndeclaredChildErrors(t *testing.T) {
	root := newCovAgg(t, covChild{ID: "c1", Label: "x"})
	domain.ChangeAggregateChild(root, covChild{ID: "c1", Label: "x"}, covChild{ID: "c1", Label: "y"})
	tx := newFakeTx()
	if err := applyChildChanges(context.Background(), tx, &root.AggregateRoot, covAggNoChildSchema(), "root-1"); err == nil {
		t.Fatal("expected error for a child without a declared schema")
	}
}
