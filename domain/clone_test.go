package domain

import (
	"testing"

	"github.com/google/uuid"
)

// simpleEntity is a minimal BaseEntity-only entity used to test clone /
// captureOld / Old on the flat (non-aggregate) path.
type simpleEntity struct {
	BaseEntity
	Name  string
	Email string
}

func (e *simpleEntity) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}
func (e *simpleEntity) BuildRules(string, Service, *Rules) {}

func TestCloneEntity_PreservesExportedFields(t *testing.T) {
	src := &simpleEntity{Name: "alice", Email: "a@x.com"}
	src.SetID(NewID(uuid.NewString()))
	ensureInit(src)

	clone := cloneEntity(src)
	if clone == nil {
		t.Fatal("expected non-nil clone")
	}
	cs, ok := clone.(*simpleEntity)
	if !ok {
		t.Fatalf("expected *simpleEntity clone, got %T", clone)
	}
	if cs.Name != "alice" || cs.Email != "a@x.com" {
		t.Errorf("clone lost exported fields: %+v", cs)
	}
}

func TestCloneEntity_ClonedNotifCtxIsZero(t *testing.T) {
	src := &simpleEntity{Name: "alice"}
	ensureInit(src)
	src.AddNotification("Name", RequiredFieldNotification{})

	clone := cloneEntity(src).(*simpleEntity)

	// The ghost carries a context of its own (every entity does), but none of
	// the source's state: no shared pointer, no inherited notifications.
	if clone.NotificationContext() == src.NotificationContext() {
		t.Error("clone must not share the source's notification context")
	}
	if msgs := clone.NotificationContext().Messages(); len(msgs) != 0 {
		t.Errorf("clone notifications should be empty (frozen ghost), got %+v", msgs)
	}
}

func TestCloneEntity_MutatingOriginalAfterCloneDoesNotAffectClone(t *testing.T) {
	src := &simpleEntity{Name: "alice", Email: "a@x.com"}
	src.SetID(NewID(uuid.NewString()))
	ensureInit(src)

	clone := cloneEntity(src).(*simpleEntity)

	src.Name = "bob"
	src.Email = "b@x.com"

	if clone.Name != "alice" || clone.Email != "a@x.com" {
		t.Errorf("clone should be decoupled from original; got %+v", clone)
	}
}

func TestCaptureOld_SimpleEntity(t *testing.T) {
	e := &simpleEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(NewID(uuid.NewString()))
	ensureInit(e)

	captureOld(e)
	e.Name = "bob"

	prev := e.Old()
	if prev == nil {
		t.Fatal("expected Old() to return the snapshot")
	}
	pe := prev.(*simpleEntity)
	if pe.Name != "alice" {
		t.Errorf("snapshot should hold pre-mutation Name, got %q", pe.Name)
	}
	if e.Name != "bob" {
		t.Errorf("original should still reflect the post-mutation Name, got %q", e.Name)
	}
}

func TestOld_ReturnsTypedSnapshot(t *testing.T) {
	e := &simpleEntity{Name: "alice"}
	ensureInit(e)
	captureOld(e)
	e.Name = "bob"

	old := Old(e)
	if old == nil {
		t.Fatal("expected Old to return non-nil")
	}
	if old.Name != "alice" {
		t.Errorf("expected typed Old.Name=alice, got %q", old.Name)
	}
}

func TestOld_ReturnsZeroWhenNoSnapshot(t *testing.T) {
	e := &simpleEntity{Name: "alice"}
	ensureInit(e)
	// No captureOld — Insert path.

	old := Old(e)
	if old != nil {
		t.Errorf("expected nil typed Old when no snapshot, got %+v", old)
	}
}

// aggEntity exercises captureOld on the AggregateRoot path. Address-like
// VO with one field; AggregateChildren reports it. Used to test that the
// pre-mutation aggregate items are preserved in Old().
type aggChild struct {
	Managed
	Label string
}

func (a aggChild) BuildRules(string, Service, *Rules) {}

type aggEntity struct {
	AggregateRoot
	Name string
}

func (a *aggEntity) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}
func (a *aggEntity) BuildRules(string, Service, *Rules) {}
func (a *aggEntity) GetAggregateRoot() *AggregateRoot   { return &a.AggregateRoot }
func (a *aggEntity) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{aggChild{}}
}

func TestCaptureOld_AggregateIncludesChildren(t *testing.T) {
	e := &aggEntity{Name: "root"}
	e.SetID(NewID(uuid.NewString()))
	ensureInit(e)
	e.AggregateConstructor([]AggregateValueObject{
		aggChild{Label: "home"},
		aggChild{Label: "work"},
	})

	captureOld(e)

	// Mutate the original aggregate AFTER the snapshot.
	e.Name = "renamed"
	RemoveAggregateChild(e, aggChild{Label: "home"})

	old := Old(e)
	if old == nil {
		t.Fatal("expected Old to return non-nil for aggregate")
	}
	if old.Name != "root" {
		t.Errorf("snapshot root field should remain 'root', got %q", old.Name)
	}
	prevChildren := GetCurrentItemsOf[aggChild](&old.AggregateRoot)
	if len(prevChildren) != 2 {
		t.Errorf("expected snapshot to hold 2 children, got %d", len(prevChildren))
	}
	// Live aggregate now has 1 active child (the other was removed).
	live := GetCurrentItemsOf[aggChild](&e.AggregateRoot)
	if len(live) != 1 {
		t.Errorf("expected live aggregate to have 1 active child after Remove, got %d", len(live))
	}
}
