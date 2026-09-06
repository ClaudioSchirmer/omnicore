package domain

import (
	"errors"
	"testing"
)

// ─── Sealed interface marker methods (invoked through their interfaces) ──────

func TestValidEntity_MarkerMethods(t *testing.T) {
	// Each sealed ValidEntity flavor satisfies the interface via its private
	// entity() marker; invoking it through the interface exercises the seal.
	markers := []ValidEntity{
		Insertable{},
		Updatable{},
		Archivable{},
		Deletable{},
		Unarchivable{},
	}
	for _, ve := range markers {
		ve.entity()
	}
}

func TestNotificationBase_IsNotificationMarker(t *testing.T) {
	var n Notification = NotificationBase{}
	n.isNotification()
}

func TestServiceBase_IsServiceMarker(t *testing.T) {
	var s Service = ServiceBase{}
	s.isService()
}

// ─── NotificationKey: pointer notification unwraps to the element name ───────

func TestNotificationKey_PointerStripsPointer(t *testing.T) {
	if got := NotificationKey(&RequiredFieldNotification{}); got != "RequiredFieldNotification" {
		t.Fatalf("expected pointer notification to resolve to element name, got %q", got)
	}
}

// ─── ExtractVarsFromTags: a non-struct dynamic type yields nil ──────────────

type nonStructNotif int

func (nonStructNotif) isNotification()                {}
func (nonStructNotif) Semantic() NotificationSemantic { return SemanticValidation }

func TestExtractVarsFromTags_NonStructKindReturnsNil(t *testing.T) {
	if got := ExtractVarsFromTags(nonStructNotif(7)); got != nil {
		t.Fatalf("expected nil for a non-struct notification kind, got %v", got)
	}
}

// ─── getArchivable / getUnarchivable: missing ID emits the no-ID notification ─

func TestGetArchivable_MissingIDEmitsNotification(t *testing.T) {
	e := &archivableTestEntity{modes: []EntityMode{ModeArchive}} // no SetID
	_, err := GetArchivable(e, nil, "GetArchivable")
	if err == nil {
		t.Fatal("expected GetArchivable to fail without an ID")
	}
	if !hasNotification(err, "UnableToDeleteWithoutIDNotification") {
		t.Fatalf("expected UnableToDeleteWithoutIDNotification, got %v", err)
	}
}

func TestGetUnarchivable_MissingIDEmitsNotification(t *testing.T) {
	e := &archivableTestEntity{modes: []EntityMode{ModeUnarchive}} // no SetID
	_, err := GetUnarchivable(e, nil, "GetUnarchivable")
	if err == nil {
		t.Fatal("expected GetUnarchivable to fail without an ID")
	}
	if !hasNotification(err, "UnableToDeleteWithoutIDNotification") {
		t.Fatalf("expected UnableToDeleteWithoutIDNotification, got %v", err)
	}
}

// ─── applyFieldAliases: aliases declared before the framework names the root ─

type aliasOnlyEntity struct {
	BaseEntity
}

func (a *aliasOnlyEntity) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (a *aliasOnlyEntity) BuildRules(string, Service, *Rules) {}

func TestApplyFieldAliases_AppliesBeforeFrameworkEntry(t *testing.T) {
	e := &aliasOnlyEntity{}
	// Both the alias and the notification are declared before any framework
	// call — the rename must still land.
	e.AddFieldNameAlias("Orig", "new")
	e.AddNotificationMessage(NotificationMessage{
		Path:         []PathSegment{{Name: "Orig"}},
		Notification: RequiredFieldNotification{},
	})

	applyFieldAliases(e)

	if got := e.NotificationContext().Messages()[0].ResolveFieldName(); got != "new" {
		t.Errorf("field name after alias = %q, want %q", got, "new")
	}
}

func TestApplyFieldAliases_NoAliasesIsNoOp(t *testing.T) {
	e := &aliasOnlyEntity{}
	applyFieldAliases(e) // must not panic on an entity that declared nothing
}

// ─── cloneEntity: a failing UnmarshalJSON degrades to nil ────────────────────

type failUnmarshalField struct{}

func (failUnmarshalField) MarshalJSON() ([]byte, error) { return []byte(`"x"`), nil }
func (*failUnmarshalField) UnmarshalJSON([]byte) error  { return errors.New("boom") }

type cloneFailEntity struct {
	BaseEntity
	F failUnmarshalField
}

func (c *cloneFailEntity) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (c *cloneFailEntity) BuildRules(string, Service, *Rules) {}

func TestCloneEntity_UnmarshalFailureReturnsNil(t *testing.T) {
	if got := cloneEntity(&cloneFailEntity{}); got != nil {
		t.Fatalf("expected nil clone when UnmarshalJSON fails, got %#v", got)
	}
}

// ─── rejectChild / ValidateAggregateChild with a nil AggregateRoot ──────────

type nilARProvider struct{}

func (nilARProvider) GetAggregateRoot() *AggregateRoot          { return nil }
func (nilARProvider) AggregateChildren() []AggregateValueObject { return nil }

func TestRejectChild_NilAggregateRootNoOp(t *testing.T) {
	// Must not panic — the nil-AR guard returns early.
	rejectChild(nilARProvider{}, testAVO{})
}

func TestValidateAggregateChild_NilAggregateRootReturnsFalse(t *testing.T) {
	if ValidateAggregateChild(nilARProvider{}, testAVO{}, ModeInsert, "GetInsertable", nil) {
		t.Fatal("expected false when the root's AggregateRoot is nil")
	}
}

// ─── ValidateAggregateChild: a root that is not an Entity ───────────────────

type nonEntityProvider struct {
	AggregateRoot
}

func (p *nonEntityProvider) GetAggregateRoot() *AggregateRoot { return &p.AggregateRoot }
func (p *nonEntityProvider) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{testAVO{}}
}

func TestValidateAggregateChild_NonEntityRootStillValidates(t *testing.T) {
	// nonEntityProvider does not implement Entity, so ensureRootInit cannot name
	// its context. Validation runs anyway: the context exists from birth, so the
	// child is judged on its own merits instead of being rejected wholesale.
	p := &nonEntityProvider{}
	if !ValidateAggregateChild(p, testAVO{}, ModeInsert, "GetInsertable", nil) {
		t.Fatalf("expected true for a child that emits nothing, got the root's messages: %+v",
			p.NotificationContext().Messages())
	}
}

// ─── ValidateAggregateChild: existing non-removed items advance the index ────

func TestValidateAggregateChild_CountsExistingItemsForIndex(t *testing.T) {
	p := newEmittingProvider()
	// Seed one existing (Added, non-removed) child so the index counting loop
	// in ValidateAggregateChild iterates a populated collection.
	AddAggregateChild(p, emittingAVO{})
	if p.NotificationContext().HasErrors() {
		t.Fatalf("seed add should not emit, got %v", p.NotificationContext().Messages())
	}
	// Validating a second item that emits should land at index 1.
	if ValidateAggregateChild(p, emittingAVO{emit: "Street"}, ModeInsert, "GetInsertable", nil) {
		t.Fatal("expected false when BuildRules emits")
	}
	msgs := p.NotificationContext().Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(msgs))
	}
	if got, want := msgs[0].ResolveFieldName(), "emittingAVOs[1].street"; got != want {
		t.Fatalf("expected field %q (index advanced past existing item), got %q", want, got)
	}
}
