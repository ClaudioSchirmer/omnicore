package domain

import (
	"reflect"
	"testing"
)

// --- aggregate_root private mutators: nil + not-found branches --------------

func TestAddAggregateItem_NilIgnored(t *testing.T) {
	p := newProviderForTest()
	p.addAggregateItem(nil) // isAggregateItemValid(nil) → false → no-op
	if len(p.AllAggregateItems()) != 0 {
		t.Fatalf("nil item must not enter the collection, got %v", p.AllAggregateItems())
	}
}

func TestChangeAggregateItem_NilIgnored(t *testing.T) {
	p := newProviderForTest()
	p.changeAggregateItem(nil, nil) // both invalid → no-op
	if len(p.AllAggregateItems()) != 0 {
		t.Fatalf("nil change must be a no-op, got %v", p.AllAggregateItems())
	}
}

func TestChangeAggregateChild_OriginalNotInCollection(t *testing.T) {
	p := newProviderForTest()
	// Both are declared types, but the original was never added → not-found
	// path emits EntityDoesNotExistNotification.
	ChangeAggregateChild(p, testAVO{}, testAVO{})
	if !p.NotificationContext().HasErrors() {
		t.Fatal("changing a non-existent child must emit a notification")
	}
}

func TestChangeAggregateChild_ReplacementUndeclared(t *testing.T) {
	p := newProviderForTest()
	p.AggregateConstructor([]AggregateValueObject{testAVO{}})
	// original declared, replacement is an undeclared type → reject replacement.
	ChangeAggregateChild(p, testAVO{}, otherAVO{})
	if !p.NotificationContext().HasErrors() {
		t.Fatal("undeclared replacement must be rejected")
	}
}

func TestRemoveAggregateItem_NilIgnored(t *testing.T) {
	p := newProviderForTest()
	p.removeAggregateItem(nil) // nil → EntityDoesNotExist, no panic
	if !p.NotificationContext().HasErrors() {
		t.Fatal("removing nil must emit a notification")
	}
}

func TestRemoveAggregateChild_NotInCollection(t *testing.T) {
	p := newProviderForTest()
	// Declared type but never added → not-found path.
	RemoveAggregateChild(p, testAVO{})
	if !p.NotificationContext().HasErrors() {
		t.Fatal("removing an absent child must emit a notification")
	}
}

func TestRemoveAggregateChild_NilRejected(t *testing.T) {
	p := newProviderForTest()
	RemoveAggregateChild(p, nil) // isAllowedChild(nil)=false → rejectChild
	if !p.NotificationContext().HasErrors() {
		t.Fatal("removing a nil child must be rejected")
	}
}

func TestClassNameOfVO_PointerType(t *testing.T) {
	if got := classNameOfVO[*testAVO](); got != "testAVO" {
		t.Fatalf("classNameOfVO[*testAVO] = %q, want testAVO", got)
	}
}

// --- clone defensive branches ----------------------------------------------

func TestCloneEntity_Nil(t *testing.T) {
	if got := cloneEntity(nil); got != nil {
		t.Fatalf("cloneEntity(nil) = %v, want nil", got)
	}
}

func TestCaptureOld_Nil(t *testing.T) {
	captureOld(nil) // must not panic
}

func TestCopyAggregateMap_Nil(t *testing.T) {
	if got := copyAggregateMap(nil); got != nil {
		t.Fatalf("copyAggregateMap(nil) = %v, want nil", got)
	}
}

// unmarshalableEntity carries an exported channel field, so json.Marshal of
// the entity fails — exercising cloneEntity's marshal-error fallback (and, via
// captureOld, the clone==nil early return).
type unmarshalableEntity struct {
	BaseEntity
	Bad chan int `json:"bad"`
}

func (u *unmarshalableEntity) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (u *unmarshalableEntity) BuildRules(string, Service, *Rules) {}

func TestCloneEntity_MarshalFailureReturnsNil(t *testing.T) {
	e := &unmarshalableEntity{Bad: make(chan int)}
	ensureInit(e)
	if got := cloneEntity(e); got != nil {
		t.Fatalf("cloneEntity of unmarshalable entity = %v, want nil", got)
	}
	// captureOld must swallow the nil clone without setting Old or panicking.
	captureOld(e)
	if e.Old() != nil {
		t.Fatal("captureOld must leave Old() nil when the clone fails")
	}
}

func TestOld_TypeMismatchReturnsZero(t *testing.T) {
	// Set the old-state ghost to a different concrete type than the generic
	// parameter requested → the type assertion fails → zero value.
	host := &plainEntity{}
	ensureInit(host)
	host.setOldEntity(&unmarshalableEntity{})
	if got := Old[*plainEntity](host); got != nil {
		t.Fatalf("Old with mismatched ghost type = %v, want nil", got)
	}
}

// --- notification_vars pointer notification branches ------------------------

func TestExtractVarsFromTags_PointerNotification(t *testing.T) {
	got := ExtractVarsFromTags(&singleTvarNotif{MaxLength: 9})
	if got["maxLength"] != "9" {
		t.Fatalf("pointer notification vars = %v, want maxLength=9", got)
	}
}

func TestExtractVarsFromTags_TypedNilPointerReturnsNil(t *testing.T) {
	var n *singleTvarNotif // typed nil — has a tvar plan but a nil value
	if got := ExtractVarsFromTags(n); got != nil {
		t.Fatalf("typed nil pointer notification = %v, want nil", got)
	}
}

// --- rules: AddNotificationWithVars nil-context guard -----------------------

func TestAddNotificationWithVars_NilContextNoop(t *testing.T) {
	r := NewRules(ModeInsert, nil, reflect.TypeOf(&plainEntity{}))
	// ctx is nil → the method returns immediately without panicking.
	r.AddNotificationWithVars(nil, RequiredFieldNotification{}, map[string]string{"k": "v"}, false)
}

// --- entity_base mode + id rejection branches -------------------------------

// updateDeleteServiceEntity requires a service and supports update/delete, so
// GetUpdatable/GetDeletable with a nil service trip the checkService branch in
// validateForUpdate / validateForDelete respectively.
type updateDeleteServiceEntity struct {
	BaseEntity
}

func (u *updateDeleteServiceEntity) Modes() []EntityMode {
	return []EntityMode{ModeUpdate, ModeDelete}
}
func (u *updateDeleteServiceEntity) BuildRules(string, Service, *Rules) {}
func (u *updateDeleteServiceEntity) RequiresService() bool              { return true }

func TestValidateForUpdate_NilServiceRejected(t *testing.T) {
	e := &updateDeleteServiceEntity{}
	e.SetID(NewID("abc"))
	if _, err := GetUpdatable(e, func(*updateDeleteServiceEntity) error { return nil }, nil, "GetUpdatable"); err == nil {
		t.Fatal("expected GetUpdatable to fail when a required service is nil")
	}
}

func TestValidateForDelete_NilServiceRejected(t *testing.T) {
	e := &updateDeleteServiceEntity{}
	e.SetID(NewID("abc"))
	if _, err := GetDeletable(e, nil, "GetDeletable"); err == nil {
		t.Fatal("expected GetDeletable to fail when a required service is nil")
	}
}

// insertDisallowedEntity supports only update — GetInsertable must surface
// InsertNotAllowedNotification (the !modeAllowed branch).
type insertDisallowedEntity struct {
	BaseEntity
}

func (i *insertDisallowedEntity) Modes() []EntityMode                { return []EntityMode{ModeUpdate} }
func (i *insertDisallowedEntity) BuildRules(string, Service, *Rules) {}

func TestValidateForInsert_ModeNotAllowed(t *testing.T) {
	e := &insertDisallowedEntity{}
	_, err := GetInsertable(e, nil, "GetInsertable")
	if err == nil {
		t.Fatal("expected GetInsertable to fail when ModeInsert is not declared")
	}
}

func TestValidateForInsert_WithIDRejected(t *testing.T) {
	e := &plainEntity{} // declares ModeInsert
	e.SetID(NewID("already-has-id"))
	_, err := GetInsertable(e, nil, "GetInsertable")
	if err == nil {
		t.Fatal("expected GetInsertable to reject an entity that already carries an ID")
	}
}
