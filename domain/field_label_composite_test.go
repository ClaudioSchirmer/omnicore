package domain

import (
	"reflect"
	"testing"
)

// A COMPOSITE value object carries several fields and emits its notifications on
// them, so the label plan has to see through it: "Street" is not a field of the
// entity, and without the hop a part-level notification would ship with no
// label at all.

type lblZip string

func (z lblZip) Value() string                             { return string(z) }
func (z lblZip) IsValid(string, *NotificationContext) bool { return true }

type lblAddress struct {
	Street  string `labelKey:"AddressStreetField"`
	ZipCode lblZip `labelKey:"AddressZipField"`
	Ignored string // no tag — absent from the plan
	private string //nolint:unused // unexported — never walked
}

func (a lblAddress) IsValid(string, *NotificationContext) bool { return true }

type lblCarrier struct {
	BaseEntity
	Name     string     `labelKey:"CarrierNameField"`
	Address  lblAddress `labelKey:"CarrierAddressField"`
	Optional *lblAddress
}

func (c *lblCarrier) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (c *lblCarrier) BuildRules(string, Service, *Rules) {}

func TestLabelPlan_ResolvesCompositePartsByLeafName(t *testing.T) {
	ct := reflect.TypeOf(lblCarrier{})
	if got := resolveLabelKey(ct, "Street"); got != "AddressStreetField" {
		t.Errorf("label of the part %q = %q, want AddressStreetField — the value object owns its vocabulary", "Street", got)
	}
	if got := resolveLabelKey(ct, "ZipCode"); got != "AddressZipField" {
		t.Errorf("label of a value-object part = %q, want AddressZipField", got)
	}
	// The composite's own field keeps its label: a rule about the value object as
	// a WHOLE emits on the entity's field name.
	if got := resolveLabelKey(ct, "Address"); got != "CarrierAddressField" {
		t.Errorf("label of the composite field = %q, want CarrierAddressField", got)
	}
	if got := resolveLabelKey(ct, "Name"); got != "CarrierNameField" {
		t.Errorf("label of a plain field = %q, want CarrierNameField", got)
	}
	if got := resolveLabelKey(ct, "Ignored"); got != "" {
		t.Errorf("an untagged part = %q, want empty", got)
	}
}

func TestLabelPlan_OptionalCompositeIsWalkedToo(t *testing.T) {
	// The plan is built from the TYPE, so a nil *Address still contributes its
	// parts' labels — absence at runtime is not absence at declaration.
	type optOnly struct {
		Optional *lblAddress
	}
	if got := resolveLabelKey(reflect.TypeOf(optOnly{}), "Street"); got != "AddressStreetField" {
		t.Errorf("optional composite part label = %q, want AddressStreetField", got)
	}
}

func TestLabelPlan_EntityFieldWinsOverAPartOfTheSameName(t *testing.T) {
	type clash struct {
		Street  string     `labelKey:"EntityStreetField"`
		Address lblAddress `labelKey:"AddressField"`
	}
	if got := resolveLabelKey(reflect.TypeOf(clash{}), "Street"); got != "EntityStreetField" {
		t.Errorf("label of Street = %q — the entity's own field must not be shadowed by a part", got)
	}
}

func TestCompositeValueObjectType_Discrimination(t *testing.T) {
	// A composite: struct, owns a rule, no Value().
	if _, ok := compositeValueObjectType(reflect.TypeOf(lblAddress{})); !ok {
		t.Error("lblAddress must be recognized as a composite value object")
	}
	// The optional form resolves to the same type.
	if got, ok := compositeValueObjectType(reflect.TypeOf(&lblAddress{})); !ok || got != reflect.TypeOf(lblAddress{}) {
		t.Errorf("*lblAddress = %v,%v, want lblAddress,true", got, ok)
	}
	// A SCALAR value object declares Value() — one column, never decomposed.
	if _, ok := compositeValueObjectType(reflect.TypeOf(lblZip(""))); ok {
		t.Error("a scalar value object must not be treated as composite")
	}
	// domain.ID is a struct that satisfies the value-object contract, but it has
	// Value() and its own dialect-native encoding.
	if _, ok := compositeValueObjectType(reflect.TypeOf(ID{})); ok {
		t.Error("domain.ID must never be treated as a composite value object")
	}
	// A plain struct owns no rule.
	if _, ok := compositeValueObjectType(reflect.TypeOf(struct{ A string }{})); ok {
		t.Error("a struct without IsValid is not a value object")
	}
	if _, ok := compositeValueObjectType(reflect.TypeOf("")); ok {
		t.Error("a non-struct is not a composite value object")
	}
}
