package domain

import (
	"reflect"
	"testing"
	"time"
)

// Tests for the value accessors on the ValidEntity flavors + metadata helpers.
// The seal types are only created by the domain package, so the tests
// construct them through the public Get* entry points or via the internal
// builder when richer setup is needed (file is in package domain).

func TestNewMetadata_DateTime(t *testing.T) {
	m := newMetadata()
	if m.dateTime.IsZero() {
		t.Error("expected dateTime to be set")
	}
	if delta := time.Since(m.dateTime); delta < 0 || delta > 5*time.Second {
		t.Errorf("dateTime drift = %v, expected near now", delta)
	}
}

func TestMetadata_PublicAccessors(t *testing.T) {
	when := time.Now().UTC()
	m := metadata{
		entityName: "User",
		actionName: "GetInsertable",
		dateTime:   when,
		events:     []DomainEvent{},
	}
	if got := m.EntityName(); got != "User" {
		t.Errorf("EntityName() = %q, want User", got)
	}
	if got := m.ActionName(); got != "GetInsertable" {
		t.Errorf("ActionName() = %q, want GetInsertable", got)
	}
	if got := m.DateTime(); !got.Equal(when) {
		t.Errorf("DateTime() = %v, want %v", got, when)
	}
	if got := m.Events(); !reflect.DeepEqual(got, []DomainEvent{}) {
		t.Errorf("Events() = %v, want empty slice", got)
	}
}

// ValidEntity flavors via the builder path — covers Source / ID / IDValue /
// IsPartial / AggregateInfo accessors and the entity() markers.

func TestInsertable_Accessors_WithoutAggregate(t *testing.T) {
	id := NewID("11111111-1111-1111-1111-111111111111")
	source := &plainEntity{}
	b := newBuilder("Plain", "GetInsertable", nil)
	ins := b.insertable(source, &id)

	ins.entity() // marker
	if ins.Source() != source {
		t.Error("Source() should return the registered entity")
	}
	if ins.ID() == nil || *ins.ID() != id {
		t.Errorf("ID() = %v, want %v", ins.ID(), id)
	}
	if _, ok := ins.AggregateInfo(); ok {
		t.Error("AggregateInfo() ok should be false when no aggregate attached")
	}
}

func TestInsertable_AggregateInfo_WithMeta(t *testing.T) {
	source := newProviderEntity()
	meta := extractAggregateMeta(source)
	if meta == nil {
		t.Fatal("expected meta from providerEntity")
	}

	b := newBuilder("providerEntity", "GetInsertable", nil).
		withAggregate(meta)
	ins := b.insertable(source, nil)

	root, ok := ins.AggregateInfo()
	if !ok {
		t.Fatal("AggregateInfo() should be ok=true when meta attached")
	}
	if root != &source.AggregateRoot {
		t.Error("AggregateInfo() should return the same *AggregateRoot pointer")
	}
}

func TestUpdatable_Accessors(t *testing.T) {
	id := NewID("22222222-2222-2222-2222-222222222222")
	source := &plainEntity{}
	b := newBuilder("Plain", "GetUpdatable", nil)

	u := b.updatable(source, id, false, ModeUpdate)
	u.entity()
	if u.Source() != source {
		t.Error("Source() mismatch")
	}
	if u.ID().Value() != id.Value() {
		t.Errorf("ID() = %q, want %q", u.ID(), id.Value())
	}
	if u.ID() != id {
		t.Errorf("ID() = %v, want %v", u.ID(), id)
	}
	if u.IsPartial() {
		t.Error("IsPartial() should be false for non-partial Updatable")
	}
	if _, ok := u.AggregateInfo(); ok {
		t.Error("AggregateInfo() should be false without meta")
	}

	partial := b.updatable(source, id, true, ModeUpdate)
	if !partial.IsPartial() {
		t.Error("IsPartial() should be true for partial Updatable")
	}
}

func TestArchivable_Accessors(t *testing.T) {
	id := NewID("33333333-3333-3333-3333-333333333333")
	source := &plainEntity{}
	a := newBuilder("Plain", "GetArchivable", nil).archivable(source, id)
	a.entity()
	if a.Source() != source {
		t.Error("Archivable.Source() mismatch")
	}
	if a.ID() != id {
		t.Errorf("Archivable.ID() = %q, want %q", a.ID(), id.Value())
	}
	if a.ID() != id {
		t.Errorf("Archivable.ID() = %v, want %v", a.ID(), id)
	}
}

func TestDeletable_Accessors(t *testing.T) {
	id := NewID("44444444-4444-4444-4444-444444444444")
	source := &plainEntity{}
	d := newBuilder("Plain", "GetDeletable", nil).deletable(source, id)
	d.entity()
	if d.Source() != source {
		t.Error("Deletable.Source() mismatch")
	}
	if d.ID() != id {
		t.Errorf("Deletable.ID() = %v, want %v", d.ID(), id)
	}
}

func TestUnarchivable_Accessors(t *testing.T) {
	id := NewID("55555555-5555-5555-5555-555555555555")
	source := &plainEntity{}
	un := newBuilder("Plain", "GetUnarchivable", nil).unarchivable(source, id)
	un.entity()
	if un.Source() != source {
		t.Error("Unarchivable.Source() mismatch")
	}
	if un.ID() != id {
		t.Errorf("Unarchivable.ID() = %v, want %v", un.ID(), id)
	}
}

func TestAggregateInfo_NilMetaReturnsFalse(t *testing.T) {
	root, ok := aggregateInfo(nil)
	if ok || root != nil {
		t.Errorf("expected nil/false, got %v / %v", root, ok)
	}
}

func TestUpdatableArchivableDeletableUnarchivable_AggregateInfoWhenAttached(t *testing.T) {
	root := newProviderEntity()
	meta := extractAggregateMeta(root)
	if meta == nil {
		t.Fatal("expected meta")
	}
	id := NewRandomID()
	b := newBuilder("providerEntity", "X", nil).withAggregate(meta)

	u := b.updatable(root, id, false, ModeUpdate)
	a := b.archivable(root, id)
	d := b.deletable(root, id)
	un := b.unarchivable(root, id)

	for _, ag := range []func() (*AggregateRoot, bool){
		u.AggregateInfo, a.AggregateInfo, d.AggregateInfo, un.AggregateInfo,
	} {
		gotRoot, ok := ag()
		if !ok || gotRoot != &root.AggregateRoot {
			t.Errorf("AggregateInfo should propagate root from meta, got %v/%v", gotRoot, ok)
		}
	}
}

func TestExtractAggregateMeta_NilForNonProvider(t *testing.T) {
	if got := extractAggregateMeta(&plainEntity{}); got != nil {
		t.Errorf("expected nil meta for non-provider entity, got %+v", got)
	}
}
