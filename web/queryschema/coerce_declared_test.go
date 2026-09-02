package queryschema

import (
	"reflect"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// timeSpec / idSpec mirror what walkSchemaLevel builds for a `*time.Time` and a
// `*domain.ID` filter leaf: pointer stripped, so GoKind is Struct for both and
// only GoType tells them apart.
func timeSpec() FilterSpec {
	return FilterSpec{
		DocPath: "CreatedAt", GoKind: reflect.Struct, GoType: reflect.TypeOf(time.Time{}),
	}
}

func idSpec() FilterSpec {
	return FilterSpec{
		DocPath: "TenantID", GoKind: reflect.Struct, GoType: reflect.TypeOf(domain.ID{}),
	}
}

// A malformed date must be refused for EVERY operator the leaf can declare —
// the range operators and the equality alike. Both spellings used to emit a
// clause carrying the verbatim string, which the relational compiler bound as
// text against a timestamp column and the driver answered with a 500.
func TestCoerceLeaf_TimeRefusesMalformedOnEveryOperator(t *testing.T) {
	for _, op := range []string{"", OpEq, OpNe, OpGte, OpLte, OpGt, OpLt} {
		t.Run("op="+op, func(t *testing.T) {
			f := map[string]any{}
			v := ApplyFilterValues(f, timeSpec(), "createdAt."+op, op, []string{"not-a-date"})
			if v == nil {
				t.Fatalf("malformed date must be refused, got clause %#v", f)
			}
			if v.Value != "not-a-date" {
				t.Errorf("violation must echo the offending value, got %q", v.Value)
			}
			if v.Field != "createdAt."+op {
				t.Errorf("violation must name the wire key the consumer wrote, got %q", v.Field)
			}
			if _, isType := v.Notification.(domain.InvalidFilterValueNotification); !isType {
				t.Errorf("notification = %T, want InvalidFilterValueNotification", v.Notification)
			}
			if len(f) != 0 {
				t.Errorf("a refused probe must emit no clause, got %#v", f)
			}
		})
	}
}

// The other half of the same rule: a WELL-FORMED date must reach the criteria
// as a real time.Time. It used to travel as a string, which on a Mongo view
// compared against a BSON datetime and matched nothing — a valid filter
// silently answering an empty page.
func TestCoerceLeaf_TimeEmitsTypedValue(t *testing.T) {
	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	f := map[string]any{}
	if v := ApplyFilterValues(f, timeSpec(), "createdAt", OpEq, []string{"2024-01-02T03:04:05Z"}); v != nil {
		t.Fatalf("valid RFC3339 refused: %+v", v)
	}
	got, ok := f["CreatedAt"].(time.Time)
	if !ok {
		t.Fatalf("eq clause = %#v (%T), want time.Time", f["CreatedAt"], f["CreatedAt"])
	}
	if !got.Equal(want) {
		t.Errorf("eq clause = %v, want %v", got, want)
	}

	g := map[string]any{}
	if v := ApplyFilterValues(g, timeSpec(), "createdAt.gte", OpGte, []string{"2024-01-02T03:04:05Z"}); v != nil {
		t.Fatalf("valid RFC3339 refused on gte: %+v", v)
	}
	cl, ok := g["CreatedAt"].(queries.Clause)
	if !ok || cl.Op != queries.FilterGte {
		t.Fatalf("gte clause = %#v", g["CreatedAt"])
	}
	if _, isTime := cl.Values[0].(time.Time); !isTime {
		t.Errorf("gte operand = %T, want time.Time", cl.Values[0])
	}
}

// RFC3339 is the only accepted spelling. A date-only value is a consumer error,
// not a second dialect — accepting it would invent a timezone nobody declared.
func TestCoerceLeaf_TimeRejectsDateOnly(t *testing.T) {
	f := map[string]any{}
	if v := ApplyFilterValues(f, timeSpec(), "createdAt", OpEq, []string{"2024-01-02"}); v == nil {
		t.Fatalf("date-only must be refused, got %#v", f)
	}
}

// A list operator refuses on its first unusable element and names it, so an
// `in` list of dates behaves like every other typed list.
func TestCoerceLeaf_TimeListNamesTheBadElement(t *testing.T) {
	f := map[string]any{}
	v := ApplyFilterValues(f, timeSpec(), "createdAt.in", OpIn,
		[]string{"2024-01-02T03:04:05Z", "nope"})
	if v == nil {
		t.Fatal("a list carrying a malformed date must be refused")
	}
	if v.Value != "nope" {
		t.Errorf("violation value = %q, want the offending element", v.Value)
	}
}

// domain.ID leaves are judged at the wire too. The relational compiler already
// refuses a malformed identity in core.place(), but only on a TableSchema-backed
// read — a Mongo view never reached that check and answered 200 with an empty
// page instead of naming the consumer's typo.
func TestCoerceLeaf_IdentityRefusesNonUUID(t *testing.T) {
	f := map[string]any{}
	v := ApplyFilterValues(f, idSpec(), "tenantId", OpEq, []string{"abc"})
	if v == nil {
		t.Fatalf("a non-uuid identity probe must be refused, got %#v", f)
	}
	if v.Value != "abc" {
		t.Errorf("violation value = %q", v.Value)
	}

	g := map[string]any{}
	id := "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"
	if v := ApplyFilterValues(g, idSpec(), "tenantId", OpEq, []string{id}); v != nil {
		t.Fatalf("a well-formed uuid refused: %+v", v)
	}
	// The canonical STRING, not a domain.ID: that type carries its value in an
	// unexported field and encodes to an EMPTY BSON sub-document, so emitting it
	// would make every identity filter on a Mongo view match nothing. The
	// relational compiler lifts the string into the dialect's id form itself.
	if got, ok := g["TenantID"].(string); !ok || got != id {
		t.Errorf("clause = %#v (%T), want the canonical string %s", g["TenantID"], g["TenantID"], id)
	}
}

// The empty probe is a caller asking for "no id" — a legitimate predicate, and
// the same carve-out core.malformedIDProbe makes.
func TestCoerceLeaf_IdentityAllowsEmptyProbe(t *testing.T) {
	f := map[string]any{}
	if v := ApplyFilterValues(f, idSpec(), "tenantId", OpEq, []string{""}); v != nil {
		t.Fatalf("empty identity probe must be allowed: %+v", v)
	}
}

// The guarantee that nothing legitimate was invalidated: a leaf whose type
// carries no rule keeps the pre-existing passthrough. Adding rules for the two
// types the framework knows must not turn every other declaration into a 400.
func TestCoerceLeaf_UnknownTypeStillPassesThrough(t *testing.T) {
	type custom struct{ V string }
	spec := FilterSpec{
		DocPath: "Custom", GoKind: reflect.Struct, GoType: reflect.TypeOf(custom{}),
	}
	f := map[string]any{}
	if v := ApplyFilterValues(f, spec, "custom", OpEq, []string{"whatever"}); v != nil {
		t.Fatalf("a type with no rule must keep passing through, got %+v", v)
	}
	if f["Custom"] != "whatever" {
		t.Errorf("clause = %#v, want the verbatim string", f["Custom"])
	}
}

// A FilterSpec built without a GoType — what web/grpc constructs by hand —
// must keep coercing by kind alone.
func TestCoerceLeaf_NilGoTypeFallsBackToKind(t *testing.T) {
	spec := FilterSpec{DocPath: "Age", GoKind: reflect.Int}
	f := map[string]any{}
	if v := ApplyFilterValues(f, spec, "age", OpEq, []string{"25"}); v != nil {
		t.Fatalf("nil GoType must coerce by kind: %+v", v)
	}
	if f["Age"] != int64(25) {
		t.Errorf("clause = %#v, want int64(25)", f["Age"])
	}
}

// ─── uuid.UUID — the type the path binding already judged ───────────────────

// A leaf declared uuid.UUID is judged exactly like domain.ID. Before, only the
// path segment was: web.classifyPathFieldType has always checked both, while
// the filter judged neither — the same surface answering the same type two ways.
func TestCoerceLeaf_BareUUIDTypeIsJudgedLikeIdentity(t *testing.T) {
	ut := reflect.TypeOf(uuid.UUID{})
	if _, ok := coerceLeaf("not-a-uuid", ut, ut.Kind()); ok {
		t.Error("a non-uuid on a uuid.UUID leaf must be refused")
	}
	id := "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"
	got, ok := coerceLeaf(id, ut, ut.Kind())
	if !ok {
		t.Fatal("a well-formed uuid on a uuid.UUID leaf must be accepted")
	}
	// The canonical string, for the same BSON reason domain.ID carries.
	if got != id {
		t.Errorf("emitted %#v (%T), want the canonical string", got, got)
	}
}

// ─── list leaves — every operand judged as the ELEMENT type ─────────────────

// `Codes []int64` declares a list OF int64, not a value of type []int64. Judging
// the slice itself found no rule and passed the operands through as strings, so
// ?codes.in=10,20 reached a bigint column as text — a 500 — and a Mongo numeric
// field as strings, matching nothing.
func TestCoerceLeaf_ListLeafCoercesPerElement(t *testing.T) {
	lt := reflect.TypeOf([]int64{})
	got, ok := coerceLeaf("10", lt, lt.Kind())
	if !ok {
		t.Fatal("a well-formed element must be accepted")
	}
	if got != int64(10) {
		t.Errorf("emitted %#v (%T), want int64(10) — the ELEMENT type", got, got)
	}
	if _, ok := coerceLeaf("abc", lt, lt.Kind()); ok {
		t.Error("a non-numeric element on a []int64 leaf must be refused")
	}
}

// A list leaf inherits EVERY rule, not just the primitive ones — recursing is
// what buys that for free.
func TestCoerceLeaf_ListLeafInheritsTheTypeRules(t *testing.T) {
	for _, c := range []struct {
		name string
		typ  reflect.Type
		bad  string
		good string
	}{
		{"[]time.Time", reflect.TypeOf([]time.Time{}), "not-a-date", "2024-01-02T03:04:05Z"},
		{"[]domain.ID", reflect.TypeOf([]domain.ID{}), "nope", "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"},
		{"[]*int64 (pointer element)", reflect.TypeOf([]*int64{}), "abc", "42"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := coerceLeaf(c.bad, c.typ, c.typ.Kind()); ok {
				t.Errorf("%q must be refused on a %s leaf", c.bad, c.name)
			}
			if _, ok := coerceLeaf(c.good, c.typ, c.typ.Kind()); !ok {
				t.Errorf("%q must be accepted on a %s leaf", c.good, c.name)
			}
		})
	}
}

// ─── time.Duration — the silent wrong answer, not a 500 ────────────────────

// Kind=int64, so the primitive switch ACCEPTED a bare number and meant
// NANOseconds: ?ttl=300 answered 200 with rows nobody asked for. The Go spelling
// is the contract, and a unitless number is refused.
func TestCoerceLeaf_DurationTakesTheGoSpellingOnly(t *testing.T) {
	dt := reflect.TypeOf(time.Duration(0))
	got, ok := coerceLeaf("5m", dt, dt.Kind())
	if !ok {
		t.Fatal(`"5m" must be accepted on a time.Duration leaf`)
	}
	if got != int64(5*time.Minute) {
		t.Errorf("emitted %#v, want the underlying int64 of 5m", got)
	}
	if _, ok := coerceLeaf("300", dt, dt.Kind()); ok {
		t.Error(`a unitless "300" must be refused — it used to mean 300 nanoseconds`)
	}
	if _, ok := coerceLeaf("teucu", dt, dt.Kind()); ok {
		t.Error("garbage must be refused")
	}
}
