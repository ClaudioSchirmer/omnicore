package domain

import (
	"reflect"
	"testing"
)

// --- test enums -------------------------------------------------------------

type testColor int

const (
	colorUnknown testColor = 0
	colorRed     testColor = 1
	colorGreen   testColor = 2
	colorBlue    testColor = 3
)

var testColorMembers = []testColor{colorRed, colorGreen, colorBlue}

func (c testColor) Value() int                        { return int(c) }
func (c testColor) Values() []testColor               { return testColorMembers }
func (c testColor) UnknownNotification() Notification { return testUnknownColorNotification{} }

type testUnknownColorNotification struct{ DomainNotificationBase }

// requireEnumVO instantiates only if E satisfies the full EnumValueObject[E, T]
// contract — a compile-time proof both backings conform (a comparable-embedding
// interface can only be used as a constraint, not as a variable type).
func requireEnumVO[E EnumValueObject[E, T], T comparable]() {}

var (
	_ = requireEnumVO[testColor, int]
	_ = requireEnumVO[testSize, string]
)

type testSize string

const (
	sizeUnknown testSize = ""
	sizeSmall   testSize = "SMALL"
	sizeLarge   testSize = "LARGE"
)

var testSizeMembers = []testSize{sizeSmall, sizeLarge}

func (s testSize) Value() string                     { return string(s) }
func (s testSize) Values() []testSize                { return testSizeMembers }
func (s testSize) UnknownNotification() Notification { return testUnknownSizeNotification{} }

type testUnknownSizeNotification struct{ DomainNotificationBase }

// a raw ValueObject (writes its own IsValid) — the other ValidateValueObject branch.
type testEmail string

func (e testEmail) Value() string { return string(e) }
func (e testEmail) IsValid(fieldName string, ctx *NotificationContext) bool {
	if string(e) == "" {
		ctx.AddNotification(fieldName, RequiredFieldNotification{})
		return false
	}
	return true
}

// --- ValidateEnum -----------------------------------------------------------

func TestValidateEnum_Membership(t *testing.T) {
	ctx := NewNotificationContext("X")
	if !ValidateEnum(colorRed, "color", ctx) {
		t.Error("colorRed is a member — should validate")
	}
	if ValidateEnum(colorUnknown, "color", ctx) {
		t.Error("colorUnknown (sentinel) should not validate")
	}
	if ValidateEnum(testColor(99), "color", ctx) {
		t.Error("out-of-range should not validate — membership is enforced")
	}
	// String-backed enum validates the same way.
	if !ValidateEnum(sizeSmall, "size", ctx) {
		t.Error("sizeSmall is a member — should validate")
	}
	if ValidateEnum(sizeUnknown, "size", ctx) {
		t.Error("sizeUnknown (empty sentinel) should not validate")
	}
	// nil ctx must not panic on the failure path.
	if ValidateEnum(colorUnknown, "color", nil) {
		t.Error("colorUnknown should fail even with a nil ctx")
	}
}

// --- EnumByValue ------------------------------------------------------------

func TestEnumByValue_Int(t *testing.T) {
	if got := EnumByValue[testColor](2); got != colorGreen {
		t.Errorf("EnumByValue(2) = %v, want colorGreen", got)
	}
	// JSON numbers decode to float64.
	if got := EnumByValue[testColor](float64(3)); got != colorBlue {
		t.Errorf("EnumByValue(3.0) = %v, want colorBlue", got)
	}
	// Unknown input converges to the zero sentinel.
	if got := EnumByValue[testColor](99); got != colorUnknown {
		t.Errorf("EnumByValue(99) = %v, want colorUnknown", got)
	}
	// A string handed to an int enum matches nothing → sentinel.
	if got := EnumByValue[testColor]("2"); got != colorUnknown {
		t.Errorf("EnumByValue(\"2\") = %v, want colorUnknown", got)
	}
}

func TestEnumByValue_String(t *testing.T) {
	if got := EnumByValue[testSize]("LARGE"); got != sizeLarge {
		t.Errorf("EnumByValue(\"LARGE\") = %v, want sizeLarge", got)
	}
	if got := EnumByValue[testSize]("XL"); got != sizeUnknown {
		t.Errorf("EnumByValue(\"XL\") = %v, want sizeUnknown", got)
	}
}

// --- EnumDescriptionKey -----------------------------------------------------

func TestEnumDescriptionKey(t *testing.T) {
	if got := EnumDescriptionKey(colorRed); got != "testColor.1" {
		t.Errorf("EnumDescriptionKey(colorRed) = %q, want testColor.1", got)
	}
	if got := EnumDescriptionKey(sizeSmall); got != "testSize.SMALL" {
		t.Errorf("EnumDescriptionKey(sizeSmall) = %q, want testSize.SMALL", got)
	}
	c := colorBlue
	if got := EnumDescriptionKey(&c); got != "testColor.3" {
		t.Errorf("EnumDescriptionKey(&colorBlue) = %q, want testColor.3", got)
	}
}

// --- enumMembershipValidator (the reflective ValidateValueObject seam) ------------

func TestEnumMembershipValidator(t *testing.T) {
	ctx := NewNotificationContext("X")
	if !(enumMembershipValidator{e: colorGreen}).IsValid("color", ctx) {
		t.Error("colorGreen should validate via the reflective seam")
	}
	if (enumMembershipValidator{e: colorUnknown}).IsValid("color", ctx) {
		t.Error("colorUnknown should fail via the reflective seam")
	}
	if !ctx.HasErrors() {
		t.Error("expected a notification recorded for colorUnknown")
	}
}

// labeledEntity is a root that declares a labelKey on an enum value-object
// field — used to prove the field label reaches the emitted notification.
type labeledEntity struct {
	BaseEntity
	Color testColor `labelKey:"ColorField"`
}

func (e *labeledEntity) Modes() []EntityMode                      { return []EntityMode{ModeInsert} }
func (e *labeledEntity) BuildRules(_ string, _ Service, _ *Rules) {} // Color is a field → auto-discovered

// TestValueObjectNotification_CarriesLabelKey guards the regression where a value
// object emitting via NotificationContext.AddNotification lost the field's
// labelKey (the context now carries the entity type and resolves it at emit).
func TestValueObjectNotification_CarriesLabelKey(t *testing.T) {
	e := &labeledEntity{Color: colorUnknown} // sentinel → fails membership
	ok, ctxs := IsValid(e, ModeInsert, nil)
	if ok {
		t.Fatal("expected validation to fail on the unknown color")
	}
	var found *NotificationMessage
	for _, c := range ctxs {
		msgs := c.Messages()
		for i := range msgs {
			if NotificationKey(msgs[i].Notification) == "testUnknownColorNotification" {
				found = &msgs[i]
			}
		}
	}
	if found == nil {
		t.Fatalf("expected testUnknownColorNotification; got %+v", ctxs)
	}
	if found.LabelKey != "ColorField" {
		t.Errorf("LabelKey = %q, want ColorField (VO notification lost the labelKey)", found.LabelKey)
	}
}

// autoEntity declares VO fields but calls NO ValidateValueObject — discovery
// validates them. Ghost is a nil pointer (skipped); ignoreEmail opts Email out.
type autoEntity struct {
	BaseEntity
	Color       testColor `labelKey:"ColorField"`
	Email       testEmail
	Ghost       *testColor
	ignoreEmail bool
}

func (e *autoEntity) Modes() []EntityMode { return []EntityMode{ModeInsert} }
func (e *autoEntity) BuildRules(_ string, _ Service, r *Rules) {
	if e.ignoreEmail {
		r.IgnoreValueObject("Email")
	}
}

func TestValueObjects_AutoDiscovered(t *testing.T) {
	e := &autoEntity{Color: colorUnknown, Email: ""} // both invalid; Ghost nil → skipped
	ok, ctxs := IsValid(e, ModeInsert, nil)
	if ok {
		t.Fatal("expected validation to fail — VO fields must auto-validate")
	}
	got := map[string]bool{}
	for _, c := range ctxs {
		for _, m := range c.Messages() {
			got[NotificationKey(m.Notification)] = true
		}
	}
	if !got["testUnknownColorNotification"] {
		t.Error("Color field was not auto-discovered/validated")
	}
	if !got["RequiredFieldNotification"] {
		t.Error("Email field was not auto-discovered/validated")
	}
}

func TestValueObjects_IgnoreOptsOut(t *testing.T) {
	// Color valid; Email empty BUT ignored; Ghost nil → the entity is valid.
	e := &autoEntity{Color: colorRed, Email: "", ignoreEmail: true}
	ok, ctxs := IsValid(e, ModeInsert, nil)
	if !ok {
		t.Fatalf("expected valid (Color ok, Email ignored, Ghost nil); got %+v", ctxs)
	}
}

// voChild is an aggregate value object with a value-object field — proves the
// AVO's VOs auto-validate (via runAggregateValidations → validateValueObjectFields),
// exactly like the root, with no manual wiring in its BuildRules.
type voChild struct {
	Managed
	Color testColor
}

func (voChild) BuildRules(_ string, _ Service, _ *Rules) {}
func (c voChild) IsSameBusinessIdentity(o AggregateValueObject) bool {
	x, ok := o.(voChild)
	return ok && c.Color == x.Color
}

func TestAVO_ValueObjectFields_AutoDiscovered(t *testing.T) {
	root := &providerEntity{}
	ensureInit(root)
	root.ValidateAggregateValueObject("VOChild", voChild{Color: colorUnknown}) // invalid enum

	runAggregateValidations(root, ModeInsert, "test", nil)

	found := false
	for _, m := range root.NotificationContext().Messages() {
		if NotificationKey(m.Notification) == "testUnknownColorNotification" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the AVO's Color VO to auto-validate; got %+v", root.NotificationContext().Messages())
	}
}

func TestIsValid_DispatchesArchiveAndUnarchive(t *testing.T) {
	// plainEntity declares only ModeInsert, so archive/unarchive must be rejected.
	// Before the fix, ModeArchive/ModeUnarchive fell through the IsValid switch as
	// a silent no-op and returned (true, nil).
	if ok, _ := IsValid(&plainEntity{}, ModeArchive, nil); ok {
		t.Error("IsValid(ModeArchive) must run validateForArchive (entity disallows archive)")
	}
	if ok, _ := IsValid(&plainEntity{}, ModeUnarchive, nil); ok {
		t.Error("IsValid(ModeUnarchive) must run validateForUnarchive (entity disallows unarchive)")
	}
}

func TestValidateValueObject_Routing(t *testing.T) {
	r := NewRules(ModeInsert, nil, nil)
	r.ValidateValueObject("color", colorRed)         // enum branch
	r.ValidateValueObject("email", testEmail("a@b")) // raw ValueObject branch
	if len(r.forcedVOs) != 2 {
		t.Fatalf("expected 2 forced value objects, got %d", len(r.forcedVOs))
	}
	// neither a ValueObject nor an EnumValueObject → panic.
	defer func() {
		if recover() == nil {
			t.Error("expected ValidateValueObject to panic on a non-value-object")
		}
	}()
	r.ValidateValueObject("bad", 123)
}

// --- persistence seam: Is*/ValueObjectValue/NewValueObjectValue -------------

func TestIsValueObject_and_IsEnumValueObject(t *testing.T) {
	// raw VO → (true, false)
	if !IsValueObject(testEmail("a@b")) || IsEnumValueObject(testEmail("a@b")) {
		t.Error("raw VO must be IsValueObject && !IsEnumValueObject")
	}
	// enum VO → (false, true): the concrete enum has no IsValid (it lives on the adapter)
	if IsValueObject(colorRed) || !IsEnumValueObject(colorRed) {
		t.Error("enum VO must be !IsValueObject && IsEnumValueObject")
	}
	if IsValueObject(sizeSmall) || !IsEnumValueObject(sizeSmall) {
		t.Error("string enum VO must be !IsValueObject && IsEnumValueObject")
	}
	// plain scalar → (false, false)
	if IsValueObject("plain") || IsEnumValueObject(42) {
		t.Error("a plain scalar is neither kind of value object")
	}
}

func TestValueObjectValue(t *testing.T) {
	if v, ok := ValueObjectValue(testEmail("a@b.com")); !ok || v != "a@b.com" {
		t.Errorf("ValueObjectValue(Email) = %v,%v want a@b.com,true", v, ok)
	}
	if v, ok := ValueObjectValue(colorBlue); !ok || v != 3 {
		t.Errorf("ValueObjectValue(colorBlue) = %v,%v want 3,true", v, ok)
	}
	if v, ok := ValueObjectValue(sizeLarge); !ok || v != "LARGE" {
		t.Errorf("ValueObjectValue(sizeLarge) = %v,%v want LARGE,true", v, ok)
	}
	// pointer VO unwraps through one deref
	c := colorGreen
	if v, ok := ValueObjectValue(&c); !ok || v != 2 {
		t.Errorf("ValueObjectValue(&colorGreen) = %v,%v want 2,true", v, ok)
	}
	// nil pointer VO → (nil,false) so the caller maps it to SQL NULL
	var np *testColor
	if v, ok := ValueObjectValue(np); ok || v != nil {
		t.Errorf("ValueObjectValue(nil *enum) = %v,%v want nil,false", v, ok)
	}
	// non-VO → (nil,false)
	if v, ok := ValueObjectValue("plain"); ok || v != nil {
		t.Errorf("ValueObjectValue(plain) = %v,%v want nil,false", v, ok)
	}
}

func TestNewValueObjectValue_RawConvert(t *testing.T) {
	got, err := NewValueObjectValue(reflect.TypeOf(testEmail("")), "x@y.com")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if e, ok := got.(testEmail); !ok || e != "x@y.com" {
		t.Errorf("NewValueObjectValue(Email,\"x@y.com\") = %#v, want testEmail(\"x@y.com\")", got)
	}
	// width tolerance: an int64 from a driver builds an int-backed VO
	// (covered by the enum path below); a bad conversion errors.
	if _, err := NewValueObjectValue(reflect.TypeOf(testEmail("")), struct{}{}); err == nil {
		t.Error("expected an error building a string VO from a struct")
	}
}

func TestNewValueObjectValue_EnumConverges(t *testing.T) {
	ct := reflect.TypeOf(colorUnknown) // testColor
	// valid member → the member itself
	got, err := NewValueObjectValue(ct, 2)
	if err != nil || got.(testColor) != colorGreen {
		t.Errorf("NewValueObjectValue(testColor,2) = %v,%v want colorGreen,nil", got, err)
	}
	// int64 from a driver still converges (sameUnderlying tolerates widths)
	if got, _ := NewValueObjectValue(ct, int64(3)); got.(testColor) != colorBlue {
		t.Errorf("NewValueObjectValue(testColor,int64(3)) = %v want colorBlue", got)
	}
	// OUT-OF-SET → Unknown sentinel (D3)
	if got, _ := NewValueObjectValue(ct, 99); got.(testColor) != colorUnknown {
		t.Errorf("NewValueObjectValue(testColor,99) = %v want colorUnknown (converge)", got)
	}
	// string enum: member and out-of-set
	st := reflect.TypeOf(sizeUnknown) // testSize
	if got, _ := NewValueObjectValue(st, "LARGE"); got.(testSize) != sizeLarge {
		t.Errorf("NewValueObjectValue(testSize,\"LARGE\") = %v want sizeLarge", got)
	}
	if got, _ := NewValueObjectValue(st, "XL"); got.(testSize) != sizeUnknown {
		t.Errorf("NewValueObjectValue(testSize,\"XL\") = %v want sizeUnknown (converge)", got)
	}
}

func (voChild) CollectionName() string { return "VoChilds" }
