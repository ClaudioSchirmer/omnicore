package write

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// A Removed child is decided by ONE thing — whether its own schema declares
// DeletedAt — exactly like the root is. Declared → ARCHIVE (the row lingers);
// absent → DELETE (there is no state to stamp). These tests pin that rule on
// every emitter the write drives: the SQL, the audit event and the outbox
// payload, which must all tell the same story about the same row.

// covAggHardChildSchema is covAggSchema with the archive column taken OFF the
// child: same aggregate, same tables, the other side of the rule.
var covAggHardChildSchema = NewTableSchema[*covAgg]("cov_aggs").
	ID("id").
	Field("Name", "name").
	DeletedAt("deleted_at").
	Child(NewTableSchema[covChild]("cov_children").
		ID("id").
		ParentID("cov_agg_id").
		Field("Label", "label"))

// removedCovChild builds an Updatable whose single DB-loaded child is Removed.
func removedCovChild(t *testing.T) domain.Updatable {
	t.Helper()
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(covChild{Label: "x"}, domain.NewID("c1"))})
	u, err := domain.GetUpdatable(root, func(r *covAgg) error {
		domain.RemoveAggregateChild(r, domain.WithID(covChild{Label: "x"}, domain.NewID("c1")))
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return u
}

// A hard-removed child cannot leave its sibling row behind: a sibling is a 1:1
// slice of the child's row, so it goes first, in the same TX — the same order
// hardDelete uses when the whole aggregate goes.
func TestRemoveChild_WithoutDeletedAt_TakesItsSiblingFirst(t *testing.T) {
	id := uuid.NewString()
	root := &csRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{
		domain.WithID(csChild{Label: "L", Note: "N"}, domain.NewID(id)),
	})
	upd, err := domain.GetUpdatable(root, func(r *csRoot) error {
		domain.RemoveAggregateChild(r, domain.WithID(csChild{Label: "L", Note: "N"}, domain.NewID(id)))
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, csSchema(), firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sib := indexOfPrefix(tx.execs, "DELETE FROM cs_child_ext")
	child := indexOfPrefix(tx.execs, "DELETE FROM cs_child ")
	if sib < 0 || child < 0 {
		t.Fatalf("both the child and its sibling must be deleted, got %v", tx.execs)
	}
	if sib > child {
		t.Errorf("the sibling row must go BEFORE the child row, got %v", tx.execs)
	}
}

// The sibling delete is part of the same write, so its failure is the write's
// failure — the child row must not be deleted behind a sibling that survived.
func TestRemoveChild_SiblingDeleteFailurePropagates(t *testing.T) {
	id := uuid.NewString()
	root := &csRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{
		domain.WithID(csChild{Label: "L", Note: "N"}, domain.NewID(id)),
	})
	upd, err := domain.GetUpdatable(root, func(r *csRoot) error {
		domain.RemoveAggregateChild(r, domain.WithID(csChild{Label: "L", Note: "N"}, domain.NewID(id)))
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	tx := &recTx{count: 1, execErrSub: "DELETE FROM cs_child_ext"}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, csSchema(), firingHook); err == nil {
		t.Fatal("a failing sibling delete must fail the write")
	}
	if hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "DELETE FROM cs_child ") }) {
		t.Errorf("the child row must not be deleted after its sibling failed, got %v", tx.execs)
	}
}

// The dominant path is untouched: a child that declares DeletedAt is stamped,
// never deleted.
func TestRemoveChild_WithDeletedAt_ArchivesAndDeletesNothing(t *testing.T) {
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), removedCovChild(t), covAggSchema, firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !hasStmt(tx.execs, func(s string) bool {
		return strings.HasPrefix(s, "UPDATE cov_children") && strings.Contains(s, "deleted_at")
	}) {
		t.Errorf("an archivable child must be stamped, got %v", tx.execs)
	}
	if hasStmt(tx.execs, func(s string) bool { return strings.HasPrefix(s, "DELETE FROM cov_children") }) {
		t.Errorf("an archivable child must never be deleted by a remove, got %v", tx.execs)
	}
}

// The audit trail reports the row's real fate. Both branches keep the previous
// state as the snapshot — that is what preserves the history of a row the
// database no longer has.
func TestChildEvent_RemovedChildOpFollowsTheColumn(t *testing.T) {
	hard := BuildUpdateEvent(newBuilderCtx(), removedCovChild(t), covAggHardChildSchema, nil)
	kids := hard.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "deleted" {
		t.Fatalf("a child without DeletedAt is deleted, not archived: %+v", hard.Children)
	}
	if kids[0].Snapshot["Label"] != "x" {
		t.Errorf("the previous state must survive the deletion in the audit trail, got %+v", kids[0])
	}

	soft := BuildUpdateEvent(newBuilderCtx(), removedCovChild(t), covAggSchema, nil)
	if kids := soft.Children["covChild"]; len(kids) != 1 || kids[0].Op != "archived" {
		t.Fatalf("a child with DeletedAt is archived: %+v", soft.Children)
	}
}

// The projection must be told what actually happened: "delete" drops the element
// from the array, "archive" stamps it. The two emitters cannot disagree, or the
// document drifts from the relational row.
func TestBuildWritePayload_RemovedChildOpFollowsTheColumn(t *testing.T) {
	opOf := func(t *testing.T, schema *TableSchema) any {
		t.Helper()
		id := uuid.NewString()
		root := &covAgg{Name: "a"}
		root.SetID(domain.NewID(id))
		root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(covChild{Label: "x"}, domain.NewID("c1"))})
		domain.RemoveAggregateChild(root, domain.WithID(covChild{Label: "x"}, domain.NewID("c1")))
		p := buildWritePayload(schema, root, root.GetAggregateRoot(), "UPDATED", testNow,
			schema.WriteFields(root), outboxMeta{ID: id})
		children, ok := p[payloadKeyChildren].(map[string]any)
		if !ok {
			t.Fatalf("the removed child must ride the payload, got %v", p)
		}
		items := children["covChild"].([]map[string]any)
		if len(items) != 1 {
			t.Fatalf("expected exactly one child entry, got %v", items)
		}
		return items[0][payloadKeyOp]
	}

	if got := opOf(t, covAggHardChildSchema); got != "delete" {
		t.Errorf("a removed child without DeletedAt must project as a delete, got %v", got)
	}
	if got := opOf(t, covAggSchema); got != "archive" {
		t.Errorf("a removed child with DeletedAt must project as an archive, got %v", got)
	}
}
