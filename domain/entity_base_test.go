package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Rec is an AggregateValueObject used in tests. Short type name → collection
// inference yields "recs" (Pluralize(PascalToSnake("Rec"))) matching the
// previous wire-format expectations from Phase 13/14.
type Rec struct {
	ID        string
	Name      string
	callCount *int
	lastMode  *EntityMode
	emit      string
}

func (r Rec) GetID() string { return r.ID }
func (r Rec) BuildRules(_ string, _ Service, rules *Rules) {
	if r.callCount != nil {
		*r.callCount++
	}
	if r.lastMode != nil {
		*r.lastMode = rules.Mode()
	}
	if r.emit != "" {
		rules.AddNotification(r.emit, RequiredFieldNotification{})
	}
}

// Tag is registered via the manual AddAggregateValueObject path (typeName
// NOT discovered via reflection on AggregateRoot.items). Allows tests of the
// manual coexistence with the auto path.
type Tag struct {
	ID        string
	Name      string
	callCount *int
}

func (u Tag) GetID() string { return u.ID }
func (u Tag) BuildRules(_ string, _ Service, _ *Rules) {
	if u.callCount != nil {
		*u.callCount++
	}
}

// providerEntity embeds AggregateRoot and implements AggregateRootProvider.
// Phase 19: no children mapping — typeNames discovered via reflection on
// AggregateRoot.AllAggregateItems(); collection names inferred via
// PluralizeSnake(PascalToSnake(typeName)).
type providerEntity struct {
	AggregateRoot
}

func (p *providerEntity) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete}
}
func (p *providerEntity) BuildRules(_ string, _ Service, _ *Rules) {}
func (p *providerEntity) GetAggregateRoot() *AggregateRoot         { return &p.AggregateRoot }
func (p *providerEntity) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{Rec{}}
}

// plainEntity embeds BaseEntity and does NOT implement AggregateRootProvider.
// Used to assert the legacy manual-only path is preserved.
type plainEntity struct {
	BaseEntity
}

func (p *plainEntity) Modes() []EntityMode                      { return []EntityMode{ModeInsert} }
func (p *plainEntity) BuildRules(_ string, _ Service, _ *Rules) {}

// newProviderEntity returns a providerEntity initialized via ensureInit.
func newProviderEntity() *providerEntity {
	p := &providerEntity{}
	ensureInit(p)
	return p
}

func TestRunAggregateValidations_AutoIteratesItems(t *testing.T) {
	calls := 0
	p := newProviderEntity()
	p.AggregateConstructor([]AggregateValueObject{
		Rec{ID: "1", Name: "a", callCount: &calls},
	})
	AddAggregateChild(p, Rec{Name: "b", callCount: &calls})

	runAggregateValidations(p, ModeInsert, "test")

	if calls != 2 {
		t.Fatalf("expected 2 auto IsValid calls, got %d", calls)
	}
}

func TestRunAggregateValidations_SkipsRemovedItems(t *testing.T) {
	calls := 0
	p := newProviderEntity()
	target := Rec{ID: "1", Name: "a", callCount: &calls}
	p.AggregateConstructor([]AggregateValueObject{target})
	RemoveAggregateChild(p, target)

	runAggregateValidations(p, ModeUpdate, "test")

	if calls != 0 {
		t.Fatalf("expected 0 calls (removed item must be skipped), got %d", calls)
	}
}

func TestRunAggregateValidations_AddedAndChangedAreValidated(t *testing.T) {
	calls := 0
	p := newProviderEntity()
	original := Rec{ID: "1", Name: "a", callCount: &calls}
	p.AggregateConstructor([]AggregateValueObject{original})
	ChangeAggregateChild(p, original, Rec{ID: "1", Name: "a2", callCount: &calls})
	AddAggregateChild(p, Rec{Name: "b", callCount: &calls})

	runAggregateValidations(p, ModeUpdate, "test")

	if calls != 2 {
		t.Fatalf("expected 2 calls (changed + added), got %d", calls)
	}
}

func TestRunAggregateValidations_IgnoresManualWhenTypeNameInAggregate(t *testing.T) {
	autoCalls := 0
	manualCalls := 0
	p := newProviderEntity()
	AddAggregateChild(p, Rec{Name: "auto", callCount: &autoCalls})
	// Manual registration of the same typeName must be ignored: typeName "Rec"
	// already lives in AggregateRoot.items.
	p.AddAggregateValueObject("Rec", Rec{Name: "manual", callCount: &manualCalls})

	runAggregateValidations(p, ModeInsert, "test")

	if autoCalls != 1 {
		t.Fatalf("expected 1 auto call, got %d", autoCalls)
	}
	if manualCalls != 0 {
		t.Fatalf("expected manual entry to be ignored for typeName in aggregate, got %d calls", manualCalls)
	}
}

func TestRunAggregateValidations_KeepsManualForUnknownTypeName(t *testing.T) {
	autoCalls := 0
	manualCalls := 0
	p := newProviderEntity()
	AddAggregateChild(p, Rec{Name: "auto", callCount: &autoCalls})
	// "Tag" is NOT in the aggregate items — manual must still run.
	p.AddAggregateValueObject("Tag", Tag{Name: "tag1", callCount: &manualCalls})

	runAggregateValidations(p, ModeInsert, "test")

	if autoCalls != 1 {
		t.Fatalf("expected 1 auto call, got %d", autoCalls)
	}
	if manualCalls != 1 {
		t.Fatalf("expected manual call for unmapped typeName, got %d", manualCalls)
	}
}

func TestRunAggregateValidations_NoProviderUsesManualOnly(t *testing.T) {
	calls := 0
	e := &plainEntity{}
	ensureInit(e)
	e.AddAggregateValueObject("Tag", Tag{Name: "x", callCount: &calls})

	runAggregateValidations(e, ModeInsert, "test")

	if calls != 1 {
		t.Fatalf("expected manual path to run for non-provider entity, got %d calls", calls)
	}
}

func TestRunAggregateValidations_RunsInDeleteMode(t *testing.T) {
	calls := 0
	captured := ModeDisplay
	p := newProviderEntity()
	p.AggregateConstructor([]AggregateValueObject{
		Rec{ID: "1", Name: "a", callCount: &calls, lastMode: &captured},
	})

	runAggregateValidations(p, ModeDelete, "test")

	if calls != 1 {
		t.Fatalf("expected IsValid to run in ModeDelete (AVO decides), got %d calls", calls)
	}
	if captured != ModeDelete {
		t.Fatalf("expected mode to be ModeDelete, got %v", captured)
	}
}

func TestRunAggregateValidations_EmptyAggregateIsSafe(t *testing.T) {
	calls := 0
	p := newProviderEntity()
	p.AddAggregateValueObject("Tag", Tag{Name: "y", callCount: &calls})

	runAggregateValidations(p, ModeInsert, "test")

	// Auto path validates nothing (no items in aggregate). Manual runs.
	if calls != 1 {
		t.Fatalf("expected only manual entry to run (1 call), got %d", calls)
	}
}

// defaultsTestEntity intentionally OMITS RequiresService to assert the
// default provided by *BaseEntity is promoted via embed.
type defaultsTestEntity struct {
	BaseEntity
}

func (d *defaultsTestEntity) Modes() []EntityMode                { return []EntityMode{ModeDisplay} }
func (d *defaultsTestEntity) BuildRules(string, Service, *Rules) {}

// overrideRequiresServiceEntity declares RequiresService=true to assert that
// the outer type's method wins over the *BaseEntity default.
type overrideRequiresServiceEntity struct {
	BaseEntity
}

func (o *overrideRequiresServiceEntity) Modes() []EntityMode                { return []EntityMode{ModeDisplay} }
func (o *overrideRequiresServiceEntity) BuildRules(string, Service, *Rules) {}
func (o *overrideRequiresServiceEntity) RequiresService() bool              { return true }

func TestRequiresService_DefaultIsFalseViaPromotion(t *testing.T) {
	var e Entity = &defaultsTestEntity{}
	if e.RequiresService() {
		t.Fatalf("expected promoted default RequiresService to be false")
	}
}

func TestRequiresService_OverrideWinsOverDefault(t *testing.T) {
	var e Entity = &overrideRequiresServiceEntity{}
	if !e.RequiresService() {
		t.Fatalf("expected override RequiresService to be true")
	}
}

// archivableTestEntity has configurable Modes() so the same type covers
// both "allowed" and "denied" scenarios for Archive/Unarchive gating.
type archivableTestEntity struct {
	BaseEntity
	modes []EntityMode
}

func (a *archivableTestEntity) Modes() []EntityMode                { return a.modes }
func (a *archivableTestEntity) BuildRules(string, Service, *Rules) {}

func newArchivableTestEntity(modes ...EntityMode) *archivableTestEntity {
	e := &archivableTestEntity{modes: modes}
	e.SetID(NewID(uuid.NewString()))
	return e
}

// hasNotification scans every context of a NotificationCarrier for a message
// whose Notification matches the typeName provided.
func hasNotification(err error, typeName string) bool {
	var carrier NotificationCarrier
	if !errors.As(err, &carrier) {
		return false
	}
	for _, ctx := range carrier.NotificationContexts() {
		for _, msg := range ctx.Messages() {
			if NotificationKey(msg.Notification) == typeName {
				return true
			}
		}
	}
	return false
}

func TestGetArchivable_AllowedWhenModeDeclared(t *testing.T) {
	e := newArchivableTestEntity(ModeArchive)

	if _, err := GetArchivable(e, nil, "GetArchivable"); err != nil {
		t.Fatalf("expected GetArchivable to succeed when ModeArchive declared, got %v", err)
	}
}

func TestGetArchivable_DeniedWhenModeMissing(t *testing.T) {
	e := newArchivableTestEntity(ModeDisplay, ModeInsert)

	_, err := GetArchivable(e, nil, "GetArchivable")
	if err == nil {
		t.Fatal("expected GetArchivable to fail when ModeArchive missing")
	}
	if !hasNotification(err, "ArchiveNotAllowedNotification") {
		t.Fatalf("expected ArchiveNotAllowedNotification, got %v", err)
	}
}

func TestGetUnarchivable_AllowedWhenModeDeclared(t *testing.T) {
	e := newArchivableTestEntity(ModeUnarchive)

	if _, err := GetUnarchivable(e, nil, "GetUnarchivable"); err != nil {
		t.Fatalf("expected GetUnarchivable to succeed when ModeUnarchive declared, got %v", err)
	}
}

func TestGetUnarchivable_DeniedWhenModeMissing(t *testing.T) {
	e := newArchivableTestEntity(ModeDisplay, ModeInsert)

	_, err := GetUnarchivable(e, nil, "GetUnarchivable")
	if err == nil {
		t.Fatal("expected GetUnarchivable to fail when ModeUnarchive missing")
	}
	if !hasNotification(err, "UnarchiveNotAllowedNotification") {
		t.Fatalf("expected UnarchiveNotAllowedNotification, got %v", err)
	}
}

func TestGetArchivable_DoesNotInheritFromModeUpdate(t *testing.T) {
	e := newArchivableTestEntity(ModeDisplay, ModeInsert, ModeUpdate, ModeDelete)

	_, err := GetArchivable(e, nil, "GetArchivable")
	if err == nil {
		t.Fatal("expected GetArchivable to fail without ModeArchive even when ModeUpdate is present")
	}
	if !hasNotification(err, "ArchiveNotAllowedNotification") {
		t.Fatalf("expected ArchiveNotAllowedNotification, got %v", err)
	}
}

// Collection name is inferred via PluralizeSnake(PascalToSnake(typeName)).
// Rec → "recs". Wire path: "recs[0].street".
func TestRunAggregateValidations_AutoComposesCollectionPathWithIndex(t *testing.T) {
	p := newProviderEntity()
	AddAggregateChild(p, Rec{Name: "x", emit: "Street"})
	AddAggregateChild(p, Rec{Name: "y", emit: "ZipCode"})

	runAggregateValidations(p, ModeInsert, "test")

	msgs := p.NotificationContext().Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 emitted messages, got %d", len(msgs))
	}
	want := []string{"recs[0].street", "recs[1].zipCode"}
	for i, m := range msgs {
		if got := m.ResolveFieldName(); got != want[i] {
			t.Errorf("msg[%d]: expected field %q, got %q", i, want[i], got)
		}
	}
}

// ─── ActionName custom propagation ───────────────────────────────────────────

// actionRecorder captures the actionName each BuildRules invocation receives.
// Used by both the root (actionAwareRoot) and the AVO (actionAwareChild) so a
// single test can assert the custom value reaches both layers verbatim.
type actionRecorder struct {
	root     []string
	children []string
}

type actionAwareChild struct {
	ID  string
	rec *actionRecorder
}

func (a actionAwareChild) GetID() string { return a.ID }
func (a actionAwareChild) BuildRules(actionName string, _ Service, _ *Rules) {
	if a.rec != nil {
		a.rec.children = append(a.rec.children, actionName)
	}
}

type actionAwareRoot struct {
	AggregateRoot
	rec *actionRecorder
}

func (a *actionAwareRoot) Modes() []EntityMode { return []EntityMode{ModeInsert, ModeUpdate, ModeDelete} }
func (a *actionAwareRoot) BuildRules(actionName string, _ Service, _ *Rules) {
	if a.rec != nil {
		a.rec.root = append(a.rec.root, actionName)
	}
}
func (a *actionAwareRoot) GetAggregateRoot() *AggregateRoot { return &a.AggregateRoot }
func (a *actionAwareRoot) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{actionAwareChild{}}
}

// TestGetInsertable_PropagatesCustomActionNameToRootAndChildren proves that a
// custom actionName passed at the public entry point reaches the root's
// BuildRules AND every aggregate child's BuildRules (via
// runAggregateValidations) without rewriting — the exact string flows through.
// This is the contract that makes "two endpoints, same verb, different rigor"
// possible (e.g. POST /users lenient vs POST /admin/users strict).
func TestGetInsertable_PropagatesCustomActionNameToRootAndChildren(t *testing.T) {
	rec := &actionRecorder{}
	root := &actionAwareRoot{rec: rec}
	ensureInit(root)
	AddAggregateChild(root, actionAwareChild{ID: "1", rec: rec})
	AddAggregateChild(root, actionAwareChild{ID: "2", rec: rec})

	if _, err := GetInsertable(root, nil, "AdminCreate"); err != nil {
		t.Fatalf("GetInsertable failed: %v", err)
	}

	if got := rec.root; len(got) != 1 || got[0] != "AdminCreate" {
		t.Errorf("expected root to receive [\"AdminCreate\"], got %v", got)
	}
	if got := rec.children; len(got) != 2 || got[0] != "AdminCreate" || got[1] != "AdminCreate" {
		t.Errorf("expected both children to receive \"AdminCreate\", got %v", got)
	}
}

// TestGetUpdatable_PropagatesCustomActionName covers the update verb too — the
// closure-form Get* preserves the actionName through validateForUpdate.
func TestGetUpdatable_PropagatesCustomActionName(t *testing.T) {
	rec := &actionRecorder{}
	root := &actionAwareRoot{rec: rec}
	ensureInit(root)
	root.SetID(NewRandomID())
	AddAggregateChild(root, actionAwareChild{ID: "1", rec: rec})

	if _, err := GetUpdatable(root, func(*actionAwareRoot) {}, nil, "StrictUpdate"); err != nil {
		t.Fatalf("GetUpdatable failed: %v", err)
	}

	if got := rec.root; len(got) != 1 || got[0] != "StrictUpdate" {
		t.Errorf("expected root to receive [\"StrictUpdate\"], got %v", got)
	}
	if got := rec.children; len(got) != 1 || got[0] != "StrictUpdate" {
		t.Errorf("expected child to receive \"StrictUpdate\", got %v", got)
	}
}
