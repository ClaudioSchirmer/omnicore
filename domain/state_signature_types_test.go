package domain

import "testing"

// Every shape a persisted field can take must move the signature when it
// changes. A shape the walk cannot see is worse than no check at all: it reports
// "unchanged" for state that did change, which is exactly the lie the signature
// exists to prevent.

// --- the raw kind: a named type over a scalar, owning its own rule ---

type sigEmail string

func (e sigEmail) Value() string { return string(e) }
func (e sigEmail) IsValid(string, *NotificationContext) bool {
	return len(e) > 0
}

type sigAmount int64

func (a sigAmount) Value() int64                              { return int64(a) }
func (a sigAmount) IsValid(string, *NotificationContext) bool { return a >= 0 }

// --- the enum kind: a closed set, zero is Unknown ---

type sigStatus int

const (
	sigStatusUnknown sigStatus = iota
	sigStatusActive
	sigStatusSuspended
)

func (s sigStatus) Value() int64                      { return int64(s) }
func (s sigStatus) Values() []sigStatus               { return []sigStatus{sigStatusActive, sigStatusSuspended} }
func (s sigStatus) UnknownNotification() Notification { return RequiredFieldNotification{} }

type sigKind string

const sigKindRetail sigKind = "retail"

func (k sigKind) Value() string                     { return string(k) }
func (k sigKind) Values() []sigKind                 { return []sigKind{sigKindRetail} }
func (k sigKind) UnknownNotification() Notification { return RequiredFieldNotification{} }

// --- the composite kind: one rule, several columns, parts exported because the
// schema decomposes them and WriteFields has to read them ---

type sigAddress struct {
	Street string
	Number int
	Zip    string
}

func (a sigAddress) IsValid(string, *NotificationContext) bool { return a.Street != "" }

// typedEntity carries one field of every persistable shape, including the
// reference kinds — a domain.ID field is how one aggregate points at another.
type typedEntity struct {
	AggregateRoot

	Email  sigEmail
	Amount sigAmount

	Status sigStatus
	Kind   sigKind

	Address   sigAddress
	MaybeAddr *sigAddress

	CategoryID domain0ID
	OwnerID    *domain0ID
}

// domain0ID keeps the test honest about the real type without importing it under
// another name — it IS domain.ID.
type domain0ID = ID

func (e *typedEntity) Modes() []EntityMode                { return []EntityMode{ModeUpdate} }
func (e *typedEntity) BuildRules(string, Service, *Rules) {}

func newTypedEntity() *typedEntity {
	addr := sigAddress{Street: "Second", Number: 2, Zip: "02"}
	owner := NewID("owner-1")
	e := &typedEntity{
		Email:      "a@x.com",
		Amount:     100,
		Status:     sigStatusActive,
		Kind:       sigKindRetail,
		Address:    sigAddress{Street: "Main", Number: 1, Zip: "01"},
		MaybeAddr:  &addr,
		CategoryID: NewID("cat-1"),
		OwnerID:    &owner,
	}
	e.SetID(NewID("fixed-root"))
	ensureInit(e)
	return e
}

func TestStateSignature_EveryPersistableShapeMoves(t *testing.T) {
	base := stateSignature(newTypedEntity())

	for _, tc := range []struct {
		shape  string
		mutate func(e *typedEntity)
	}{
		{"a raw string value object", func(e *typedEntity) { e.Email = "b@x.com" }},
		{"a raw int value object", func(e *typedEntity) { e.Amount = 101 }},
		{"an int enum value object", func(e *typedEntity) { e.Status = sigStatusSuspended }},
		{"an int enum falling back to Unknown", func(e *typedEntity) { e.Status = sigStatusUnknown }},
		{"a string enum value object", func(e *typedEntity) { e.Kind = "wholesale" }},
		{"one part of a composite value object", func(e *typedEntity) { e.Address.Number = 9 }},
		{"another part of the same composite", func(e *typedEntity) { e.Address.Zip = "99" }},
		{"a nullable composite becoming nil", func(e *typedEntity) { e.MaybeAddr = nil }},
		{"a part inside a nullable composite", func(e *typedEntity) { e.MaybeAddr.Street = "Third" }},
		{"an ID reference", func(e *typedEntity) { e.CategoryID = NewID("cat-2") }},
		{"a nullable ID reference", func(e *typedEntity) { o := NewID("owner-2"); e.OwnerID = &o }},
		{"a nullable ID reference becoming nil", func(e *typedEntity) { e.OwnerID = nil }},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			e := newTypedEntity()
			tc.mutate(e)
			if stateSignature(e) == base {
				t.Errorf("changing %s must move the signature — the walk cannot see this shape", tc.shape)
			}
		})
	}
}

// Two entities holding equal values must agree, or every write would be refused.
func TestStateSignature_EqualStateAgreesAcrossInstances(t *testing.T) {
	if stateSignature(newTypedEntity()) != stateSignature(newTypedEntity()) {
		t.Error("two entities built with the same values must hash the same")
	}
}

// The trap the ID reference fell into, pinned so a future type cannot fall in
// silently: a struct whose fields are all unexported is INVISIBLE to the walk.
// Any framework type shaped that way and usable as an entity field must get an
// explicit case in hashValue, the way ID and time.Time have.
type opaqueValue struct{ hidden string }

type opaqueEntity struct {
	AggregateRoot
	Opaque opaqueValue
	Ref    ID
}

func (e *opaqueEntity) Modes() []EntityMode                { return []EntityMode{ModeUpdate} }
func (e *opaqueEntity) BuildRules(string, Service, *Rules) {}

func TestStateSignature_OpaqueStructsAreBlindUnlessHandled(t *testing.T) {
	build := func(hidden, ref string) uint64 {
		e := &opaqueEntity{Opaque: opaqueValue{hidden: hidden}, Ref: NewID(ref)}
		e.SetID(NewID("fixed"))
		ensureInit(e)
		return stateSignature(e)
	}

	// The general property: an unexported-only struct cannot be seen. This is not
	// a defect on its own — such a field cannot be persisted either, because
	// WriteFields reads through the same exported-field rule.
	if build("a", "r") != build("b", "r") {
		t.Error("premise changed: the walk now sees unexported struct fields — revisit which types need a case")
	}

	// ID is the exception that MUST be handled: it keeps its value unexported and
	// it IS persisted, as the reference from one aggregate to another.
	if build("a", "r1") == build("a", "r2") {
		t.Error("an ID reference must move the signature — hashValue needs its explicit case")
	}
}
