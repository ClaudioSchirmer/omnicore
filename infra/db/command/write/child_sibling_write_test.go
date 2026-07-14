package write

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// A2b coverage: an aggregate child that carries its OWN sibling (the one allowed
// recursive width). The child-sibling shares the child's PK; on INSERT it is
// written after the child row, on hard-delete it is removed via a subquery over
// the child rows before they are deleted.

type csChild struct {
	ID    string
	Label string
	Note  string // mapped to the child's sibling table
}

func (c csChild) GetID() domain.ID                                 { return domain.NewID(c.ID) }
func (c csChild) BuildRules(string, domain.Service, *domain.Rules) {}

type csRoot struct {
	domain.AggregateRoot
	Name string
}

func (e *csRoot) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *csRoot) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *csRoot) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *csRoot) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{csChild{}}
}

func csSchema() *TableSchema {
	return NewTableSchema[*csRoot]("cs_root").
		PK("id").
		Field("Name", "name").
		Child(NewTableSchema[csChild]("cs_child").
			PK("id").
			FK("root_id").
			Field("Label", "label").
			Sibling(NewSiblingSchema[csChild]("cs_child_ext").Field("Note", "note")))
}

func TestInsertAggregate_ChildSibling(t *testing.T) {
	root := &csRoot{Name: "r"}
	domain.AddAggregateChild(root, csChild{Label: "L", Note: "N"})
	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), ins, csSchema(), firingHook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// insert root + insert child + insert child-sibling + outbox + audit = 5.
	if len(tx.execs) != 5 {
		t.Fatalf("expected 5 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	found := false
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "INSERT INTO cs_child_ext") {
			found = true
		}
	}
	if !found {
		t.Errorf("the child's sibling row must be inserted, got %v", tx.execs)
	}
}

func TestDeleteAggregate_ChildSibling(t *testing.T) {
	root := &csRoot{Name: "r"}
	root.SetID(domain.NewID(uuid.NewString()))
	del, err := domain.GetDeletable(root, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), del, csSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// child-sibling subquery delete + child delete + root delete + outbox + audit = 5.
	if len(tx.execs) != 5 {
		t.Fatalf("expected 5 statements, got %d: %v", len(tx.execs), tx.execs)
	}
	if !strings.HasPrefix(tx.execs[0], "DELETE FROM cs_child_ext WHERE id IN (SELECT") {
		t.Errorf("stmt[0] must delete the child siblings via subquery first, got %q", tx.execs[0])
	}
	if !strings.HasPrefix(tx.execs[1], "DELETE FROM cs_child WHERE root_id") {
		t.Errorf("stmt[1] must delete the child rows, got %q", tx.execs[1])
	}
}
