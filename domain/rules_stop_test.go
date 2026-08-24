package domain

import (
	"errors"
	"testing"
)

// guardEntity is a root whose BuildRules is laid out exactly like the shape the
// barrier is meant to protect: a rule that may reject, the barrier, and then
// work that must NOT run once it did — a bare statement AND a mode closure, so
// the test proves the barrier covers both, not just the DSL.
//
// It also carries an invalid value object (Color) and an aggregate child, so
// one pass can assert the three things the barrier stops: the rest of the body,
// the automatic value-object validation, and the children.
type guardEntity struct {
	AggregateRoot
	Color     testColor
	failGuard bool
	barrier   bool
	trace     *[]string
	modes     []EntityMode
}

func (e *guardEntity) Modes() []EntityMode {
	if e.modes != nil {
		return e.modes
	}
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}
func (e *guardEntity) GetAggregateRoot() *AggregateRoot { return &e.AggregateRoot }
func (e *guardEntity) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{Rec{}}
}

func (e *guardEntity) BuildRules(_ string, _ Service, r *Rules) {
	*e.trace = append(*e.trace, "guard")
	if e.failGuard {
		r.AddNotification("Guard", RequiredFieldNotification{})
	}
	if e.barrier {
		r.StopIfInvalid()
	}
	*e.trace = append(*e.trace, "bareStatement")
	r.IfInsertOrUpdate(func() {
		*e.trace = append(*e.trace, "closure")
	})
}

func newGuardEntity(failGuard, barrier bool, trace *[]string, childCalls *int) *guardEntity {
	e := &guardEntity{Color: colorUnknown, failGuard: failGuard, barrier: barrier, trace: trace}
	ensureInit(e)
	e.AggregateConstructor([]AggregateValueObject{Rec{Name: "a", callCount: childCalls}})
	return e
}

func keysOf(ctxs []*NotificationContext) map[string]bool {
	got := map[string]bool{}
	for _, c := range ctxs {
		for _, m := range c.Messages() {
			got[NotificationKey(m.Notification)] = true
		}
	}
	return got
}

// The barrier fires only on an already-rejected write. With a clean context it
// is inert and the pass runs whole — this is what makes it impossible for a
// barrier to turn an invalid write into a valid one.
func TestStopIfInvalid_CleanContextIsInert(t *testing.T) {
	trace := []string{}
	childCalls := 0
	e := newGuardEntity(false, true, &trace, &childCalls)

	_ = validateForInsert(e, "GetInsertable")

	want := []string{"guard", "bareStatement", "closure"}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	if childCalls != 1 {
		t.Errorf("child BuildRules calls = %d, want 1", childCalls)
	}
	if !keysOf(collectContexts(e))["testUnknownColorNotification"] {
		t.Error("value objects must still auto-validate when the barrier is inert")
	}
}

// The control case: same entity, same failing guard, no barrier. Everything
// below the guard runs. Read together with the next test, this is what isolates
// the barrier as the cause.
func TestStopIfInvalid_WithoutBarrierEverythingRuns(t *testing.T) {
	trace := []string{}
	childCalls := 0
	e := newGuardEntity(true, false, &trace, &childCalls)

	_ = validateForInsert(e, "GetInsertable")

	if len(trace) != 3 {
		t.Fatalf("trace = %v, want guard+bareStatement+closure", trace)
	}
	if childCalls != 1 {
		t.Errorf("child BuildRules calls = %d, want 1", childCalls)
	}
	if !keysOf(collectContexts(e))["testUnknownColorNotification"] {
		t.Error("value objects must validate when nothing stopped the pass")
	}
}

func TestStopIfInvalid_HaltsTheWholePass(t *testing.T) {
	trace := []string{}
	childCalls := 0
	e := newGuardEntity(true, true, &trace, &childCalls)

	err := validateForInsert(e, "GetInsertable")
	if err == nil {
		t.Fatal("expected the guard notification to reject the write")
	}

	if len(trace) != 1 || trace[0] != "guard" {
		t.Fatalf("trace = %v, want only [guard] — nothing below the barrier may run", trace)
	}
	if childCalls != 0 {
		t.Errorf("child BuildRules calls = %d, want 0 — the barrier stops the aggregate too", childCalls)
	}
	if keysOf(collectContexts(e))["testUnknownColorNotification"] {
		t.Error("automatic value-object validation ran past the barrier")
	}
}

// What the caller gets back: the rule that rejected, alone. This is the point
// of the feature — the response names the guard, not every field the entity
// would also have failed on.
func TestStopIfInvalid_ErrorCarriesOnlyTheGuardNotification(t *testing.T) {
	trace := []string{}
	childCalls := 0
	e := newGuardEntity(true, true, &trace, &childCalls)

	_, err := GetInsertable(e, nil, "GetInsertable")
	if err == nil {
		t.Fatal("expected a rejected write")
	}
	var carrier NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("error must be a NotificationCarrier, got %T", err)
	}
	msgs := []NotificationMessage{}
	for _, c := range carrier.NotificationContexts() {
		msgs = append(msgs, c.Messages()...)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly the guard notification, got %d: %+v", len(msgs), msgs)
	}
	if got := msgs[0].ResolveFieldName(); got != "guard" {
		t.Errorf("field = %q, want guard", got)
	}
}

// The structural gates are the frame of the write, not a rule about its values:
// they sit outside the barrier and always report, so a rejected write still
// says the verb was not allowed at all.
func TestStopIfInvalid_StructuralGatesStillReport(t *testing.T) {
	trace := []string{}
	childCalls := 0
	e := newGuardEntity(true, true, &trace, &childCalls)
	e.modes = []EntityMode{ModeUpdate} // Insert not declared

	_ = validateForInsert(e, "GetInsertable")

	if !keysOf(collectContexts(e))["InsertNotAllowedNotification"] {
		t.Error("Modes() gate must report even after a barrier halted the pass")
	}
}

func TestStopIfInvalid_EveryMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(Entity, string) error
	}{
		{"insert", validateForInsert},
		{"update", validateForUpdate},
		{"delete", validateForDelete},
		{"archive", validateForArchive},
		{"unarchive", validateForUnarchive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := []string{}
			childCalls := 0
			e := newGuardEntity(true, true, &trace, &childCalls)

			_ = tc.run(e, "test")

			if len(trace) != 1 {
				t.Fatalf("trace = %v, want only [guard]", trace)
			}
			if childCalls != 0 {
				t.Errorf("child BuildRules calls = %d, want 0", childCalls)
			}
		})
	}
}

// guardChild raises the barrier from a CHILD's BuildRules. Its own Color is
// invalid, so the test can tell whether the child's automatic value-object
// validation ran after it stopped.
type guardChild struct {
	Managed
	Name    string
	Color   testColor
	barrier bool
	calls   *int
}

func (c guardChild) CollectionName() string { return "GuardChilds" }
func (c guardChild) IsSameBusinessIdentity(o AggregateValueObject) bool {
	x, ok := o.(guardChild)
	return ok && c.Name == x.Name
}
func (c guardChild) BuildRules(_ string, _ Service, r *Rules) {
	if c.calls != nil {
		*c.calls++
	}
	if c.barrier {
		r.AddNotification("ChildGuard", RequiredFieldNotification{})
		r.StopIfInvalid()
	}
}

// From a child's seat the barrier stops what has not run yet: this child's own
// value objects, and every sibling still queued behind it.
func TestStopIfInvalid_FromChildCutsOwnVOsAndSiblings(t *testing.T) {
	calls := 0
	root := &providerEntity{}
	ensureInit(root)
	root.AggregateConstructor([]AggregateValueObject{
		guardChild{Name: "first", Color: colorUnknown, barrier: true, calls: &calls},
		guardChild{Name: "second", Color: colorUnknown, calls: &calls},
	})

	runAggregateValidations(root, ModeInsert, "test", &rulesPass{})

	if calls != 1 {
		t.Fatalf("child BuildRules calls = %d, want 1 — the sibling must not run", calls)
	}
	got := keysOf([]*NotificationContext{root.NotificationContext()})
	if !got["RequiredFieldNotification"] {
		t.Error("the child's own guard notification must survive")
	}
	if got["testUnknownColorNotification"] {
		t.Error("the stopping child's value objects must not validate past the barrier")
	}
}

// panicEntity blows up for a reason that has nothing to do with the barrier.
// The seam that absorbs the stop signal must let this through untouched.
type panicEntity struct {
	BaseEntity
}

func (p *panicEntity) Modes() []EntityMode { return []EntityMode{ModeInsert} }
func (p *panicEntity) BuildRules(_ string, _ Service, _ *Rules) {
	panic("a genuine bug in a rule")
}

func TestStopIfInvalid_GenuinePanicStillPropagates(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("a real panic must reach the caller, not be swallowed by the stop seam")
		}
		if rec != "a genuine bug in a rule" {
			t.Fatalf("panic value = %v, want it re-panicked intact", rec)
		}
	}()
	e := &panicEntity{}
	ensureInit(e)
	_ = validateForInsert(e, "GetInsertable")
}

// deferEntity proves the unwind is an ordinary Go unwind: a defer the rule
// itself registered still runs on the way out.
type deferEntity struct {
	BaseEntity
	ran *bool
}

func (e *deferEntity) Modes() []EntityMode { return []EntityMode{ModeInsert} }
func (e *deferEntity) BuildRules(_ string, _ Service, r *Rules) {
	defer func() { *e.ran = true }()
	r.AddNotification("Guard", RequiredFieldNotification{})
	r.StopIfInvalid()
	t := 0
	_ = t
}

func TestStopIfInvalid_DeferInBuildRulesStillRuns(t *testing.T) {
	ran := false
	e := &deferEntity{ran: &ran}
	ensureInit(e)

	_ = validateForInsert(e, "GetInsertable")

	if !ran {
		t.Error("a defer registered inside BuildRules must still run when the barrier unwinds")
	}
}

// The rules window is the door CompleteAsArchive comes through. It must close
// on the barrier path exactly as it does on the normal one.
func TestStopIfInvalid_RulesWindowClosesAfterStop(t *testing.T) {
	trace := []string{}
	childCalls := 0
	e := newGuardEntity(true, true, &trace, &childCalls)

	_ = validateForInsert(e, "GetInsertable")

	if e.rulesWindow {
		t.Error("the rules window was left open after a barrier unwound BuildRules")
	}
}

func TestStopIfInvalid_NilContextIsInert(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("StopIfInvalid on a context-less Rules must be inert, panicked with %v", rec)
		}
	}()
	NewRules(ModeInsert, nil, nil).StopIfInvalid()
}

// A Rules built by hand (a generated fixture, ValidateAggregateChild) has no
// pass behind it. The barrier still unwinds its own body; there is simply
// nothing queued for it to halt.
func TestStopIfInvalid_HandBuiltRulesUnwindWithoutPass(t *testing.T) {
	ctx := NewNotificationContext("t")
	r := NewRules(ModeInsert, ctx, nil)
	r.AddNotification("Guard", RequiredFieldNotification{})

	reached := false
	buildRules(func() {
		r.StopIfInvalid()
		reached = true
	})

	if reached {
		t.Error("the barrier must unwind the body even with no pass attached")
	}
}

// The manual registration path (ValidateAggregateValueObject) is a second queue
// of children, and the barrier has to cut it for the same reason it cuts the
// discovered one.
func TestStopIfInvalid_FromManuallyRegisteredChildCutsTheQueue(t *testing.T) {
	calls := 0
	root := &providerEntity{}
	ensureInit(root)
	root.ValidateAggregateValueObject("First", guardChild{Name: "first", Color: colorUnknown, barrier: true, calls: &calls})
	root.ValidateAggregateValueObject("Second", guardChild{Name: "second", Color: colorUnknown, calls: &calls})

	runAggregateValidations(root, ModeInsert, "test", &rulesPass{})

	if calls != 1 {
		t.Fatalf("child BuildRules calls = %d, want 1 — the queued sibling must not run", calls)
	}
	if keysOf([]*NotificationContext{root.NotificationContext()})["testUnknownColorNotification"] {
		t.Error("the stopping child's value objects must not validate past the barrier")
	}
}

// flatGuard is the shape most services actually write: a plain BaseEntity with
// value-object fields and no aggregate at all. `place` moves the barrier around
// so one fixture covers every position a developer can put it in.
type flatGuard struct {
	BaseEntity
	Color       testColor
	Email       testEmail
	place       string
	emit        bool
	ignoreEmail bool
	trace       *[]string
}

func (e *flatGuard) Modes() []EntityMode {
	return []EntityMode{ModeInsert, ModeUpdate, ModeDelete, ModeArchive, ModeUnarchive}
}

func (e *flatGuard) BuildRules(_ string, _ Service, r *Rules) {
	*e.trace = append(*e.trace, "start")
	if e.ignoreEmail {
		r.IgnoreValueObject("Email")
	}
	if e.emit {
		r.AddNotification("Guard", RequiredFieldNotification{})
	}
	switch e.place {
	case "between":
		r.StopIfInvalid()
	case "inClosure":
		r.IfInsertOrUpdate(func() {
			r.StopIfInvalid()
			*e.trace = append(*e.trace, "afterBarrierInsideClosure")
		})
	case "otherMode":
		r.IfDelete(func() { r.StopIfInvalid() })
	}
	*e.trace = append(*e.trace, "tail")
	r.IfInsertOrUpdate(func() { *e.trace = append(*e.trace, "closure") })
}

func newFlatGuard(place string, emit bool, trace *[]string) *flatGuard {
	e := &flatGuard{Color: colorUnknown, Email: "", place: place, emit: emit, trace: trace}
	ensureInit(e)
	return e
}

func TestStopIfInvalid_FlatEntity_HaltsBodyAndValueObjects(t *testing.T) {
	trace := []string{}
	e := newFlatGuard("between", true, &trace)

	_ = validateForInsert(e, "GetInsertable")

	if len(trace) != 1 || trace[0] != "start" {
		t.Fatalf("trace = %v, want only [start]", trace)
	}
	got := keysOf(collectContexts(e))
	if got["testUnknownColorNotification"] {
		t.Error("Color validated past the barrier")
	}
	if len(got) != 1 || !got["RequiredFieldNotification"] {
		t.Errorf("expected only the guard notification, got %v", got)
	}
}

// Control for the test above: identical entity, identical failing rule, no
// barrier. Everything runs and the response carries the value-object errors too.
func TestStopIfInvalid_FlatEntity_ControlWithoutBarrier(t *testing.T) {
	trace := []string{}
	e := newFlatGuard("none", true, &trace)

	_ = validateForInsert(e, "GetInsertable")

	if len(trace) != 3 {
		t.Fatalf("trace = %v, want start+tail+closure", trace)
	}
	if !keysOf(collectContexts(e))["testUnknownColorNotification"] {
		t.Error("Color must validate when nothing stopped the pass")
	}
}

// Raised from inside a mode closure the barrier still unwinds the WHOLE body,
// not just the closure — the developer writes one call and nothing after it runs.
func TestStopIfInvalid_InsideAClosureUnwindsTheWholeBody(t *testing.T) {
	trace := []string{}
	e := newFlatGuard("inClosure", true, &trace)

	_ = validateForInsert(e, "GetInsertable")

	for _, unwanted := range []string{"afterBarrierInsideClosure", "tail", "closure"} {
		for _, got := range trace {
			if got == unwanted {
				t.Errorf("%q ran past a barrier raised inside a closure (trace %v)", unwanted, trace)
			}
		}
	}
}

// A barrier in a closure the current verb does not dispatch never runs, so it
// cannot stop anything — the mode gate still governs.
func TestStopIfInvalid_InANonDispatchedClosureNeverFires(t *testing.T) {
	trace := []string{}
	e := newFlatGuard("otherMode", true, &trace)

	_ = validateForInsert(e, "GetInsertable") // IfDelete does not fire here

	if len(trace) != 3 {
		t.Fatalf("trace = %v, want the body to run whole", trace)
	}
}

// Bookkeeping recorded before the barrier is simply never consumed — no crash,
// no half-applied opt-out.
func TestStopIfInvalid_IgnoreValueObjectBeforeBarrierIsHarmless(t *testing.T) {
	trace := []string{}
	e := newFlatGuard("between", true, &trace)
	e.ignoreEmail = true

	_ = validateForInsert(e, "GetInsertable")

	if got := keysOf(collectContexts(e)); len(got) != 1 || !got["RequiredFieldNotification"] {
		t.Errorf("expected only the guard notification, got %v", got)
	}
}

// A notification emitted BEFORE the pass (a construction-time rejection, which
// the Get* family deliberately does not clear) is a real rejection, so a barrier
// at the top of BuildRules fires on it.
func TestStopIfInvalid_FiresOnAConstructionTimeNotification(t *testing.T) {
	trace := []string{}
	e := newFlatGuard("between", false, &trace)
	e.NotificationContext().AddNotification("Constructed", RequiredFieldNotification{})

	_ = validateForInsert(e, "GetInsertable")

	if len(trace) != 1 {
		t.Fatalf("trace = %v, want only [start] — the pre-existing rejection must trip the barrier", trace)
	}
}

// The halt state belongs to ONE pass. A second validation of the same entity
// starts clean, so a barrier can never poison a later call.
func TestStopIfInvalid_HaltDoesNotLeakIntoTheNextPass(t *testing.T) {
	trace := []string{}
	e := newFlatGuard("between", true, &trace)
	e.Color = colorRed
	e.Email = "someone@example.com"

	if ok, _ := IsValid(e, ModeInsert, nil); ok {
		t.Fatal("expected the guard to reject the first pass")
	}
	if len(trace) != 1 {
		t.Fatalf("first pass trace = %v, want only [start]", trace)
	}

	e.emit = false
	trace = trace[:0]

	ok, ctxs := IsValid(e, ModeInsert, nil)
	if !ok {
		t.Fatalf("second pass must run whole and pass; got %+v", ctxs)
	}
	if len(trace) != 3 {
		t.Fatalf("second pass trace = %v, want the whole body", trace)
	}
}

// Every public verb goes through the same seam.
func TestStopIfInvalid_ThroughThePublicVerbs(t *testing.T) {
	id := NewID("guard-id")
	for _, tc := range []struct {
		name string
		run  func(*flatGuard) error
	}{
		{"GetInsertable", func(e *flatGuard) error { _, err := GetInsertable(e, nil, "GetInsertable"); return err }},
		{"GetDeletable", func(e *flatGuard) error { _, err := GetDeletable(e, nil, "GetDeletable"); return err }},
		{"GetArchivable", func(e *flatGuard) error { _, err := GetArchivable(e, nil, "GetArchivable"); return err }},
		{"GetUnarchivable", func(e *flatGuard) error { _, err := GetUnarchivable(e, nil, "GetUnarchivable"); return err }},
		{"GetUpdatable", func(e *flatGuard) error {
			_, err := GetUpdatable(e, func(*flatGuard) error { return nil }, nil, "GetUpdatable")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := []string{}
			e := newFlatGuard("between", true, &trace)
			if tc.name != "GetInsertable" {
				e.SetID(id)
			}

			err := tc.run(e)
			if err == nil {
				t.Fatal("expected the guard to reject the write")
			}
			if len(trace) != 1 {
				t.Fatalf("trace = %v, want only [start]", trace)
			}
			if keysOf(collectContexts(e))["testUnknownColorNotification"] {
				t.Error("value objects validated past the barrier")
			}
		})
	}
}

// The manual ValidateAggregateValueObject queue runs AFTER the discovered
// collections, so a barrier raised in a discovered child must cut it too.
func TestStopIfInvalid_FromDiscoveredChildCutsTheManualQueue(t *testing.T) {
	calls := 0
	root := &providerEntity{}
	ensureInit(root)
	root.AggregateConstructor([]AggregateValueObject{
		guardChild{Name: "first", Color: colorUnknown, barrier: true, calls: &calls},
	})
	root.ValidateAggregateValueObject("Queued", guardChild{Name: "queued", calls: &calls})

	runAggregateValidations(root, ModeInsert, "test", &rulesPass{})

	if calls != 1 {
		t.Fatalf("child BuildRules calls = %d, want 1 — the manual queue must be cut too", calls)
	}
}

// ValidateAggregateChild is a standalone primitive with no pass behind it. A
// barrier raised there must still unwind that child's body and never escape.
func TestStopIfInvalid_InValidateAggregateChildDoesNotEscape(t *testing.T) {
	calls := 0
	root := &providerEntity{}
	ensureInit(root)

	ok := ValidateAggregateChild(root, guardChild{Name: "x", Color: colorUnknown, barrier: true, calls: &calls}, ModeInsert, "test", nil)

	if ok {
		t.Error("a child that emitted a notification must not report clean")
	}
	if calls != 1 {
		t.Fatalf("child BuildRules calls = %d, want 1", calls)
	}
}

// completionEntity2 pairs the barrier with CompleteAsArchive: the barrier is the
// guard, the completion is the decision below it.
type completionEntity2 struct {
	BaseEntity
	emit    bool
	barrier bool
}

func (e *completionEntity2) Modes() []EntityMode {
	return []EntityMode{ModeUpdate, ModeArchive}
}
func (e *completionEntity2) BuildRules(_ string, _ Service, r *Rules) {
	if e.emit {
		r.AddNotification("Guard", RequiredFieldNotification{})
	}
	if e.barrier {
		r.StopIfInvalid()
	}
	r.IfUpdate(func() { e.CompleteAsArchive() })
}

func TestStopIfInvalid_BarrierBeforeCompleteAsArchiveSkipsTheTransition(t *testing.T) {
	e := &completionEntity2{emit: true, barrier: true}
	ensureInit(e)
	id := NewID("guard-id")
	e.SetID(id)

	_ = validateForUpdate(e, "GetUpdatable")

	if e.requestedMode != ModeUnknown {
		t.Errorf("requestedMode = %v, want ModeUnknown — the barrier cut the closure that requests it", e.requestedMode)
	}
	if e.rulesWindow {
		t.Error("the rules window must close on the barrier path")
	}
}

func TestStopIfInvalid_WithoutBarrierCompleteAsArchiveStillRuns(t *testing.T) {
	e := &completionEntity2{emit: true, barrier: false}
	ensureInit(e)
	id := NewID("guard-id")
	e.SetID(id)

	_ = validateForUpdate(e, "GetUpdatable")

	if e.requestedMode != ModeArchive {
		t.Errorf("requestedMode = %v, want ModeArchive", e.requestedMode)
	}
}

// A panic raised inside an aggregate CHILD's rules must propagate too — the
// child seat recovers the stop signal by type, nothing else.
type panicChild struct {
	Managed
	Name string
}

func (c panicChild) CollectionName() string { return "PanicChilds" }
func (c panicChild) IsSameBusinessIdentity(o AggregateValueObject) bool {
	x, ok := o.(panicChild)
	return ok && c.Name == x.Name
}
func (c panicChild) BuildRules(_ string, _ Service, _ *Rules) {
	panic("a genuine bug in a child rule")
}

func TestStopIfInvalid_GenuinePanicInAChildStillPropagates(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("a real panic in a child must reach the caller")
		}
		if rec != "a genuine bug in a child rule" {
			t.Fatalf("panic value = %v, want it re-panicked intact", rec)
		}
	}()
	root := &providerEntity{}
	ensureInit(root)
	root.AggregateConstructor([]AggregateValueObject{panicChild{Name: "boom"}})

	runAggregateValidations(root, ModeInsert, "test", &rulesPass{})
}
