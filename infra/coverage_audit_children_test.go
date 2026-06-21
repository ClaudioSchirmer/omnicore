package infra

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// covAgg is a self-contained aggregate fixture declaring all five modes so the
// archive/unarchive/delete child-event branches of childEventOf are reachable
// through the Build*Event surface.
type covAgg struct {
	domain.AggregateRoot
	Name string
}

func (e *covAgg) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *covAgg) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *covAgg) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *covAgg) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{covChild{}}
}

type covChild struct {
	ID    string
	Label string
}

func (c covChild) GetID() string                                    { return c.ID }
func (c covChild) BuildRules(string, domain.Service, *domain.Rules) {}

var covAggSchema = NewTableSchema[*covAgg]("cov_aggs").
	PK("id").
	Field("Name", "name").
	SoftDelete("deleted_at").
	Child(NewTableSchema[covChild]("cov_children").
		PK("id").
		FK("cov_agg_id").
		Field("Label", "label").
		SoftDelete("deleted_at"))

func TestChildEventOf_InsertSnapshotsChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
	id := domain.NewID(uuid.NewString())
	i, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	ev := BuildInsertEvent(newBuilderCtx(), i, id, covAggSchema, nil)
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "inserted" || kids[0].Snapshot["Label"] != "x" {
		t.Fatalf("insert child event drifted: %+v", ev.Children)
	}
}

func TestChildEventOf_UpdateAddedChild(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{})
	u, err := domain.GetUpdatable(root, func(r *covAgg) {
		domain.AddAggregateChild(r, covChild{ID: "c2", Label: "y"})
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, covAggSchema, nil)
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "inserted" {
		t.Fatalf("update-added child event drifted: %+v", ev.Children)
	}
}

func TestChildEventOf_UpdateRemovedChild(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{covChild{ID: "c1", Label: "x"}})
	u, err := domain.GetUpdatable(root, func(r *covAgg) {
		domain.RemoveAggregateChild(r, covChild{ID: "c1", Label: "x"})
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, covAggSchema, nil)
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "archived" {
		t.Fatalf("update-removed child event drifted: %+v", ev.Children)
	}
}

func TestChildEventOf_ArchiveChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{covChild{ID: "c1", Label: "x"}})
	ar, err := domain.GetArchivable(root, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	ev := BuildArchiveEvent(newBuilderCtx(), ar, covAggSchema, nil)
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "archived" {
		t.Fatalf("archive child event drifted: %+v", ev.Children)
	}
}

func TestChildEventOf_UnarchiveChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{covChild{ID: "c1", Label: "x"}})
	un, err := domain.GetUnarchivable(root, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	ev := BuildUnarchiveEvent(newBuilderCtx(), un, covAggSchema, nil)
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "unarchived" {
		t.Fatalf("unarchive child event drifted: %+v", ev.Children)
	}
}

func TestChildEventOf_DeleteChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{covChild{ID: "c1", Label: "x"}})
	d, err := domain.GetDeletable(root, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	ev := BuildDeleteEvent(newBuilderCtx(), d, covAggSchema, nil)
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "deleted" {
		t.Fatalf("delete child event drifted: %+v", ev.Children)
	}
}
