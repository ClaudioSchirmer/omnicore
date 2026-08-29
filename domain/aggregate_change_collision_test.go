package domain

import "testing"

// keyedAVO is the natural-key shape the collision guard is about: identity is
// Key alone, Payload is outside it. The package's other fixtures delegate to
// IsSameByBusinessFields (every field is identity), which cannot express the
// two cases that matter here — a change that keeps the identity and edits the
// payload, and a change that moves the entry onto another entry's identity.
type keyedAVO struct {
	Managed
	Key     string
	Payload string
}

func (keyedAVO) BuildRules(string, Service, *Rules) {}
func (keyedAVO) CollectionName() string             { return "KeyedAVOs" }

func (k keyedAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	o, ok := other.(keyedAVO)
	return ok && k.Key == o.Key
}

type keyedProvider struct {
	AggregateRoot
}

func (p *keyedProvider) Modes() []EntityMode                { return []EntityMode{ModeUpdate} }
func (p *keyedProvider) BuildRules(string, Service, *Rules) {}
func (p *keyedProvider) GetAggregateRoot() *AggregateRoot   { return &p.AggregateRoot }
func (p *keyedProvider) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{keyedAVO{}}
}

// newKeyedProvider seeds two DB-loaded children with distinct identities and
// distinct row ids — the shape every addressed-by-id verb operates on.
func newKeyedProvider() *keyedProvider {
	p := &keyedProvider{}
	ensureInit(p)
	p.AggregateConstructor([]AggregateValueObject{
		WithID(keyedAVO{Key: "a", Payload: "first"}, NewID("row-a")),
		WithID(keyedAVO{Key: "b", Payload: "second"}, NewID("row-b")),
	})
	return p
}

func keyedItems(p *keyedProvider) []AggregateItem[keyedAVO] {
	return GetAggregateItemsOf[keyedAVO](&p.AggregateRoot)
}

func onlyNotification(t *testing.T, p *keyedProvider) string {
	t.Helper()
	msgs := p.NotificationContext().Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 notification, got %d (%v)", len(msgs), msgs)
	}
	return NotificationKey(msgs[0].Notification)
}

// TestChangeAggregateChild_RejectsIdentityCollisionWithActiveSibling is the
// guard itself: moving an entry onto an identity another ACTIVE entry already
// holds would leave two active children matching the same identity — the state
// every match site resolves "the entry that matches" against.
func TestChangeAggregateChild_RejectsIdentityCollisionWithActiveSibling(t *testing.T) {
	p := newKeyedProvider()
	items := keyedItems(p)

	// The caller addressed row-b and sent row-a's identity.
	ChangeAggregateChild(p, items[1].Item, WithID(keyedAVO{Key: "a", Payload: "moved"}, NewID("row-b")))

	if got := onlyNotification(t, p); got != "EntityAlreadyAddedNotification" {
		t.Fatalf("expected EntityAlreadyAddedNotification, got %s", got)
	}
	after := keyedItems(p)
	if len(after) != 2 {
		t.Fatalf("expected the collection untouched at 2 entries, got %d", len(after))
	}
	for _, it := range after {
		if it.CurrentStatus != StatusConstructor {
			t.Fatalf("a rejected change must write nothing; entry %q is %s",
				it.Item.Key, it.CurrentStatus)
		}
	}
	if after[1].Item.Payload != "second" {
		t.Fatalf("the addressed entry kept its value; got payload %q", after[1].Item.Payload)
	}
}

// TestChangeAggregateChild_AllowsIdentityChangeIntoFreeIdentity pins the
// semantics the guard does NOT change: identity finds the entry, it does not
// freeze it. An aggregate keyed by a natural key edits those very fields here.
func TestChangeAggregateChild_AllowsIdentityChangeIntoFreeIdentity(t *testing.T) {
	p := newKeyedProvider()
	items := keyedItems(p)

	ChangeAggregateChild(p, items[0].Item, WithID(keyedAVO{Key: "c", Payload: "renamed"}, NewID("row-a")))

	if p.NotificationContext().HasErrors() {
		t.Fatalf("expected the change accepted, got %v", p.NotificationContext().Messages())
	}
	changed := GetChangedItemsOf[keyedAVO](&p.AggregateRoot)
	if len(changed) != 1 || changed[0].Key != "c" || changed[0].GetID().Value() != "row-a" {
		t.Fatalf("expected row-a changed to key c, got %+v", changed)
	}
}

// TestChangeAggregateChild_AllowsPayloadOnlyChange covers the target matching
// the replacement's identity: the entry collides with itself, which is not a
// collision.
func TestChangeAggregateChild_AllowsPayloadOnlyChange(t *testing.T) {
	p := newKeyedProvider()
	items := keyedItems(p)

	ChangeAggregateChild(p, items[0].Item, WithID(keyedAVO{Key: "a", Payload: "edited"}, NewID("row-a")))

	if p.NotificationContext().HasErrors() {
		t.Fatalf("expected the change accepted, got %v", p.NotificationContext().Messages())
	}
	changed := GetChangedItemsOf[keyedAVO](&p.AggregateRoot)
	if len(changed) != 1 || changed[0].Payload != "edited" {
		t.Fatalf("expected the payload edited in place, got %+v", changed)
	}
}

// TestChangeAggregateChild_RemovedSiblingDoesNotCollide mirrors the add path,
// where a matching REMOVED entry re-activates instead of conflicting: an entry
// on its way out does not hold its identity against a change.
func TestChangeAggregateChild_RemovedSiblingDoesNotCollide(t *testing.T) {
	p := newKeyedProvider()
	items := keyedItems(p)

	RemoveAggregateChild(p, items[0].Item) // row-a leaves
	ChangeAggregateChild(p, items[1].Item, WithID(keyedAVO{Key: "a", Payload: "took over"}, NewID("row-b")))

	if p.NotificationContext().HasErrors() {
		t.Fatalf("expected the change accepted, got %v", p.NotificationContext().Messages())
	}
	changed := GetChangedItemsOf[keyedAVO](&p.AggregateRoot)
	if len(changed) != 1 || changed[0].GetID().Value() != "row-b" || changed[0].Key != "a" {
		t.Fatalf("expected row-b changed to key a, got %+v", changed)
	}
	if removed := GetRemovedItemsOf[keyedAVO](&p.AggregateRoot); len(removed) != 1 {
		t.Fatalf("expected row-a still removed, got %d removed", len(removed))
	}
}

// TestChangeAggregateChild_AbsentOriginalAnswersNotFoundNotCollision: the
// caller addressed an entry that is not there, and that is the answer — the
// collision check never gets to speak for a change that has no target.
func TestChangeAggregateChild_AbsentOriginalAnswersNotFoundNotCollision(t *testing.T) {
	p := newKeyedProvider()

	ChangeAggregateChild(p, keyedAVO{Key: "ghost"}, keyedAVO{Key: "a", Payload: "moved"})

	if got := onlyNotification(t, p); got != "EntityDoesNotExistNotification" {
		t.Fatalf("expected EntityDoesNotExistNotification, got %s", got)
	}
}
