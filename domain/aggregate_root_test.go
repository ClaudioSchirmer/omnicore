package domain

import "testing"

type testAVO struct {
	Managed
	Name string
}

func (t testAVO) BuildRules(_ string, _ Service, _ *Rules) {}

// otherAVO is an AVO not declared in providerForTest's AggregateChildren list.
// Used to exercise the type-guard rejection path.
type otherAVO struct {
	Managed
}

func (o otherAVO) BuildRules(_ string, _ Service, _ *Rules) {}

// providerForTest is the minimal AggregateRootProvider used by the typed
// primitives tests. Declares only testAVO as a legitimate child.
type providerForTest struct {
	AggregateRoot
}

func (p *providerForTest) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (p *providerForTest) BuildRules(string, Service, *Rules) {}
func (p *providerForTest) GetAggregateRoot() *AggregateRoot   { return &p.AggregateRoot }
func (p *providerForTest) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{testAVO{}}
}

func newProviderForTest() *providerForTest {
	p := &providerForTest{}
	ensureInit(p)
	return p
}

func TestReplaceAggregateChildrenOf_FullReplace(t *testing.T) {
	p := newProviderForTest()
	p.AggregateConstructor([]AggregateValueObject{testAVO{Name: "a"}})

	newItems := []testAVO{{Name: "b"}, {Name: "c"}}
	ReplaceAggregateChildrenOf(p, newItems)

	added := GetAddedItemsOf[testAVO](&p.AggregateRoot)
	if len(added) != 2 {
		t.Fatalf("expected 2 added items, got %d", len(added))
	}
	removed := GetRemovedItemsOf[testAVO](&p.AggregateRoot)
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed item, got %d", len(removed))
	}
}

func TestReplaceAggregateChildrenOf_EmptyClears(t *testing.T) {
	p := newProviderForTest()
	p.AggregateConstructor([]AggregateValueObject{testAVO{}})

	ReplaceAggregateChildrenOf(p, []testAVO{})

	removed := GetRemovedItemsOf[testAVO](&p.AggregateRoot)
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed item, got %d", len(removed))
	}
	added := GetAddedItemsOf[testAVO](&p.AggregateRoot)
	if len(added) != 0 {
		t.Fatalf("expected 0 added items, got %d", len(added))
	}
}

func TestReplaceAggregateChildrenOf_NilSliceSameAsEmpty(t *testing.T) {
	p := newProviderForTest()
	p.AggregateConstructor([]AggregateValueObject{testAVO{}})

	var nilSlice []testAVO
	ReplaceAggregateChildrenOf(p, nilSlice)

	removed := GetRemovedItemsOf[testAVO](&p.AggregateRoot)
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed item, got %d", len(removed))
	}
	added := GetAddedItemsOf[testAVO](&p.AggregateRoot)
	if len(added) != 0 {
		t.Fatalf("expected 0 added items, got %d", len(added))
	}
}

func TestClassNameOfVO(t *testing.T) {
	if got := classNameOfVO[testAVO](); got != "testAVO" {
		t.Fatalf("expected %q, got %q", "testAVO", got)
	}
}

// ─── Phase 20: type-guard tests ─────────────────────────────────────────────

func TestAddAggregateChild_AcceptsDeclaredType(t *testing.T) {
	p := newProviderForTest()
	AddAggregateChild(p, testAVO{Name: "ok"})

	if p.NotificationContext().HasErrors() {
		t.Fatalf("expected no notifications, got %v", p.NotificationContext().Messages())
	}
	added := GetAddedItemsOf[testAVO](&p.AggregateRoot)
	if len(added) != 1 {
		t.Fatalf("expected 1 added item, got %d", len(added))
	}
}

func TestAddAggregateChild_RejectsUndeclaredType(t *testing.T) {
	p := newProviderForTest()
	AddAggregateChild(p, otherAVO{})

	msgs := p.NotificationContext().Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(msgs))
	}
	if NotificationKey(msgs[0].Notification) != "InvalidAggregateChildNotification" {
		t.Fatalf("expected InvalidAggregateChildNotification, got %s",
			NotificationKey(msgs[0].Notification))
	}
	// Map should not have received an otherAVO entry.
	if len(p.aggregates["otherAVO"]) != 0 {
		t.Fatalf("expected otherAVO entries to remain empty after rejection, got %d",
			len(p.aggregates["otherAVO"]))
	}
}

func TestChangeAggregateChild_RejectsUndeclaredType(t *testing.T) {
	p := newProviderForTest()
	ChangeAggregateChild(p, otherAVO{}, otherAVO{})

	msgs := p.NotificationContext().Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(msgs))
	}
	if NotificationKey(msgs[0].Notification) != "InvalidAggregateChildNotification" {
		t.Fatalf("expected InvalidAggregateChildNotification, got %s",
			NotificationKey(msgs[0].Notification))
	}
}

func TestRemoveAggregateChild_RejectsUndeclaredType(t *testing.T) {
	p := newProviderForTest()
	RemoveAggregateChild(p, otherAVO{})

	msgs := p.NotificationContext().Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(msgs))
	}
	if NotificationKey(msgs[0].Notification) != "InvalidAggregateChildNotification" {
		t.Fatalf("expected InvalidAggregateChildNotification, got %s",
			NotificationKey(msgs[0].Notification))
	}
}

func TestReplaceAggregateChildrenOf_RejectsUndeclaredType(t *testing.T) {
	p := newProviderForTest()
	// otherAVO is not declared. Clear of "otherAVO" runs (no-op), then each
	// item is rejected and not added.
	ReplaceAggregateChildrenOf(p, []otherAVO{{}, {}})

	msgs := p.NotificationContext().Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 notifications (one per rejected item), got %d", len(msgs))
	}
	for i, m := range msgs {
		if NotificationKey(m.Notification) != "InvalidAggregateChildNotification" {
			t.Fatalf("msg[%d] expected InvalidAggregateChildNotification, got %s",
				i, NotificationKey(m.Notification))
		}
	}
}

// ─── Phase 20: ValidateAggregateChild (optional inline validation) ──────────

type emittingAVO struct {
	Managed
	emit string
}

func (e emittingAVO) BuildRules(_ string, _ Service, r *Rules) {
	if e.emit != "" {
		r.AddNotification(e.emit, RequiredFieldNotification{})
	}
}

type emittingProvider struct {
	AggregateRoot
}

func (e *emittingProvider) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (e *emittingProvider) BuildRules(string, Service, *Rules) {}
func (e *emittingProvider) GetAggregateRoot() *AggregateRoot   { return &e.AggregateRoot }
func (e *emittingProvider) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{emittingAVO{}}
}

func newEmittingProvider() *emittingProvider {
	p := &emittingProvider{}
	ensureInit(p)
	return p
}

func TestValidateAggregateChild_NoEmitReturnsTrue(t *testing.T) {
	p := newEmittingProvider()
	if !ValidateAggregateChild(p, emittingAVO{}, "GetInsertable", nil) {
		t.Fatal("expected ValidateAggregateChild to return true when BuildRules emits nothing")
	}
	if p.NotificationContext().HasErrors() {
		t.Fatalf("expected no notifications, got %v", p.NotificationContext().Messages())
	}
}

func TestValidateAggregateChild_EmitsReturnsFalse(t *testing.T) {
	p := newEmittingProvider()
	if ValidateAggregateChild(p, emittingAVO{emit: "Street"}, "GetInsertable", nil) {
		t.Fatal("expected ValidateAggregateChild to return false when BuildRules emits")
	}
	msgs := p.NotificationContext().Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 notification accumulated on root via scoped ctx, got %d", len(msgs))
	}
	// Path is the camelCase collection segment + index 0: "emittingAVOs[0].street"
	want := "emittingAVOs[0].street"
	if got := msgs[0].ResolveFieldName(); got != want {
		t.Fatalf("expected field %q, got %q", want, got)
	}
}

func TestValidateAggregateChild_NilItemReturnsFalse(t *testing.T) {
	p := newEmittingProvider()
	if ValidateAggregateChild(p, nil, "GetInsertable", nil) {
		t.Fatal("expected nil item to yield false")
	}
}
