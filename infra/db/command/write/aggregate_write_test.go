package write

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box unit coverage for the shared aggregate write path on BaseEngine,
// driven through the recording fake WriteTx/WriteBeginner (see flat_write_test.go).
// The integration suites remain the real-SQL contract; these exercise the
// control flow + the child state machine (Added/Changed/Removed/Constructor) +
// the cascade + the guard branches without a live backend.

type aggWriteChild struct {
	domain.Managed
	Label string
}

func (c aggWriteChild) BuildRules(string, domain.Service, *domain.Rules) {}

type aggWriteRoot struct {
	domain.AggregateRoot
	Name string
}

func (e *aggWriteRoot) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *aggWriteRoot) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *aggWriteRoot) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *aggWriteRoot) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{aggWriteChild{}}
}

func aggWriteSchema() *TableSchema {
	return NewTableSchema[*aggWriteRoot]("agg_w").
		ID("id").
		Field("Name", "name").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Child(NewTableSchema[aggWriteChild]("agg_w_children").
			ID("id").
			ParentID("agg_w_id").
			Field("Label", "label").
			DeletedAt("deleted_at").
			CreatedAt("created_at").
			UpdatedAt("updated_at"))
}

func TestBaseEngine_InsertAggregate_WithChild(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	domain.AddAggregateChild(root, aggWriteChild{Label: "a"})
	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}

	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	res, err := be.Insert(newBuilderCtx(), ins, aggWriteSchema(), firingHook)
	if err != nil {
		t.Fatalf("insertAggregate: %v", err)
	}
	if res.ID.Value() == "" || !tx.committed {
		t.Fatalf("expected committed insert with id, got id=%q committed=%v", res.ID, tx.committed)
	}
	// root INSERT + child INSERT + outbox + audit.
	if len(tx.execs) != 4 {
		t.Errorf("expected 4 statements (root+child+outbox+audit), got %d: %v", len(tx.execs), tx.execs)
	}
}

func TestBaseEngine_InsertAggregate_WritesMintedChildIDBack(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	domain.AddAggregateChild(root, aggWriteChild{Label: "a"})
	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}

	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("insertAggregate: %v", err)
	}

	// The persister mints the child ID inside the INSERT; the write-back must
	// surface it in the aggregate map so post-write readers (FromEntity
	// projections, outbox/audit snapshots) see the child as persisted.
	items := domain.GetCurrentItemsOf[aggWriteChild](&root.AggregateRoot)
	if len(items) != 1 {
		t.Fatalf("expected exactly one current child, got %+v", items)
	}
	if items[0].GetID().IsEmpty() {
		t.Fatal("minted child id must be written back into the aggregate map")
	}
	if _, err := uuid.Parse(items[0].GetID().Value()); err != nil {
		t.Errorf("written-back id must be the minted UUID, got %q: %v", items[0].GetID(), err)
	}
	if items[0].Label != "a" {
		t.Errorf("write-back must not disturb the child's data, got %+v", items[0])
	}
}

func TestBaseEngine_UpdateAggregate_AllChildOps(t *testing.T) {
	id1 := uuid.NewString() // will be Changed
	id2 := uuid.NewString() // will be Removed (archived)
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{
		domain.WithID(aggWriteChild{Label: "keep"}, domain.NewID(id1)),
		domain.WithID(aggWriteChild{Label: "drop"}, domain.NewID(id2)),
	})
	upd, err := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
		domain.ChangeAggregateChild(r, domain.WithID(aggWriteChild{Label: "keep"}, domain.NewID(id1)), domain.WithID(aggWriteChild{Label: "changed"}, domain.NewID(id1)))
		domain.RemoveAggregateChild(r, domain.WithID(aggWriteChild{Label: "drop"}, domain.NewID(id2)))
		domain.AddAggregateChild(r, aggWriteChild{Label: "new"})
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	tx := &recTx{count: 1} // root UPDATE + child UPDATE both match a row
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("updateAggregate: %v", err)
	}
	if !tx.committed {
		t.Error("expected commit")
	}
	// root UPDATE + child INSERT(new) + child UPDATE(changed) + child Archive(removed)
	// + outbox + audit = 6 statements.
	if len(tx.execs) != 6 {
		t.Errorf("expected 6 statements, got %d: %v", len(tx.execs), tx.execs)
	}
}

func TestBaseEngine_ArchiveUnarchiveAggregate_Cascade(t *testing.T) {
	for _, verb := range []string{"archive", "unarchive"} {
		root := &aggWriteRoot{Name: "r"}
		root.SetID(domain.NewID(uuid.NewString()))
		root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(aggWriteChild{Label: "c"}, domain.NewIDFromUUID(uuid.New()))})

		tx := &recTx{count: 1}
		be := newFlatBE(&recBeginner{tx: tx})
		var err error
		if verb == "archive" {
			a, _ := domain.GetArchivable(root, nil, "GetArchivable")
			err = be.Archive(newBuilderCtx(), a, aggWriteSchema(), firingHook)
		} else {
			u, _ := domain.GetUnarchivable(root, nil, "GetUnarchivable")
			err = be.Unarchive(newBuilderCtx(), u, aggWriteSchema(), firingHook)
		}
		if err != nil {
			t.Fatalf("%s aggregate: %v", verb, err)
		}
		if !tx.committed {
			t.Errorf("%s: expected commit", verb)
		}
		// root soft-write + child cascade + outbox + audit = 4.
		if len(tx.execs) != 4 {
			t.Errorf("%s: expected 4 statements, got %d: %v", verb, len(tx.execs), tx.execs)
		}
	}
}

func TestBaseEngine_DeleteAggregate(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(aggWriteChild{Label: "c"}, domain.NewIDFromUUID(uuid.New()))})
	d, _ := domain.GetDeletable(root, nil, "GetDeletable")

	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), d, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("deleteAggregate: %v", err)
	}
	// The framework owns the cascade in Go: an explicit child DELETE (by ParentID)
	// precedes the root DELETE, then outbox + audit = 4 statements — no reliance
	// on a database ON DELETE CASCADE.
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements (child delete + root delete + outbox + audit), got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "DELETE FROM agg_w_children WHERE agg_w_id") {
		t.Errorf("stmt[0]: expected child DELETE by ParentID first, got %q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "DELETE FROM agg_w WHERE id") {
		t.Errorf("stmt[1]: expected root DELETE after children, got %q", tx.execs[1])
	}
}

// A declared child with NO loaded items must still be deleted: deleteAggregate
// enumerates the schema's declared ChildSchemas(), not the loaded aggregate
// items, so every child table is cleared by ParentID — the reach of ON DELETE CASCADE
// without depending on it.
func TestBaseEngine_DeleteAggregate_DeclaredChildWithoutLoadedItems(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	// No AggregateConstructor → no loaded child items, but the schema declares one.
	d, _ := domain.GetDeletable(root, nil, "GetDeletable")

	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), d, aggWriteSchema(), firingHook); err != nil {
		t.Fatalf("deleteAggregate: %v", err)
	}
	if len(tx.execs) != 4 {
		t.Fatalf("expected 4 statements even with no loaded children, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "DELETE FROM agg_w_children WHERE agg_w_id") {
		t.Errorf("stmt[0]: declared child must be deleted by ParentID, got %q", tx.execs[0])
	}
}

func TestBaseEngine_UpdateAggregate_ChangedChildWithoutIDIsError(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	// A Constructor child with no id, then Changed → updateChild requires an id.
	root.AggregateConstructor([]domain.AggregateValueObject{aggWriteChild{Label: "x"}})
	upd, _ := domain.GetUpdatable(root, func(r *aggWriteRoot) error {
		domain.ChangeAggregateChild(r, aggWriteChild{Label: "x"}, aggWriteChild{Label: "y"})
		return nil
	}, nil, "GetUpdatable")

	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), upd, aggWriteSchema(), WriteHook{}); err == nil {
		t.Fatal("expected an error updating a child without an id")
	}
}

func TestBaseEngine_InsertAggregate_UndeclaredChildSchemaIsError(t *testing.T) {
	root := &aggWriteRoot{Name: "r"}
	domain.AddAggregateChild(root, aggWriteChild{Label: "a"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")

	// Schema WITHOUT the child declaration → childSchemaOrErr must fail loudly.
	schemaNoChild := NewTableSchema[*aggWriteRoot]("agg_w").
		ID("id").Field("Name", "name").
		DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at")

	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	_, err := be.Insert(newBuilderCtx(), ins, schemaNoChild, WriteHook{})
	if err == nil || !strings.Contains(err.Error(), "no TableSchema declared") {
		t.Fatalf("expected undeclared-child error, got %v", err)
	}
}
