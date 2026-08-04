package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- ID ---------------------------------------------------------------------

func TestNewID_FromString(t *testing.T) {
	id := NewID("abc")
	if id.Value() != "abc" {
		t.Errorf("Value() = %q, want %q", id.Value(), "abc")
	}
}

func TestNewIDFromUUID(t *testing.T) {
	u := uuid.New()
	id := NewIDFromUUID(u)
	if id.Value() != u.String() {
		t.Errorf("Value() = %q, want %q", id.Value(), u.String())
	}
}

func TestID_String(t *testing.T) {
	id := NewID("abc-123")
	if got := id.String(); got != "abc-123" {
		t.Errorf("String() = %q, want %q", got, "abc-123")
	}
}

func TestID_IsEmpty(t *testing.T) {
	if !(ID{}.IsEmpty()) {
		t.Error("zero ID should be empty")
	}
	if NewID("abc").IsEmpty() {
		t.Error("non-zero ID should NOT be empty")
	}
}

func TestID_UUID_Parses(t *testing.T) {
	want := "11111111-2222-3333-4444-555555555555"
	id := NewID(want)
	got, err := id.UUID()
	if err != nil {
		t.Fatalf("UUID() err = %v", err)
	}
	if got.String() != want {
		t.Errorf("UUID() = %v, want %v", got, want)
	}
}

func TestID_UUID_RejectsInvalid(t *testing.T) {
	if _, err := NewID("not-a-uuid").UUID(); err == nil {
		t.Error("expected UUID() to fail on garbage input")
	}
}

func TestID_IsValid_Valid(t *testing.T) {
	id := NewID(uuid.NewString())
	ctx := NewNotificationContext("Test")
	if !id.IsValid("id", ctx) {
		t.Error("expected valid UUID to pass IsValid")
	}
	if ctx.HasErrors() {
		t.Errorf("expected no notification recorded, got %+v", ctx.Messages())
	}
}

func TestID_IsValid_Invalid_RecordsNotification(t *testing.T) {
	id := NewID("not-uuid")
	ctx := NewNotificationContext("Test")
	if id.IsValid("id", ctx) {
		t.Error("expected invalid UUID to fail IsValid")
	}
	if !ctx.HasErrors() {
		t.Error("expected notification to be recorded")
	}
	if got := NotificationKey(ctx.Messages()[0].Notification); got != "InvalidIDUUIDNotification" {
		t.Errorf("expected InvalidIDUUIDNotification, got %q", got)
	}
}

func TestID_IsValid_NilCtxNoCrash(t *testing.T) {
	id := NewID("not-uuid")
	if id.IsValid("id", nil) {
		t.Error("expected invalid UUID to still report false on nil ctx")
	}
}

// --- EntityMode -------------------------------------------------------------

func TestEntityMode_String(t *testing.T) {
	cases := []struct {
		mode EntityMode
		want string
	}{
		{ModeUnknown, "UNKNOWN"},
		{ModeDisplay, "DISPLAY"},
		{ModeInsert, "INSERT"},
		{ModeUpdate, "UPDATE"},
		{ModeDelete, "DELETE"},
		{ModeArchive, "ARCHIVE"},
		{ModeUnarchive, "UNARCHIVE"},
		{EntityMode(99), "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestEntityMode_IsValid(t *testing.T) {
	for _, m := range []EntityMode{ModeDisplay, ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive} {
		if !m.IsValid() {
			t.Errorf("%v should be IsValid()", m)
		}
	}
	for _, m := range []EntityMode{ModeUnknown, EntityMode(99)} {
		if m.IsValid() {
			t.Errorf("%v should NOT be IsValid()", m)
		}
	}
}

// --- AggregateItemStatus ----------------------------------------------------

func TestAggregateItemStatus_String(t *testing.T) {
	cases := []struct {
		s    AggregateItemStatus
		want string
	}{
		{StatusUnknown, "UNKNOWN"},
		{StatusConstructor, "CONSTRUCTOR"},
		{StatusAdded, "ADDED"},
		{StatusChanged, "CHANGED"},
		{StatusRemoved, "REMOVED"},
		{AggregateItemStatus(99), "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestAggregateItemStatus_IsValid(t *testing.T) {
	for _, s := range []AggregateItemStatus{StatusConstructor, StatusAdded, StatusChanged, StatusRemoved} {
		if !s.IsValid() {
			t.Errorf("%v should be IsValid()", s)
		}
	}
	for _, s := range []AggregateItemStatus{StatusUnknown, AggregateItemStatus(99)} {
		if s.IsValid() {
			t.Errorf("%v should NOT be IsValid()", s)
		}
	}
}

// --- AggregateItem helpers --------------------------------------------------

func TestNewAggregateItem_PreservesStatusOnBothFields(t *testing.T) {
	it := NewAggregateItem("payload", StatusAdded)
	if it.Item != "payload" || it.OriginalStatus != StatusAdded || it.CurrentStatus != StatusAdded {
		t.Errorf("NewAggregateItem returned %+v", it)
	}
}

func TestFilterByStatus(t *testing.T) {
	items := []AggregateItem[string]{
		{Item: "a", CurrentStatus: StatusAdded},
		{Item: "b", CurrentStatus: StatusChanged},
		{Item: "c", CurrentStatus: StatusRemoved},
		{Item: "d", CurrentStatus: StatusAdded},
	}
	got := FilterByStatus(items, StatusAdded)
	if len(got) != 2 || got[0] != "a" || got[1] != "d" {
		t.Errorf("FilterByStatus(Added) = %v, want [a d]", got)
	}
}

func TestGetCurrentItems_SkipsRemoved(t *testing.T) {
	items := []AggregateItem[int]{
		{Item: 1, CurrentStatus: StatusAdded},
		{Item: 2, CurrentStatus: StatusRemoved},
		{Item: 3, CurrentStatus: StatusConstructor},
	}
	got := GetCurrentItems(items)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("GetCurrentItems = %v, want [1 3]", got)
	}
}

func TestGetAddedChangedRemoved(t *testing.T) {
	// Categorization is by OperationOf(original, current): a new item (original
	// Added), a DB item changed (original Constructor → Changed), a DB item removed
	// (original Constructor → Removed).
	items := []AggregateItem[string]{
		{Item: "added", OriginalStatus: StatusAdded, CurrentStatus: StatusAdded},
		{Item: "changed", OriginalStatus: StatusConstructor, CurrentStatus: StatusChanged},
		{Item: "removed", OriginalStatus: StatusConstructor, CurrentStatus: StatusRemoved},
	}
	if got := GetAddedItems(items); len(got) != 1 || got[0] != "added" {
		t.Errorf("GetAddedItems = %v", got)
	}
	if got := GetChangedItems(items); len(got) != 1 || got[0] != "changed" {
		t.Errorf("GetChangedItems = %v", got)
	}
	if got := GetRemovedItems(items); len(got) != 1 || got[0] != "removed" {
		t.Errorf("GetRemovedItems = %v", got)
	}
}

// --- EventType + DomainEvent -----------------------------------------------

func TestEventType_Values(t *testing.T) {
	if int(EventLog) != 1 {
		t.Errorf("EventLog underlying = %d, want 1", int(EventLog))
	}
	members := EventLog.Values()
	if len(members) != 4 {
		t.Errorf("Values() len = %d, want 4", len(members))
	}
	// The Unknown sentinel is never a declared member.
	for _, m := range members {
		if m == EventUnknown {
			t.Error("EventUnknown must not appear in Values()")
		}
	}
}

func TestEventType_UnknownNotification(t *testing.T) {
	if n := EventLog.UnknownNotification(); NotificationKey(n) != "InvalidEventTypeNotification" {
		t.Errorf("UnknownNotification() = %T, want InvalidEventTypeNotification", n)
	}
}

func TestEventType_ValidateEnum(t *testing.T) {
	ctx := NewNotificationContext("Event")
	if !ValidateEnum(EventLog, "type", ctx) {
		t.Error("EventLog should validate")
	}
	if ValidateEnum(EventUnknown, "type", ctx) {
		t.Error("EventUnknown (the sentinel) should not validate")
	}
	// Membership is enforced in the domain now: an out-of-range cast is not a
	// declared member, so it fails too (no longer a mere zero-value guard).
	if ValidateEnum(EventType(99), "type", ctx) {
		t.Error("EventType(99) is outside the declared set — should not validate")
	}
}

func TestEventType_String(t *testing.T) {
	cases := []struct {
		t    EventType
		want string
	}{
		{EventUnknown, "unknown"},
		{EventLog, "log"},
		{EventDebug, "debug"},
		{EventError, "error"},
		{EventWarning, "warning"},
		{EventType(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestDomainEvent_Accessors(t *testing.T) {
	cause := errors.New("downstream")
	e := DomainEvent{
		Type:   EventLog,
		Class:  "User",
		Msg:    "saved",
		Vals:   map[string]string{"k": "v"},
		Reason: cause,
	}
	if e.EventType() != EventLog {
		t.Errorf("EventType() = %v", e.EventType())
	}
	if e.ClassName() != "User" {
		t.Errorf("ClassName() = %q", e.ClassName())
	}
	if e.Message() != "saved" {
		t.Errorf("Message() = %q", e.Message())
	}
	if got, ok := e.Values().(map[string]string); !ok || got["k"] != "v" {
		t.Errorf("Values() = %#v", e.Values())
	}
	if !errors.Is(e.Err(), cause) {
		t.Errorf("Err() = %v", e.Err())
	}

	// Compile-time check that DomainEvent satisfies Event.
	var _ Event = DomainEvent{}
}

// --- DomainError edge cases ------------------------------------------------

func TestDomainError_ErrorEmpty(t *testing.T) {
	if got := (&DomainError{}).Error(); got != "domain validation error" {
		t.Errorf("Error() with empty contexts = %q", got)
	}
}

func TestDomainError_ErrorWithContexts(t *testing.T) {
	e := NewDomainError([]*NotificationContext{NewNotificationContext("A"), NewNotificationContext("B")})
	if got := e.Error(); !strings.Contains(got, "2 context(s)") {
		t.Errorf("Error() should mention context count, got %q", got)
	}
}

func TestDomainError_HasErrors_True(t *testing.T) {
	ctx := NewNotificationContext("A")
	ctx.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
	e := NewDomainError([]*NotificationContext{ctx})
	if !e.HasErrors() {
		t.Error("HasErrors should be true when context carries a message")
	}
}

func TestDomainError_HasErrors_False(t *testing.T) {
	ctx := NewNotificationContext("A") // empty
	e := NewDomainError([]*NotificationContext{ctx})
	if e.HasErrors() {
		t.Error("HasErrors should be false when no context carries messages")
	}
	if (&DomainError{}).HasErrors() {
		t.Error("HasErrors should be false for empty contexts")
	}
}

func TestDomainError_NotificationContexts_Identity(t *testing.T) {
	a := NewNotificationContext("A")
	b := NewNotificationContext("B")
	e := NewDomainError([]*NotificationContext{a, b})
	got := e.NotificationContexts()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("NotificationContexts() should return the same slice (same pointers)")
	}
}

// --- Notification base classes ---------------------------------------------

func TestNotificationBase_DefaultSemanticIsValidation(t *testing.T) {
	if got := (NotificationBase{}).Semantic(); got != SemanticValidation {
		t.Errorf("NotificationBase.Semantic() = %v, want SemanticValidation", got)
	}
	if got := (DomainNotificationBase{}).Semantic(); got != SemanticValidation {
		t.Errorf("DomainNotificationBase.Semantic() = %v, want SemanticValidation", got)
	}
	if got := (ApplicationNotificationBase{}).Semantic(); got != SemanticValidation {
		t.Errorf("ApplicationNotificationBase.Semantic() = %v, want SemanticValidation", got)
	}
	if got := (InfrastructureNotificationBase{}).Semantic(); got != SemanticValidation {
		t.Errorf("InfrastructureNotificationBase.Semantic() = %v, want SemanticValidation", got)
	}
}

func TestNotificationBase_IsNotificationCallable(t *testing.T) {
	// Private marker — only callable from within the package. Exercising it
	// counts the empty body for the coverage profile.
	NotificationBase{}.isNotification()
}

// --- NotificationContext Copy / Scoped / ChangeFieldName / Clear ----------

func TestNotificationContext_HasErrors(t *testing.T) {
	c := NewNotificationContext("A")
	if c.HasErrors() {
		t.Error("fresh context should have no errors")
	}
	c.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
	if !c.HasErrors() {
		t.Error("after adding a message HasErrors should be true")
	}
}

func TestNotificationContext_Clear(t *testing.T) {
	c := NewNotificationContext("A")
	c.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
	c.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
	c.Clear()
	if c.HasErrors() {
		t.Error("Clear should drop all messages")
	}
	// Adding after Clear works.
	c.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
	if !c.HasErrors() {
		t.Error("messages can be added after Clear")
	}
}

func TestNotificationContext_ChangeFieldName(t *testing.T) {
	c := NewNotificationContext("A")
	c.AddNotificationMessage(NotificationMessage{FieldName: "email", Notification: RequiredFieldNotification{}})
	c.AddNotificationMessage(NotificationMessage{FieldName: "phone", Notification: RequiredFieldNotification{}})
	c.ChangeFieldName("email", "primaryEmail")

	msgs := c.Messages()
	if msgs[0].ResolveFieldName() != "primaryEmail" {
		t.Errorf("first msg field = %q, want %q", msgs[0].ResolveFieldName(), "primaryEmail")
	}
	if msgs[1].ResolveFieldName() != "phone" {
		t.Errorf("second msg field = %q, want untouched", msgs[1].ResolveFieldName())
	}
}

func TestNotificationContext_Scoped_NoSegmentsReturnsSame(t *testing.T) {
	c := NewNotificationContext("A")
	got := c.Scoped()
	if got != c {
		t.Error("Scoped() with no segments should return the receiver")
	}
}

func TestNotificationContext_Scoped_AddsToRoot(t *testing.T) {
	root := NewNotificationContext("A")
	scoped := root.Scoped(NameSegment("items"), IndexSegment(0))
	scoped.AddNotification("field", RequiredFieldNotification{})

	if !root.HasErrors() {
		t.Error("messages should land in the root context")
	}
	if got := root.Messages()[0].ResolveFieldName(); got != "items[0].field" {
		t.Errorf("ResolveFieldName = %q, want items[0].field", got)
	}
}

func TestNotificationContext_Scoped_ChainedNesting(t *testing.T) {
	root := NewNotificationContext("A")
	level1 := root.Scoped(NameSegment("orders"), IndexSegment(2))
	level2 := level1.Scoped(NameSegment("lines"), IndexSegment(5))
	level2.AddNotification("quantity", RequiredFieldNotification{})

	got := root.Messages()[0].ResolveFieldName()
	if got != "orders[2].lines[5].quantity" {
		t.Errorf("nested field = %q, want orders[2].lines[5].quantity", got)
	}
}

func TestNotificationContext_Copy_DefaultName(t *testing.T) {
	c := NewNotificationContext("Original")
	c.AddNotificationMessage(NotificationMessage{FieldName: "x", Notification: RequiredFieldNotification{}})
	cp := c.Copy()
	if cp.Context() != "Original" {
		t.Errorf("Copy() default context = %q, want Original", cp.Context())
	}
	if len(cp.Messages()) != 1 {
		t.Errorf("Copy() should preserve messages, got %d", len(cp.Messages()))
	}

	// Mutating the copy must not affect the original.
	cp.AddNotificationMessage(NotificationMessage{FieldName: "y", Notification: RequiredFieldNotification{}})
	if len(c.Messages()) != 1 {
		t.Errorf("source mutated after copy modification: %d messages", len(c.Messages()))
	}
}

func TestNotificationContext_Copy_RenameContext(t *testing.T) {
	c := NewNotificationContext("Original")
	cp := c.Copy("Renamed")
	if cp.Context() != "Renamed" {
		t.Errorf("Copy(\"Renamed\").Context() = %q", cp.Context())
	}

	// Passing an empty string should keep the original name.
	cp2 := c.Copy("")
	if cp2.Context() != "Original" {
		t.Errorf("Copy(\"\") should preserve original name, got %q", cp2.Context())
	}
}

// --- DomainResult helpers ---------------------------------------------------

func TestDomainResult_DefaultIsEmpty(t *testing.T) {
	r := NewDomainResult()
	if r.HasErrors() {
		t.Error("fresh DomainResult should have no errors")
	}
	if r.Entity != nil {
		t.Error("fresh DomainResult.Entity should be nil")
	}
}

func TestDomainResult_AddContext_FiltersEmpty(t *testing.T) {
	r := NewDomainResult()
	r.AddContext(nil)
	r.AddContext(NewNotificationContext("Empty")) // no messages → filtered

	good := NewNotificationContext("X")
	good.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
	r.AddContext(good)

	if !r.HasErrors() {
		t.Error("expected HasErrors=true after adding context with messages")
	}
	if len(r.Contexts) != 1 {
		t.Errorf("expected only 1 context (the populated one), got %d", len(r.Contexts))
	}
}

func TestDomainResult_WithEntity_Chainable(t *testing.T) {
	r := NewDomainResult()
	source := &plainEntity{}
	id := NewRandomID()
	ins := newBuilder("plain", "GetInsertable", newMetadata().signature, nil).insertable(source, &id)

	got := r.WithEntity(ins)
	if got != r {
		t.Error("WithEntity should return the same DomainResult")
	}
	storedIns, ok := r.Entity.(Insertable)
	if !ok {
		t.Fatalf("Entity should be an Insertable, got %T", r.Entity)
	}
	if storedIns.Source() != source {
		t.Error("WithEntity did not preserve the entity's Source")
	}
}
