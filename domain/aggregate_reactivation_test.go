package domain

import "testing"

// narrowAVO is an aggregate child whose business identity is a NARROW natural
// key (Key alone) — Label is a non-identity field. This is the shape that
// exposes the reactivation contract: a same-identity re-send that changes Label
// must persist the new Label, not silently keep the tracked one.
type narrowAVO struct {
	Managed
	Key   string
	Label string
}

func (n narrowAVO) BuildRules(string, Service, *Rules) {}

func (n narrowAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	o, ok := other.(narrowAVO)
	return ok && n.Key == o.Key
}

type narrowProvider struct{ AggregateRoot }

func (p *narrowProvider) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (p *narrowProvider) BuildRules(string, Service, *Rules) {}
func (p *narrowProvider) GetAggregateRoot() *AggregateRoot   { return &p.AggregateRoot }
func (p *narrowProvider) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{narrowAVO{}}
}

func newNarrowProvider() *narrowProvider {
	p := &narrowProvider{}
	ensureInit(p)
	return p
}

// Fix #1: a full-replace PUT that re-sends a child with the SAME business
// identity but a CHANGED non-identity field must update in place (id preserved)
// AND persist the new field value — never archive-old + insert-new, never drop
// the change back to the tracked value.
func TestReplaceAggregateChildren_ReactivationPreservesIDAndAppliesChange(t *testing.T) {
	p := newNarrowProvider()

	loaded := narrowAVO{Key: "k1", Label: "old"}
	loaded.SetID(NewID("id1"))
	p.AggregateConstructor([]AggregateValueObject{loaded})

	// Full replace re-sending the same identity (Key="k1") with a new Label and
	// no id (exactly what a strict PUT of the whole document carries).
	ReplaceAggregateChildrenOf(p, []narrowAVO{{Key: "k1", Label: "new"}})

	// Resolves to an in-place UPDATE, not an insert and not a delete.
	changed := GetChangedItemsOf[narrowAVO](&p.AggregateRoot)
	if len(changed) != 1 {
		t.Fatalf("expected 1 UPDATE (in-place reactivation), got %d", len(changed))
	}
	if added := GetAddedItemsOf[narrowAVO](&p.AggregateRoot); len(added) != 0 {
		t.Fatalf("expected 0 INSERTs (no id churn), got %d", len(added))
	}
	if removed := GetRemovedItemsOf[narrowAVO](&p.AggregateRoot); len(removed) != 0 {
		t.Fatalf("expected 0 DELETEs (no archive-old churn), got %d", len(removed))
	}

	got := changed[0]
	if got.Label != "new" {
		t.Errorf("non-identity change dropped: Label = %q, want %q", got.Label, "new")
	}
	if got.GetID().Value() != "id1" {
		t.Errorf("tracked id not preserved: GetID() = %q, want %q", got.GetID().Value(), "id1")
	}
}

// Fix #1 edge: a NEVER-PERSISTED item (added, then removed, then re-added in the
// same unit of work) has an empty id, so reactivation carries the re-sent values
// as a fresh INSERT — no stale value, no phantom id.
func TestReactivation_NeverPersistedItemCarriesResentValuesAsInsert(t *testing.T) {
	p := newNarrowProvider()

	AddAggregateChild(p, narrowAVO{Key: "k1", Label: "first"})
	RemoveAggregateChild(p, narrowAVO{Key: "k1"})
	AddAggregateChild(p, narrowAVO{Key: "k1", Label: "second"})

	added := GetAddedItemsOf[narrowAVO](&p.AggregateRoot)
	if len(added) != 1 {
		t.Fatalf("expected 1 INSERT, got %d", len(added))
	}
	if added[0].Label != "second" {
		t.Errorf("re-sent value dropped: Label = %q, want %q", added[0].Label, "second")
	}
	if !added[0].GetID().IsEmpty() {
		t.Errorf("never-persisted item must have no id, got %q", added[0].GetID().Value())
	}
}

// ptrCarrierAVO embeds the managed carrier by POINTER instead of by value.
type ptrCarrierAVO struct {
	*Managed
	Name string
}

func (a ptrCarrierAVO) BuildRules(string, Service, *Rules) {}
func (a ptrCarrierAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	return IsSameByBusinessFields(a, other)
}

// Fix #13: isManagedCarrier must skip a pointer-embedded *Managed too, so its id
// and timestamps never leak into structural business identity.
func TestIsSameByBusinessFields_SkipsPointerManagedCarrier(t *testing.T) {
	a := ptrCarrierAVO{Managed: &Managed{}, Name: "x"}
	b := ptrCarrierAVO{Managed: &Managed{}, Name: "x"}
	a.SetID(NewID("id-a"))
	b.SetID(NewID("id-b"))

	if !IsSameByBusinessFields(a, b) {
		t.Fatal("pointer-embedded Managed leaked into identity: same Name should be same identity regardless of id")
	}

	c := ptrCarrierAVO{Managed: &Managed{}, Name: "y"}
	if IsSameByBusinessFields(a, c) {
		t.Fatal("different Name must not be the same identity")
	}
}
