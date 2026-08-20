package graphql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/google/uuid"
)

// ── put: first writer wins ───────────────────────────────────────────────────

func TestPut_DuplicateNameIgnored(t *testing.T) {
	b := newSDLBuilder()
	b.put("Dup", "type Dup { a: Int }")
	b.put("Dup", "type Dup { b: String }")
	if got := b.defs["Dup"]; got != "type Dup { a: Int }" {
		t.Errorf("second put must be ignored, defs[Dup] = %q", got)
	}
	if len(b.order) != 1 {
		t.Errorf("duplicate put must not extend order, got %v", b.order)
	}
}

// ── scalarName: every scalar branch ──────────────────────────────────────────

func TestScalarName_WellKnownAndKindBranches(t *testing.T) {
	b := newSDLBuilder()
	cases := []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeOf(uuid.UUID{}), "ID"},
		{reflect.TypeOf(domain.ID{}), "ID"},
		{reflect.TypeOf(true), "Boolean"},
		{reflect.TypeOf(float32(0)), "Float"},
		{reflect.TypeOf(float64(0)), "Float"},
		{reflect.TypeOf(""), "String"},
		{reflect.TypeOf(int64(0)), "Int"},
		{reflect.TypeOf(struct{ X int }{}), ""}, // not a leaf scalar
		{reflect.TypeOf([]string{}), ""},
	}
	for _, c := range cases {
		if got := b.scalarName(c.typ); got != c.want {
			t.Errorf("scalarName(%v) = %q, want %q", c.typ, got, c.want)
		}
	}
}

// ── typeRef: slice-of-pointer, []byte, map fallback, array ───────────────────

type covRefNode struct {
	Label string `json:"label"`
}

func TestTypeRef_KindBranches(t *testing.T) {
	b := newSDLBuilder()
	if got := b.typeRef(reflect.TypeOf([]*covRefNode{})); got != "[covRefNode]" {
		t.Errorf("slice of *struct = %q, want [covRefNode]", got)
	}
	if got := b.typeRef(reflect.TypeOf([]byte{})); got != "String" {
		t.Errorf("[]byte = %q, want String (base64 wire)", got)
	}
	if got := b.typeRef(reflect.TypeOf(map[string]string{})); got != "String" {
		t.Errorf("map = %q, want String fallback", got)
	}
	if got := b.typeRef(reflect.TypeOf([2]int{})); got != "[Int]" {
		t.Errorf("array = %q, want [Int]", got)
	}
}

// ── objectTypeAs: pointer deref, dedup by Go type, self-recursion break ──────

type covSelfNode struct {
	Name string       `json:"name"`
	Next *covSelfNode `json:"next,omitempty"`
}

func TestObjectTypeAs_PointerDerefAndDedup(t *testing.T) {
	b := newSDLBuilder()
	first := b.objectTypeAs("First", reflect.TypeOf(&covRefNode{}))
	if first != "First" {
		t.Fatalf("objectTypeAs = %q, want First", first)
	}
	// Same Go type again under another name → the existing name wins.
	if again := b.objectTypeAs("Second", reflect.TypeOf(covRefNode{})); again != "First" {
		t.Errorf("re-registering the same Go type must return the first name, got %q", again)
	}
	if _, ok := b.defs["Second"]; ok {
		t.Error("a duplicate type must not emit a second definition")
	}
}

func TestObjectTypeAs_SelfReferentialTypeTerminates(t *testing.T) {
	b := newSDLBuilder()
	name := b.objectTypeAs("SelfNode", reflect.TypeOf(covSelfNode{}))
	def := b.defs[name]
	if !strings.Contains(def, "next: SelfNode") {
		t.Errorf("self reference must resolve to the reserved name, def:\n%s", def)
	}
}

// ── whereInput: no filter leaves → no where argument ─────────────────────────

type covNoFilterRequest struct {
	First           *int64  `query:"first"`
	Last            *int64  `query:"last"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
}

func TestWhereInput_NoFilterLeavesOmitted(t *testing.T) {
	b := newSDLBuilder()
	name, ok := b.whereInput("Thing", reflect.TypeOf(covNoFilterRequest{}))
	if ok || name != "" {
		t.Errorf("no filter leaves must yield (\"\", false), got (%q, %v)", name, ok)
	}
}

// ── operatorInput: non-scalar leaf degrades to String ────────────────────────

func TestOperatorInput_NonScalarLeafFallsBackToString(t *testing.T) {
	b := newSDLBuilder()
	leaf := queryschema.RequestLeaf{
		WirePath: "tags",
		Field:    reflect.StructField{Name: "Tags", Type: reflect.TypeOf([]string{})},
		Ops:      []string{queryschema.OpEq, queryschema.OpIn},
	}
	name := b.operatorInput("Thing", leaf)
	def := b.defs[name]
	if !strings.Contains(def, "eq: String\n") {
		t.Errorf("non-scalar leaf must degrade eq to String, def:\n%s", def)
	}
	if !strings.Contains(def, "in: [String!]\n") {
		t.Errorf("list operator must be a String list, def:\n%s", def)
	}
}

// ── exportedJSONFields: skips + embed promotion ──────────────────────────────

type covEmbedBase struct {
	Base string `json:"base"`
}

type covPtrEmbedBase struct {
	PtrBase string `json:"ptrBase"`
}

type covRichResponse struct {
	covEmbedBase
	*covPtrEmbedBase
	hidden  string //nolint:unused // exercises the unexported skip
	Skipped string `json:"-"`
	Name    string `json:"name"`
	NoTag   string
}

func TestExportedJSONFields_SkipsAndPromotesEmbeds(t *testing.T) {
	fields := exportedJSONFields(reflect.TypeOf(&covRichResponse{})) // pointer deref
	wires := make([]string, 0, len(fields))
	for _, f := range fields {
		wires = append(wires, f.wire)
	}
	want := []string{"base", "ptrBase", "name", "NoTag"}
	if !reflect.DeepEqual(wires, want) {
		t.Errorf("wires = %v, want %v (embeds promoted, unexported/json:\"-\" skipped)", wires, want)
	}
}

func TestExportedJSONFields_NonStructIsEmpty(t *testing.T) {
	if got := exportedJSONFields(reflect.TypeOf("x")); len(got) != 0 {
		t.Errorf("non-struct must yield no fields, got %v", got)
	}
}

// ── graphqlName: anonymous type gets a stable synthetic name ─────────────────

func TestGraphqlName_AnonymousType(t *testing.T) {
	name := graphqlName(reflect.TypeOf(struct{ X int }{}))
	if !strings.HasPrefix(name, "Anon") {
		t.Errorf("anonymous type name = %q, want Anon prefix", name)
	}
	if name != sanitize(name) {
		t.Errorf("synthetic name must be GraphQL-legal, got %q", name)
	}
}
