package queryschema

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func i64(n int64) *int64 { return &n }

func bp(b bool) *bool { return &b }

func allReserved() map[string]bool {
	return map[string]bool{
		KeyFirst: true, KeyLast: true, KeyAfter: true, KeyBefore: true,
		KeyOrderBy: true, KeyFields: true, KeySearch: true,
		KeyIncludeArchived: true, KeyOnlyTotal: true,
	}
}

// noneReserved declares nothing: every control on the wire is undeclared.
func noneReserved() map[string]bool { return map[string]bool{} }

func TestValidateControls_CleanForward(t *testing.T) {
	v := ValidateControls(allReserved(), Controls{First: i64(10), After: true, OrderBy: true}, nil)
	if len(v) != 0 {
		t.Fatalf("clean forward page must pass, got %v", v)
	}
}

func TestValidateControls_CleanBackward(t *testing.T) {
	v := ValidateControls(allReserved(), Controls{Last: i64(5), Before: true}, nil)
	if len(v) != 0 {
		t.Fatalf("clean backward page must pass, got %v", v)
	}
}

func TestValidateControls_LastAloneIsValid(t *testing.T) {
	// `last` with no cursor = the LAST N of the set — expressible on every surface.
	if v := ValidateControls(allReserved(), Controls{Last: i64(3)}, nil); len(v) != 0 {
		t.Fatalf("last alone must pass, got %v", v)
	}
}

func TestValidateControls_OptInGate(t *testing.T) {
	// Nothing declared on the DTO: every present control is a NotDeclared
	// violation, in canonical key order.
	c := Controls{
		First: i64(10), Last: i64(2), After: true, Before: true,
		OrderBy: true, Fields: true, Search: true, IncludeArchived: true, OnlyTotal: bp(true),
	}
	v := ValidateControls(noneReserved(), c, nil)
	wantKeys := []string{
		KeyFirst, KeyLast, KeyAfter, KeyBefore, KeyOrderBy,
		KeyFields, KeySearch, KeyIncludeArchived, KeyOnlyTotal,
	}
	var gate []ControlViolation
	for _, viol := range v {
		if viol.Kind == ViolationNotDeclared {
			gate = append(gate, viol)
		}
	}
	if len(gate) != len(wantKeys) {
		t.Fatalf("want %d NotDeclared, got %d (%v)", len(wantKeys), len(gate), v)
	}
	for i, k := range wantKeys {
		if gate[i].Key != k {
			t.Fatalf("gate[%d]: want key %q, got %q", i, k, gate[i].Key)
		}
	}
}

func TestValidateControls_NaturalKeysExempt(t *testing.T) {
	// GraphQL posture: fields (selection IS the projection) and onlyTotal
	// (selection shape is the switch) carry no wire name to gate.
	natural := map[string]bool{KeyFields: true, KeyOnlyTotal: true}
	if v := ValidateControls(noneReserved(), Controls{Fields: true}, natural); len(v) != 0 {
		t.Fatalf("natural fields must be exempt from the gate, got %v", v)
	}
	if v := ValidateControls(noneReserved(), Controls{OnlyTotal: bp(true)}, natural); len(v) != 0 {
		t.Fatalf("natural onlyTotal must be exempt from the gate, got %v", v)
	}
	// The conflict matrix still applies to natural keys — a only-total request
	// shaped by a projection is contradictory on every surface.
	v := ValidateControls(noneReserved(), Controls{Fields: true, OnlyTotal: bp(true)}, natural)
	if len(v) != 1 || v[0].Kind != ViolationOnlyTotalConflict || v[0].Key != KeyFields {
		t.Fatalf("natural fields+onlyTotal must still conflict, got %v", v)
	}
}

func TestValidateControls_DirectionMix(t *testing.T) {
	cases := []struct {
		name    string
		c       Controls
		wantKey string
	}{
		{"first+last", Controls{First: i64(1), Last: i64(1)}, KeyLast},
		{"first+before", Controls{First: i64(1), Before: true}, KeyBefore},
		{"last+after", Controls{Last: i64(1), After: true}, KeyLast},
		{"after+before", Controls{After: true, Before: true}, KeyBefore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateControls(allReserved(), tc.c, nil)
			if len(v) != 1 || v[0].Kind != ViolationDirection {
				t.Fatalf("want single Direction violation, got %v", v)
			}
			if v[0].Key != tc.wantKey {
				t.Fatalf("want backward-side key %q, got %q", tc.wantKey, v[0].Key)
			}
		})
	}
}

func TestValidateControls_NonPositiveSize(t *testing.T) {
	v := ValidateControls(allReserved(), Controls{First: i64(0)}, nil)
	if len(v) != 1 || v[0].Kind != ViolationNonPositiveSize || v[0].Key != KeyFirst {
		t.Fatalf("first=0 must reject as NonPositiveSize on first, got %v", v)
	}
	v = ValidateControls(allReserved(), Controls{Last: i64(-2)}, nil)
	if len(v) != 1 || v[0].Kind != ViolationNonPositiveSize || v[0].Key != KeyLast {
		t.Fatalf("last=-2 must reject as NonPositiveSize on last, got %v", v)
	}
}

func TestValidateControls_OnlyTotalConflicts(t *testing.T) {
	c := Controls{
		OnlyTotal: bp(true),
		Fields:    true, OrderBy: true, First: i64(1), Last: i64(1), After: true, Before: true,
	}
	v := ValidateControls(allReserved(), c, nil)
	var conflicts []string
	for _, viol := range v {
		if viol.Kind == ViolationOnlyTotalConflict {
			conflicts = append(conflicts, viol.Key)
		}
	}
	want := []string{KeyFields, KeyOrderBy, KeyFirst, KeyLast, KeyAfter, KeyBefore}
	if len(conflicts) != len(want) {
		t.Fatalf("want %d conflicts, got %v", len(want), conflicts)
	}
	for i, k := range want {
		if conflicts[i] != k {
			t.Fatalf("conflict[%d]: want %q, got %q", i, k, conflicts[i])
		}
	}
}

func TestValidateControls_OnlyTotalCompatibleControls(t *testing.T) {
	// search + includeArchived bound the count — the canonical use case.
	v := ValidateControls(allReserved(), Controls{OnlyTotal: bp(true), Search: true, IncludeArchived: true}, nil)
	if len(v) != 0 {
		t.Fatalf("onlyTotal with search/includeArchived must pass, got %v", v)
	}
}

func TestValidateControls_OnlyTotalPresentButInactive(t *testing.T) {
	// &false = on the wire but not activating (REST's `?onlyTotal=false`).
	// The opt-in gate keys on PRESENCE: undeclared → NotDeclared, exactly
	// like includeArchived=false.
	v := ValidateControls(noneReserved(), Controls{OnlyTotal: bp(false)}, nil)
	if len(v) != 1 || v[0].Kind != ViolationNotDeclared || v[0].Key != KeyOnlyTotal {
		t.Fatalf("present-but-inactive onlyTotal must gate on presence, got %v", v)
	}
	// The conflict matrix keys on ACTIVATION: an inactive spelling alongside
	// page-shaping controls is a plain paged read, not a conflict.
	v = ValidateControls(allReserved(), Controls{OnlyTotal: bp(false), First: i64(5), OrderBy: true}, nil)
	if len(v) != 0 {
		t.Fatalf("inactive onlyTotal must not trip the conflict matrix, got %v", v)
	}
}

func TestControlViolation_Field(t *testing.T) {
	if f := (ControlViolation{Kind: ViolationOnlyTotalConflict, Key: KeyFirst}).Field(); f != "onlyTotal[first]" {
		t.Fatalf("conflict field: got %q", f)
	}
	if f := (ControlViolation{Kind: ViolationDirection, Key: KeyLast}).Field(); f != "last" {
		t.Fatalf("direction field: got %q", f)
	}
}

func TestControlViolation_Message(t *testing.T) {
	m := ControlViolation{Kind: ViolationNotDeclared, Key: KeyOrderBy}.Message()
	if m.ResolveFieldName() != "orderBy" {
		t.Fatalf("message field: got %q", m.ResolveFieldName())
	}
	if _, ok := m.Notification.(domain.SchemaViolationNotification); !ok {
		t.Fatalf("message notification: got %T", m.Notification)
	}
}
