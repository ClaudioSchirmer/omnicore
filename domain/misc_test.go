package domain

import (
	"errors"
	"strings"
	"testing"
)

// --- Rules DSL --------------------------------------------------------------

func TestRules_Mode_Context(t *testing.T) {
	ctx := NewNotificationContext("X")
	r := NewRules(ModeInsert, ctx, nil)
	if r.Mode() != ModeInsert {
		t.Errorf("Mode() = %v", r.Mode())
	}
	if r.Context() != ctx {
		t.Error("Context() should return the wired ctx")
	}
}

func TestRules_AddNotification_NilCtxNoOp(t *testing.T) {
	r := NewRules(ModeInsert, nil, nil)
	r.AddNotification("x", RequiredFieldNotification{}) // must not panic
	r.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
}

func TestRules_AddNotification_Forwards(t *testing.T) {
	ctx := NewNotificationContext("Root")
	r := NewRules(ModeInsert, ctx, nil)
	r.AddNotification("name", RequiredFieldNotification{})
	r.AddNotification("email", RequiredFieldNotification{}, "x@y")

	msgs := ctx.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[1].FieldValue != "x@y" {
		t.Errorf("FieldValue = %q, want x@y", msgs[1].FieldValue)
	}
}

func TestRules_AddNotificationMessage_Forwards(t *testing.T) {
	ctx := NewNotificationContext("Root")
	r := NewRules(ModeInsert, ctx, nil)
	r.AddNotificationMessage(NotificationMessage{
		FieldName:    "service",
		Err:          errors.New("boom"),
		Notification: ServiceIsRequiredNotification{},
	})
	if len(ctx.Messages()) != 1 {
		t.Errorf("expected 1 message, got %d", len(ctx.Messages()))
	}
}

func TestRules_IfInsert_FiresOnlyOnInsert(t *testing.T) {
	cases := []struct {
		mode      EntityMode
		shouldRun bool
	}{
		{ModeInsert, true},
		{ModeUpdate, false},
		{ModeDelete, false},
		{ModeDisplay, false},
	}
	for _, tc := range cases {
		ran := false
		ret := NewRules(tc.mode, nil, nil).IfInsert(func() { ran = true })
		if ret == nil {
			t.Error("IfInsert should return *Rules")
		}
		if ran != tc.shouldRun {
			t.Errorf("mode=%v IfInsert ran=%v, want %v", tc.mode, ran, tc.shouldRun)
		}
	}
}

// allModes is the full EntityMode spectrum every IfXxx dispatch test sweeps,
// so each closure is proven to fire on ITS mode and stay silent on every other
// — in particular that IfUpdate does NOT fire on Archive/Unarchive (the whole
// point of giving those verbs their own clauses).
var allModes = []EntityMode{ModeDisplay, ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}

func TestRules_IfUpdate_FiresOnlyOnUpdate(t *testing.T) {
	for _, m := range allModes {
		ran := false
		NewRules(m, nil, nil).IfUpdate(func() { ran = true })
		if ran != (m == ModeUpdate) {
			t.Errorf("mode=%v IfUpdate ran=%v (must fire ONLY on ModeUpdate)", m, ran)
		}
	}
}

func TestRules_IfDelete_FiresOnlyOnDelete(t *testing.T) {
	for _, m := range allModes {
		ran := false
		NewRules(m, nil, nil).IfDelete(func() { ran = true })
		if ran != (m == ModeDelete) {
			t.Errorf("mode=%v IfDelete ran=%v", m, ran)
		}
	}
}

func TestRules_IfArchive_FiresOnlyOnArchive(t *testing.T) {
	for _, m := range allModes {
		ran := false
		NewRules(m, nil, nil).IfArchive(func() { ran = true })
		if ran != (m == ModeArchive) {
			t.Errorf("mode=%v IfArchive ran=%v (must fire ONLY on ModeArchive)", m, ran)
		}
	}
}

func TestRules_IfUnarchive_FiresOnlyOnUnarchive(t *testing.T) {
	for _, m := range allModes {
		ran := false
		NewRules(m, nil, nil).IfUnarchive(func() { ran = true })
		if ran != (m == ModeUnarchive) {
			t.Errorf("mode=%v IfUnarchive ran=%v (must fire ONLY on ModeUnarchive)", m, ran)
		}
	}
}

func TestRules_IfInsertOrUpdate(t *testing.T) {
	for _, m := range []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeDisplay} {
		ran := false
		NewRules(m, nil, nil).IfInsertOrUpdate(func() { ran = true })
		want := m == ModeInsert || m == ModeUpdate
		if ran != want {
			t.Errorf("mode=%v IfInsertOrUpdate ran=%v, want %v", m, ran, want)
		}
	}
}

func TestRules_IfDisplay(t *testing.T) {
	for _, m := range []EntityMode{ModeInsert, ModeDisplay} {
		ran := false
		NewRules(m, nil, nil).IfDisplay(func() { ran = true })
		if ran != (m == ModeDisplay) {
			t.Errorf("mode=%v IfDisplay ran=%v", m, ran)
		}
	}
}

// --- DefaultRepository -----------------------------------------------------

func TestDefaultRepository_FindByID_NotImplemented(t *testing.T) {
	repo := DefaultRepository[*plainEntity]{Name: "TestRepo"}

	if v, err := repo.FindByID(NewRandomID()); err == nil || v != nil {
		t.Error("FindByID default should fail and return zero")
	}
	if v := repo.New(); v != nil {
		t.Errorf("New default should return zero (nil), got %v", v)
	}
}

func TestDefaultRepository_ErrorCarriesNotImplementedNotification(t *testing.T) {
	repo := DefaultRepository[*plainEntity]{Name: "TestRepo"}
	_, err := repo.FindByID(NewRandomID())
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	var carrier NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected error to implement NotificationCarrier, got %T", err)
	}
	msgs := carrier.NotificationContexts()[0].Messages()
	if got := NotificationKey(msgs[0].Notification); got != "RepositoryFunctionNotImplementedNotification" {
		t.Errorf("notification key = %q", got)
	}
	if !strings.Contains(msgs[0].FuncName, "TestRepo.FindByID") {
		t.Errorf("FuncName = %q, want to contain 'TestRepo.FindByID'", msgs[0].FuncName)
	}
}

// --- BaseEntity helpers ----------------------------------------------------

func TestBaseEntity_ClearID(t *testing.T) {
	e := &plainEntity{}
	id := NewRandomID()
	e.SetID(id)
	if got := e.GetID(); got == nil || *got != id {
		t.Errorf("SetID did not stick: %v", got)
	}
	e.ClearID()
	if e.GetID() != nil {
		t.Errorf("ClearID failed, GetID() = %v", e.GetID())
	}
}

func TestBaseEntity_AddNotificationContextAccumulates(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	extra := NewNotificationContext("Extra")
	extra.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
	e.AddNotificationContext(extra)

	all := collectContexts(e)
	// extra has errors, root has none → only extra is collected.
	if len(all) != 1 {
		t.Errorf("expected 1 context with errors, got %d", len(all))
	}
}

// fakeVO satisfies ValueObjectValidator without IO.
type fakeVO struct {
	called *bool
}

func (v fakeVO) IsValid(_ string, _ *NotificationContext) bool {
	if v.called != nil {
		*v.called = true
	}
	return true
}

func TestRules_ValidateValueObject_ForcedRunsDuringValidation(t *testing.T) {
	called := false
	ctx := NewNotificationContext("X")
	r := NewRules(ModeInsert, ctx, nil)
	r.ValidateValueObject("MyVO", fakeVO{called: &called})

	// A forced VO (not a plain field) runs even on a value with no VO fields.
	validateValueObjectFields(struct{}{}, ctx, r.ignoredValueObjects(), r.forcedValueObjects())
	if !called {
		t.Error("forced value object should run via validateValueObjectFields")
	}
}

func TestBaseEntity_ValidateAggregateValueObjects_FansOut(t *testing.T) {
	calls := 0
	e := &plainEntity{}
	ensureInit(e)
	e.ValidateAggregateValueObjects("Tag", []AggregateValueObject{
		Tag{Name: "a", callCount: &calls},
		Tag{Name: "b", callCount: &calls},
		Tag{Name: "c", callCount: &calls},
	})

	runAggregateValidations(e, ModeInsert, "test")
	if calls != 3 {
		t.Errorf("expected 3 AVO BuildRules calls, got %d", calls)
	}
}

func TestBaseEntity_RegisterEvent(t *testing.T) {
	e := &plainEntity{}
	e.RegisterEvent(DomainEvent{Type: EventLog, Msg: "a"})
	e.RegisterEvent(DomainEvent{Type: EventLog, Msg: "b"})
	if got := e.Events(); len(got) != 2 || got[0].Message() != "a" {
		t.Errorf("Events() = %+v", got)
	}
}

func TestBaseEntity_AddFieldNameAlias(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	e.AddFieldNameAlias("street", "addressLine")
	e.AddFieldNameAlias("zip", "postalCode")

	// Emit two notifications keyed on alias-source names, then run alias pass.
	e.NotificationContext().AddNotificationMessage(NotificationMessage{
		FieldName: "street", Notification: RequiredFieldNotification{},
	})
	e.NotificationContext().AddNotificationMessage(NotificationMessage{
		FieldName: "zip", Notification: RequiredFieldNotification{},
	})

	applyFieldAliases(e)

	msgs := e.NotificationContext().Messages()
	if msgs[0].ResolveFieldName() != "addressLine" {
		t.Errorf("first message field = %q, want addressLine", msgs[0].ResolveFieldName())
	}
	if msgs[1].ResolveFieldName() != "postalCode" {
		t.Errorf("second message field = %q, want postalCode", msgs[1].ResolveFieldName())
	}
}

func TestApplyFieldAliases_NoOpWithoutAliases(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	e.NotificationContext().AddNotificationMessage(NotificationMessage{FieldName: "x", Notification: RequiredFieldNotification{}})

	// No aliases registered — should be a silent no-op.
	applyFieldAliases(e)
	if e.NotificationContext().Messages()[0].ResolveFieldName() != "x" {
		t.Errorf("expected field name to stay 'x' without aliases")
	}
}

func TestApplyFieldAliases_NoCtx(t *testing.T) {
	e := &plainEntity{}
	// notifCtx is nil — must not panic.
	applyFieldAliases(e)
}

func TestBaseEntity_GetMode_DefaultIsDisplayAfterInit(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	if got := e.getMode(); got != ModeDisplay {
		t.Errorf("default mode after init = %v, want ModeDisplay", got)
	}
}

func TestEnsureInitialized_PublicWrapper(t *testing.T) {
	e := &plainEntity{}
	EnsureInitialized(e)
	if e.NotificationContext() == nil {
		t.Error("EnsureInitialized should populate the NotificationContext")
	}
	// Idempotent — a second call should not panic and should not reset.
	ctx := e.NotificationContext()
	EnsureInitialized(e)
	if e.NotificationContext() != ctx {
		t.Error("EnsureInitialized should be idempotent (same ctx after second call)")
	}
}

// --- IsValid -------------------------------------------------------------

func TestIsValid_InsertOK(t *testing.T) {
	e := newArchivableTestEntity(ModeInsert)
	e.ClearID() // no ID needed for Insert
	ok, ctxs := IsValid(e, ModeInsert, nil)
	if !ok {
		t.Errorf("IsValid Insert ok=false unexpectedly, contexts=%+v", ctxs)
	}
}

func TestIsValid_UpdateRequiresID(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	ok, _ := IsValid(e, ModeUpdate, nil)
	if ok {
		t.Error("IsValid Update without ID should be false")
	}
}

func TestIsValid_DeleteRequiresID(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	ok, _ := IsValid(e, ModeDelete, nil)
	if ok {
		t.Error("IsValid Delete without ID should be false")
	}
}

// --- EnumDescriptionKey ----------------------------------------------------

func TestEnumDescriptionKey_ValueAndPointer(t *testing.T) {
	if got := EnumDescriptionKey(ModeInsert); got != "EntityMode.INSERT" {
		t.Errorf("EnumDescriptionKey = %q", got)
	}
	m := ModeUpdate
	if got := EnumDescriptionKey(&m); got != "EntityMode.UPDATE" {
		t.Errorf("EnumDescriptionKey(ptr) = %q", got)
	}
}

// --- identifier_render: isVowelByte ---------------------------------------

func TestIsVowelByte(t *testing.T) {
	for _, b := range []byte{'a', 'e', 'i', 'o', 'u'} {
		if !isVowelByte(b) {
			t.Errorf("isVowelByte(%c) = false, want true", b)
		}
	}
	for _, b := range []byte{'b', 'c', 'd', 'z', '0', '!'} {
		if isVowelByte(b) {
			t.Errorf("isVowelByte(%c) = true, want false", b)
		}
	}
}

// --- WorkUnit -------------------------------------------------------------

func TestRunWorkUnit_ReturnsValue(t *testing.T) {
	v, err := RunWorkUnit(WorkUnit[int](func() (int, error) { return 42, nil }))
	if err != nil || v != 42 {
		t.Errorf("RunWorkUnit = (%d,%v)", v, err)
	}
}

func TestRunWorkUnit_PropagatesError(t *testing.T) {
	want := errors.New("boom")
	_, err := RunWorkUnit(WorkUnit[int](func() (int, error) { return 0, want }))
	if !errors.Is(err, want) {
		t.Errorf("RunWorkUnit error = %v, want %v", err, want)
	}
}

// --- ServiceBase --------------------------------------------------------------

func TestServiceBase_SatisfiesService(t *testing.T) {
	var _ Service = ServiceBase{}
	ServiceBase{}.isService()
}

// --- AddNotificationMessage on BaseEntity ---------------------------------

func TestBaseEntity_AddNotificationMessage_RoutesToCtx(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	e.AddNotificationMessage(NotificationMessage{FieldName: "f", Notification: RequiredFieldNotification{}})
	if !e.NotificationContext().HasErrors() {
		t.Error("AddNotificationMessage should route to ctx")
	}
}

func TestBaseEntity_AddNotificationMessage_NoCtxSilent(t *testing.T) {
	e := &plainEntity{}
	// notifCtx nil — must not panic.
	e.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
}

func TestBaseEntity_AddNotification_NoCtxSilent(t *testing.T) {
	e := &plainEntity{}
	e.AddNotification("f", RequiredFieldNotification{}) // notifCtx nil — silent
}

// --- DomainNotificationBase overrides (kernel) ----------------------------

func TestKernelNotifications_Semantic(t *testing.T) {
	cases := []struct {
		n    Notification
		want NotificationSemantic
	}{
		{RecordNotFoundNotification{}, SemanticNotFound},
		{EntityIsNotActiveNotification{}, SemanticStateConflict},
		{EntityAlreadyAddedNotification{}, SemanticConflict},
		{InsertNotAllowedNotification{}, SemanticForbidden},
		{UpdateNotAllowedNotification{}, SemanticForbidden},
		{DeleteNotAllowedNotification{}, SemanticForbidden},
		{ArchiveNotAllowedNotification{}, SemanticForbidden},
		{UnarchiveNotAllowedNotification{}, SemanticForbidden},
	}
	for _, tc := range cases {
		if got := tc.n.Semantic(); got != tc.want {
			t.Errorf("%T.Semantic() = %v, want %v", tc.n, got, tc.want)
		}
	}
}
