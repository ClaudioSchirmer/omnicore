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
	r.AddNotificationNamed("x", RequiredFieldNotification{}) // must not panic
	r.AddNotificationMessage(NotificationMessage{Notification: RequiredFieldNotification{}})
}

func TestRules_AddNotification_Forwards(t *testing.T) {
	ctx := NewNotificationContext("Root")
	r := NewRules(ModeInsert, ctx, nil)
	r.AddNotificationNamed("name", RequiredFieldNotification{})
	r.AddNotificationNamed("email", RequiredFieldNotification{}, "x@y")

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
		Override:     "service",
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

	runAggregateValidations(e, ModeInsert, "test", nil)
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
	e.AddFieldNameAlias("Street", "addressLine")
	e.AddFieldNameAlias("Zip", "postalCode")

	// Emit two notifications on the alias-source GO field names (the alias
	// keys on the Go name and rewrites the leaf segment's wire token).
	e.NotificationContext().AddNotificationMessage(NotificationMessage{
		Path: []PathSegment{{Name: "Street"}}, Notification: RequiredFieldNotification{},
	})
	e.NotificationContext().AddNotificationMessage(NotificationMessage{
		Path: []PathSegment{{Name: "Zip"}}, Notification: RequiredFieldNotification{},
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
	e.NotificationContext().AddNotificationMessage(NotificationMessage{Override: "x", Notification: RequiredFieldNotification{}})

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

func TestEnsureInit_Idempotent(t *testing.T) {
	e := &plainEntity{}
	ensureInit(e)
	ctx := e.NotificationContext()
	if ctx == nil {
		t.Fatal("ensureInit should populate the NotificationContext")
	}
	ensureInit(e)
	if e.NotificationContext() != ctx {
		t.Error("ensureInit should be idempotent (same ctx after second call)")
	}
}

// --- automatic context (no initialization step for the developer) ---------

// lateLabelEntity carries a `labelKey` tag so the late stamping can be observed
// on a message emitted before the framework ever saw the entity.
type lateLabelEntity struct {
	BaseEntity
	Name string `labelKey:"LateLabelNameField"`
}

func (l *lateLabelEntity) Modes() []EntityMode                      { return []EntityMode{ModeInsert} }
func (l *lateLabelEntity) BuildRules(_ string, _ Service, _ *Rules) {}

func TestBaseEntity_NotificationContext_NeverNil(t *testing.T) {
	e := &plainEntity{}
	if e.NotificationContext() == nil {
		t.Fatal("a freshly constructed entity must already carry a context")
	}
}

func TestBaseEntity_AddNotification_BeforeFrameworkEntry(t *testing.T) {
	e := &plainEntity{}
	e.AddNotificationNamed("Name", RequiredFieldNotification{})

	if !e.NotificationContext().HasErrors() {
		t.Fatal("a notification emitted before any framework call must be kept, not dropped")
	}
	// The framework's own entry point then names the context in place; the
	// notification emitted earlier survives it.
	ensureInit(e)
	if got := e.NotificationContext().Context(); got != "plainEntity" {
		t.Errorf("context name after ensureInit = %q, want %q", got, "plainEntity")
	}
	if n := len(e.NotificationContext().Messages()); n != 1 {
		t.Fatalf("messages after ensureInit = %d, want 1", n)
	}
}

func TestBaseEntity_AddNotificationMessage_BeforeFrameworkEntry(t *testing.T) {
	e := &plainEntity{}
	e.AddNotificationMessage(NotificationMessage{
		Override:     "name",
		Notification: RequiredFieldNotification{},
	})
	if n := len(e.NotificationContext().Messages()); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
}

func TestInitWithName_BackfillsPendingLabels(t *testing.T) {
	e := &lateLabelEntity{}
	// Emitted while the context is still anonymous: the label cannot be
	// resolved yet, because *BaseEntity does not know it is inside a
	// labeledEntity.
	e.AddNotificationNamed("Name", RequiredFieldNotification{})
	if got := e.NotificationContext().Messages()[0].LabelKey; got != "" {
		t.Fatalf("LabelKey before naming = %q, want empty", got)
	}

	ensureInit(e)

	if got := e.NotificationContext().Messages()[0].LabelKey; got != "LateLabelNameField" {
		t.Errorf("LabelKey after naming = %q, want %q", got, "LateLabelNameField")
	}
}

func TestInitWithName_BackfillLeavesResolvedLabelsAlone(t *testing.T) {
	e := &lateLabelEntity{}
	ensureInit(e)
	// Emitted after naming: resolved at emit time, and a second stamping
	// attempt must not disturb it.
	e.AddNotificationNamed("Name", RequiredFieldNotification{})
	ensureInit(e)
	if got := e.NotificationContext().Messages()[0].LabelKey; got != "LateLabelNameField" {
		t.Errorf("LabelKey = %q, want %q", got, "LateLabelNameField")
	}
}

func TestResolvePendingLabels_SkipsUnnamedType(t *testing.T) {
	ctx := NewNotificationContext("")
	ctx.AddNotificationNamed("Name", RequiredFieldNotification{})
	ctx.resolvePendingLabels() // entityType is nil — nothing to resolve, no panic
	if got := ctx.Messages()[0].LabelKey; got != "" {
		t.Errorf("LabelKey = %q, want empty", got)
	}
}

func TestResolvePendingLabels_SkipsScopedForwardedMessages(t *testing.T) {
	e := &lateLabelEntity{}
	ensureInit(e)
	// A child AVO's scoped view resolves its own label and forwards a
	// multi-segment path; the root's backfill must not touch it.
	scoped := e.NotificationContext().Scoped(NameSegment("Items"), IndexSegment(0))
	scoped.AddNotificationNamed("Name", RequiredFieldNotification{})

	e.NotificationContext().resolvePendingLabels()

	msg := e.NotificationContext().Messages()[0]
	if msg.LabelKey != "" {
		t.Errorf("forwarded message LabelKey = %q, want empty (child owns its label)", msg.LabelKey)
	}
	if got := msg.ResolveFieldName(); got != "items[0].name" {
		t.Errorf("forwarded field name = %q, want %q", got, "items[0].name")
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
	e.AddNotificationMessage(NotificationMessage{Override: "f", Notification: RequiredFieldNotification{}})
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
	e.AddNotificationNamed("f", RequiredFieldNotification{}) // notifCtx nil — silent
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
