package write

import (
	"testing"
	"time"

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
	domain.Managed
	Label string
}

func (c covChild) BuildRules(string, domain.Service, *domain.Rules) {}

var covAggSchema = NewTableSchema[*covAgg]("cov_aggs").
	ID("id").
	Field("Name", "name").
	DeletedAt("deleted_at").
	Child(NewTableSchema[covChild]("cov_children").
		ID("id").
		ParentID("cov_agg_id").
		Field("Label", "label").
		DeletedAt("deleted_at"))

func TestChildEventOf_InsertSnapshotsChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	domain.AddAggregateChild(root, domain.WithID(covChild{Label: "x"}, domain.NewID("c1")))
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
	u, err := domain.GetUpdatable(root, func(r *covAgg) error {
		domain.AddAggregateChild(r, domain.WithID(covChild{Label: "y"}, domain.NewID("c2")))
		return nil
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
	root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(covChild{Label: "x"}, domain.NewID("c1"))})
	u, err := domain.GetUpdatable(root, func(r *covAgg) error {
		domain.RemoveAggregateChild(r, domain.WithID(covChild{Label: "x"}, domain.NewID("c1")))
		return nil
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

// archiveTestStamp is the instant these tests archive with — the one value a
// root and the children its cascade stamped all carry.
var archiveTestStamp = time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

// archivedCovChild builds a child as the LOADER would hand it over: identified,
// and carrying the DeletedAt its row holds (domain.SetManagedColumns is the
// framework's populate seam).
func archivedCovChild(id string, deletedAt time.Time) domain.AggregateValueObject {
	child := domain.WithID(covChild{Label: "x"}, domain.NewID(id))
	domain.SetManagedColumns(&child, 1, nil, nil, &deletedAt)
	return child
}

func TestChildEventOf_ArchiveChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(covChild{Label: "x"}, domain.NewID("c1"))})
	ar, err := domain.GetArchivable(root, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	ev := BuildArchiveEvent(newBuilderCtx(), ar, covAggSchema, nil, CascadeStamps{Own: archiveTestStamp})
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "archived" {
		t.Fatalf("archive child event drifted: %+v", ev.Children)
	}
}

// An already-archived child is NOT part of the archive cascade: the statement is
// gated on `deleted_at IS NULL`, so its row keeps the older stamp it carries and
// the trail must not claim this archive touched it.
func TestChildEventOf_ArchiveSkipsAlreadyArchivedChild(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{archivedCovChild("c1", archiveTestStamp.Add(-2*time.Hour))})
	ar, err := domain.GetArchivable(root, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	ev := BuildArchiveEvent(newBuilderCtx(), ar, covAggSchema, nil, CascadeStamps{Own: archiveTestStamp})
	if len(ev.Children["covChild"]) != 0 {
		t.Fatalf("a child already archived must not appear on the archive trail: %+v", ev.Children)
	}
}

func TestChildEventOf_UnarchiveChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{archivedCovChild("c1", archiveTestStamp)})
	un, err := domain.GetUnarchivable(root, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	ev := BuildUnarchiveEvent(newBuilderCtx(), un, covAggSchema, nil, CascadeStamps{Own: archiveTestStamp})
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].Op != "unarchived" {
		t.Fatalf("unarchive child event drifted: %+v", ev.Children)
	}
}

// The child the ROOT'S archive put to sleep comes back and is reported; the one
// archived on its own, under its own older stamp, is not — the restore statement
// never reached its row.
func TestChildEventOf_UnarchiveReportsOnlyTheCascadedChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{
		archivedCovChild("c1", archiveTestStamp),
		archivedCovChild("c2", archiveTestStamp.Add(-2*time.Hour)),
	})
	un, err := domain.GetUnarchivable(root, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	ev := BuildUnarchiveEvent(newBuilderCtx(), un, covAggSchema, nil, CascadeStamps{Own: archiveTestStamp})
	kids := ev.Children["covChild"]
	if len(kids) != 1 || kids[0].ID != "c1" || kids[0].Op != "unarchived" {
		t.Fatalf("the restore must report ONLY the child this root archived, got %+v", kids)
	}
}

func TestChildEventOf_DeleteChildren(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{domain.WithID(covChild{Label: "x"}, domain.NewID("c1"))})
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
