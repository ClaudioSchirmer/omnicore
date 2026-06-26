package domain

import (
	"reflect"
	"testing"
)

// Test fixtures — covering the canonical declaration shapes a consumer service
// will use plus the corner cases the resolver must skip silently.

type fixtureUser struct {
	Name      string  `labelKey:"UserNameField"`
	Email     string  `labelKey:"UserEmailField"`
	Phone     *string `labelKey:"UserPhoneField"`
	NoTag     string
	SkippedV1 string `labelKey:"-"`
	SkippedV2 string `labelKey:""`
	private   string `labelKey:"NeverReached"` // unexported; reflection plan does not see it
}

type fixtureAddress struct {
	Street  string `labelKey:"AddressStreetField"`
	ZipCode string `labelKey:"AddressZipCodeField"`
}

// Embedded struct exercising the anonymous-embed flattening. The embedded
// type MUST be exported because Go marks anonymous embeds of unexported types
// as unexported fields (reflect.StructField.IsExported() = false), and the
// resolver — by design, mirroring the tvar plan-builder — skips unexported
// fields entirely. The canonical framework embeds (BaseEntity, AggregateRoot)
// are exported, so this matches real-world consumer code.
type FixtureBase struct {
	BaseField string `labelKey:"BaseFieldKey"`
}

type fixtureChildWithEmbed struct {
	FixtureBase
	Own string `labelKey:"OwnFieldKey"`
}

// Anonymous field whose later-declared parent override SHADOWS the embedded
// label — mirroring Go's own field-promotion semantics where an outer field
// with the same name as an embedded field wins. buildLabelPlan respects this:
// the embed pass adds out["BaseField"] first, then the outer field overwrites
// it via the unconditional `out[f.Name] = tag`.
type fixtureShadowEmbed struct {
	FixtureBase
	BaseField string `labelKey:"ParentLevelKey"` // parent shadows embed (Go semantic)
}

func TestResolveLabelKey_TagOnExportedField(t *testing.T) {
	got := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "Name")
	if got != "UserNameField" {
		t.Errorf("resolveLabelKey(Name) = %q, want %q", got, "UserNameField")
	}
}

func TestResolveLabelKey_AVO(t *testing.T) {
	got := resolveLabelKey(reflect.TypeOf(fixtureAddress{}), "ZipCode")
	if got != "AddressZipCodeField" {
		t.Errorf("resolveLabelKey(ZipCode) = %q, want %q", got, "AddressZipCodeField")
	}
}

func TestResolveLabelKey_PointerType(t *testing.T) {
	// reflect.TypeOf(&fixtureUser{}) returns *fixtureUser — resolver must unwrap.
	got := resolveLabelKey(reflect.TypeOf(&fixtureUser{}), "Email")
	if got != "UserEmailField" {
		t.Errorf("resolveLabelKey via pointer = %q, want %q", got, "UserEmailField")
	}
}

func TestResolveLabelKey_NoTagReturnsEmpty(t *testing.T) {
	if got := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "NoTag"); got != "" {
		t.Errorf("resolveLabelKey(NoTag) = %q, want empty", got)
	}
}

func TestResolveLabelKey_DashSkipsField(t *testing.T) {
	if got := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "SkippedV1"); got != "" {
		t.Errorf("resolveLabelKey(SkippedV1, label:\"-\") = %q, want empty", got)
	}
}

func TestResolveLabelKey_EmptyTagValueSkipped(t *testing.T) {
	if got := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "SkippedV2"); got != "" {
		t.Errorf("resolveLabelKey(SkippedV2, label:\"\") = %q, want empty", got)
	}
}

func TestResolveLabelKey_UnexportedFieldSkipped(t *testing.T) {
	// Caller will never realistically pass an unexported name (BuildRules emits
	// Go identifiers), but the resolver still has to stay safe under the
	// defensive contract.
	if got := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "private"); got != "" {
		t.Errorf("resolveLabelKey(private) = %q, want empty", got)
	}
}

func TestResolveLabelKey_AbsentFieldName(t *testing.T) {
	// Defensive — typo'd field name from a BuildRules caller should not panic;
	// just returns empty.
	if got := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "Naem"); got != "" {
		t.Errorf("resolveLabelKey(typo'd name) = %q, want empty", got)
	}
}

func TestResolveLabelKey_NilType(t *testing.T) {
	if got := resolveLabelKey(nil, "Name"); got != "" {
		t.Errorf("resolveLabelKey(nil type) = %q, want empty", got)
	}
}

func TestResolveLabelKey_EmptyFieldName(t *testing.T) {
	if got := resolveLabelKey(reflect.TypeOf(fixtureUser{}), ""); got != "" {
		t.Errorf("resolveLabelKey(empty name) = %q, want empty", got)
	}
}

func TestResolveLabelKey_NonStructType(t *testing.T) {
	// int is not a struct — must not panic, returns empty.
	if got := resolveLabelKey(reflect.TypeOf(42), "anything"); got != "" {
		t.Errorf("resolveLabelKey(int type) = %q, want empty", got)
	}
}

func TestResolveLabelKey_EmbeddedFieldFlattened(t *testing.T) {
	// BaseField lives on fixtureBase but is promoted via anonymous embed on
	// fixtureChildWithEmbed. The resolver must reach it through the same
	// Go-name lookup the caller would use.
	if got := resolveLabelKey(reflect.TypeOf(fixtureChildWithEmbed{}), "BaseField"); got != "BaseFieldKey" {
		t.Errorf("resolveLabelKey(embedded BaseField) = %q, want BaseFieldKey", got)
	}
	if got := resolveLabelKey(reflect.TypeOf(fixtureChildWithEmbed{}), "Own"); got != "OwnFieldKey" {
		t.Errorf("resolveLabelKey(Own) = %q, want OwnFieldKey", got)
	}
}

func TestResolveLabelKey_ParentShadowsEmbed(t *testing.T) {
	// fixtureShadowEmbed declares its own BaseField with `labelKey:"ParentLevelKey"`
	// while also embedding FixtureBase which has BaseField with `labelKey:"BaseFieldKey"`.
	// Mirroring Go's field-promotion semantics (outer field shadows embedded
	// field of the same name), the parent label wins.
	if got := resolveLabelKey(reflect.TypeOf(fixtureShadowEmbed{}), "BaseField"); got != "ParentLevelKey" {
		t.Errorf("resolveLabelKey shadow embed = %q, want ParentLevelKey (parent shadows)", got)
	}
}

func TestResolveLabelKey_CacheReturnsSameMap(t *testing.T) {
	t1 := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "Name")
	t2 := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "Email")
	if t1 != "UserNameField" || t2 != "UserEmailField" {
		t.Fatalf("cache hit must still resolve: got %q / %q", t1, t2)
	}
	// Cache identity check — buildLabelPlan must run at most once per type.
	t3 := resolveLabelKey(reflect.TypeOf(fixtureUser{}), "Name")
	if t1 != t3 {
		t.Errorf("cached lookup produced different value: %q vs %q", t1, t3)
	}
	// And via the underlying loader — same pointer.
	planA := loadLabelPlan(reflect.TypeOf(fixtureUser{}))
	planB := loadLabelPlan(reflect.TypeOf(fixtureUser{}))
	if reflect.ValueOf(planA).Pointer() != reflect.ValueOf(planB).Pointer() {
		t.Error("loadLabelPlan should return the same cached map on repeat calls")
	}
}
