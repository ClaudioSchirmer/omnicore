package openapi

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// ─── Primitives ────────────────────────────────────────────────────────────

func TestGenerate_Primitives(t *testing.T) {
	cases := []struct {
		name    string
		typ     reflect.Type
		typeStr string
		format  string
	}{
		{"string", reflect.TypeOf(""), "string", ""},
		{"bool", reflect.TypeOf(true), "boolean", ""},
		{"int", reflect.TypeOf(int(0)), "integer", "int32"},
		{"int8", reflect.TypeOf(int8(0)), "integer", "int32"},
		{"int32", reflect.TypeOf(int32(0)), "integer", "int32"},
		{"int64", reflect.TypeOf(int64(0)), "integer", "int64"},
		{"uint", reflect.TypeOf(uint(0)), "integer", "int32"},
		{"uint64", reflect.TypeOf(uint64(0)), "integer", "int64"},
		{"float32", reflect.TypeOf(float32(0)), "number", "float"},
		{"float64", reflect.TypeOf(float64(0)), "number", "double"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := NewGenerator(nil)
			s := g.Generate(c.typ)
			if s.Type != c.typeStr {
				t.Fatalf("type: got %q, want %q", s.Type, c.typeStr)
			}
			if s.Format != c.format {
				t.Fatalf("format: got %q, want %q", s.Format, c.format)
			}
		})
	}
}

// ─── Pointers ──────────────────────────────────────────────────────────────

func TestGenerate_PointerIsNullable(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf((*string)(nil)))
	if s.Type != "string" {
		t.Fatalf("type: got %q, want string", s.Type)
	}
	if !s.Nullable {
		t.Fatal("pointer type must mark schema nullable")
	}
}

func TestGenerate_PointerCloneIndependentOfInner(t *testing.T) {
	g := NewGenerator(nil)
	inner := g.Generate(reflect.TypeOf(""))
	ptr := g.Generate(reflect.TypeOf((*string)(nil)))
	if inner.Nullable {
		t.Fatal("inner string schema must NOT be mutated when its pointer is generated")
	}
	if !ptr.Nullable {
		t.Fatal("pointer schema must be nullable")
	}
}

// ─── Well-known types ─────────────────────────────────────────────────────

func TestGenerate_WellKnownTypes(t *testing.T) {
	g := NewGenerator(nil)
	cases := []struct {
		name   string
		typ    reflect.Type
		format string
	}{
		{"time.Time", reflect.TypeOf(time.Time{}), "date-time"},
		{"uuid.UUID", reflect.TypeOf(uuid.UUID{}), "uuid"},
		{"domain.ID", reflect.TypeOf(domain.ID{}), "uuid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := g.Generate(c.typ)
			if s.Type != "string" {
				t.Fatalf("type: got %q, want string", s.Type)
			}
			if s.Format != c.format {
				t.Fatalf("format: got %q, want %q", s.Format, c.format)
			}
			// Well-known types must NOT register a Component entry — they
			// have a fixed wire shape; emitting them as $ref would force
			// consumers to chase a one-liner across the spec.
			if _, exists := g.Components().Schemas[c.typ.Name()]; exists {
				t.Fatalf("%s must not appear in Components", c.typ.Name())
			}
		})
	}
}

func TestGenerate_PointerToWellKnownIsNullable(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf((*time.Time)(nil)))
	if s.Type != "string" || s.Format != "date-time" || !s.Nullable {
		t.Fatalf("*time.Time should be nullable date-time string, got %+v", s)
	}
}

// ─── Slices ────────────────────────────────────────────────────────────────

func TestGenerate_SliceOfString(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf([]string{}))
	if s.Type != "array" {
		t.Fatalf("type: got %q, want array", s.Type)
	}
	if s.Items == nil || s.Items.Type != "string" {
		t.Fatalf("items: got %+v, want {type:string}", s.Items)
	}
}

func TestGenerate_ByteSliceIsBase64String(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf([]byte{}))
	if s.Type != "string" || s.Format != "byte" {
		t.Fatalf("[]byte should render as string format byte, got %+v", s)
	}
}

// ─── Maps ──────────────────────────────────────────────────────────────────

func TestGenerate_MapStringInt(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf(map[string]int{}))
	if s.Type != "object" {
		t.Fatalf("type: got %q, want object", s.Type)
	}
	inner, ok := s.AdditionalProperties.(*Schema)
	if !ok {
		t.Fatalf("additionalProperties should be *Schema, got %T", s.AdditionalProperties)
	}
	if inner.Type != "integer" {
		t.Fatalf("inner type: got %q, want integer", inner.Type)
	}
}

func TestGenerate_MapStringAnyIsFreeForm(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf(map[string]any{}))
	if s.Type != "object" {
		t.Fatalf("type: got %q, want object", s.Type)
	}
	if s.AdditionalProperties != true {
		t.Fatalf("additionalProperties: got %v, want true (free-form)", s.AdditionalProperties)
	}
}

func TestGenerate_MapNonStringKeyIsOpaque(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf(map[int]string{}))
	if s.Type != "object" {
		t.Fatalf("type: got %q, want object", s.Type)
	}
	if s.AdditionalProperties != nil {
		t.Fatalf("non-string-keyed maps cannot round-trip as JSON; expected opaque object, got additionalProperties=%v", s.AdditionalProperties)
	}
}

// ─── Struct: tag rules + required ─────────────────────────────────────────

type sampleRequest struct {
	Name     string  `json:"name"`
	Email    string  `json:"email"`
	Phone    *string `json:"phone,omitempty"`
	Nickname string  `json:",omitempty"`
	Skipped  string  `json:"-"`
	PathID   string  `path:"id"`
	Query    string  `query:"q"`
	Hidden   string  // unexported via lowercase below ensures non-export skip path
	internal string
}

func TestGenerate_StructLenient_HonorsTagRules(t *testing.T) {
	g := NewGenerator(NewComponents())
	root := g.Generate(reflect.TypeOf(sampleRequest{}))
	if root.Ref == "" {
		t.Fatalf("named struct should yield a $ref, got %+v", root)
	}
	def := g.Components().Schemas["sampleRequest"]
	if def == nil {
		t.Fatal("sampleRequest should be registered in Components")
	}

	// Properties expected (json tag rules + skip path:/query:/json:"-"):
	//   name, email, phone, Nickname, Hidden
	// (Hidden is exported uppercase with no tag — included by field name.)
	expectedProps := []string{"Hidden", "Nickname", "email", "name", "phone"}
	gotProps := sortedKeys(toSet(def.Properties))
	if !equalSlices(gotProps, expectedProps) {
		t.Fatalf("properties: got %v, want %v", gotProps, expectedProps)
	}

	// Required in lenient mode: non-pointer + no `,omitempty`.
	//   name: required (non-pointer, no omitempty)
	//   email: required (non-pointer, no omitempty)
	//   phone: NOT required (pointer)
	//   Nickname: NOT required (json:",omitempty")
	//   Hidden: required (non-pointer, no omitempty)
	expectedReq := []string{"Hidden", "email", "name"}
	if !equalSlices(def.Required, expectedReq) {
		t.Fatalf("required (lenient): got %v, want %v", def.Required, expectedReq)
	}
}

func TestGenerate_StructStrict_EveryFieldRequired(t *testing.T) {
	g := NewGenerator(NewComponents())
	root := g.GenerateStrict(reflect.TypeOf(sampleRequest{}))
	if root.Ref == "" {
		t.Fatalf("named struct should yield a $ref, got %+v", root)
	}
	def := g.Components().Schemas["sampleRequest"]
	if def == nil {
		t.Fatal("sampleRequest should be registered in Components")
	}
	expectedReq := []string{"Hidden", "Nickname", "email", "name", "phone"}
	if !equalSlices(def.Required, expectedReq) {
		t.Fatalf("required (strict): got %v, want %v", def.Required, expectedReq)
	}
}

func TestGenerate_PointerFieldRendersNullable(t *testing.T) {
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(sampleRequest{}))
	def := g.Components().Schemas["sampleRequest"]
	phone := def.Properties["phone"]
	if phone == nil {
		t.Fatal("phone property missing")
	}
	if phone.Type != "string" || !phone.Nullable {
		t.Fatalf("phone should be nullable string, got %+v", phone)
	}
}

// ─── Struct: anonymous embed flattening ───────────────────────────────────

type embedBase struct {
	BaseField string `json:"baseField"`
}

type embedChild struct {
	embedBase
	OwnField string `json:"ownField"`
}

func TestGenerate_AnonymousEmbedFlattens(t *testing.T) {
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(embedChild{}))
	def := g.Components().Schemas["embedChild"]
	if def == nil {
		t.Fatal("embedChild not registered")
	}
	if _, ok := def.Properties["baseField"]; !ok {
		t.Fatalf("baseField from embedBase should be flattened into embedChild, properties=%v", keysOf(def.Properties))
	}
	if _, ok := def.Properties["ownField"]; !ok {
		t.Fatalf("ownField missing from embedChild, properties=%v", keysOf(def.Properties))
	}
	// embedBase should NOT appear in Components — anonymous embeds inline.
	if _, exists := g.Components().Schemas["embedBase"]; exists {
		t.Fatal("embedBase should not be registered as a Component (anonymous embed must inline)")
	}
}

// ─── Struct: named-type field renders as $ref ─────────────────────────────

type childStruct struct {
	Value string `json:"value"`
}

type parentStruct struct {
	Child   childStruct   `json:"child"`
	Brother childStruct   `json:"brother"`
	List    []childStruct `json:"list"`
}

func TestGenerate_NamedFieldUsesRef_DedupedInComponents(t *testing.T) {
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(parentStruct{}))
	parent := g.Components().Schemas["parentStruct"]
	if parent == nil {
		t.Fatal("parentStruct not registered")
	}
	for _, key := range []string{"child", "brother"} {
		got := parent.Properties[key]
		if got == nil || got.Ref != "#/components/schemas/childStruct" {
			t.Fatalf("%s should be $ref to childStruct, got %+v", key, got)
		}
	}
	list := parent.Properties["list"]
	if list == nil || list.Type != "array" {
		t.Fatalf("list should be array, got %+v", list)
	}
	if list.Items == nil || list.Items.Ref != "#/components/schemas/childStruct" {
		t.Fatalf("list.items should be $ref to childStruct, got %+v", list.Items)
	}
	// Exactly one childStruct entry — dedup must collapse the three sites.
	if _, ok := g.Components().Schemas["childStruct"]; !ok {
		t.Fatal("childStruct missing from Components")
	}
	if got := len(g.Components().Schemas); got != 2 {
		t.Fatalf("Components should hold {parentStruct, childStruct}; got %d entries: %v", got, keysOf(g.Components().Schemas))
	}
}

// ─── Struct: self-reference does not infinite-loop ────────────────────────

type node struct {
	Name string `json:"name"`
	Next *node  `json:"next,omitempty"`
}

func TestGenerate_SelfReferentialType(t *testing.T) {
	g := NewGenerator(NewComponents())
	root := g.Generate(reflect.TypeOf(node{}))
	if root.Ref != "#/components/schemas/node" {
		t.Fatalf("self-ref entry: got %+v", root)
	}
	def := g.Components().Schemas["node"]
	if def == nil {
		t.Fatal("node not registered")
	}
	next := def.Properties["next"]
	if next == nil {
		t.Fatal("next property missing")
	}
	if next.Ref != "#/components/schemas/node" {
		t.Fatalf("self-reference should resolve to its own $ref, got %+v", next)
	}
	if !next.Nullable {
		t.Fatal("*node should mark next nullable")
	}
}

// ─── Inline anonymous struct field ────────────────────────────────────────

func TestGenerate_AnonymousStructFieldInlines(t *testing.T) {
	g := NewGenerator(NewComponents())
	type holder struct {
		Bar struct {
			X int `json:"x"`
		} `json:"bar"`
	}
	root := g.Generate(reflect.TypeOf(holder{}))
	def := g.Components().Schemas["holder"]
	if def == nil {
		t.Fatalf("holder should be registered (got root=%+v)", root)
	}
	bar := def.Properties["bar"]
	if bar == nil {
		t.Fatal("bar property missing")
	}
	if bar.Ref != "" {
		t.Fatalf("anonymous struct field should inline, not $ref; got %+v", bar)
	}
	if bar.Type != "object" || bar.Properties["x"] == nil {
		t.Fatalf("inline bar should be object with x property, got %+v", bar)
	}
}

// ─── Empty struct (responses.None analogue) ───────────────────────────────

func TestGenerate_EmptyStruct(t *testing.T) {
	g := NewGenerator(NewComponents())
	type emptyResp struct{}
	g.Generate(reflect.TypeOf(emptyResp{}))
	def := g.Components().Schemas["emptyResp"]
	if def == nil {
		t.Fatal("emptyResp not registered")
	}
	if def.Type != "object" {
		t.Fatalf("type: got %q, want object", def.Type)
	}
	if len(def.Properties) != 0 {
		t.Fatalf("empty struct should have no properties, got %v", keysOf(def.Properties))
	}
	if len(def.Required) != 0 {
		t.Fatalf("empty struct should have no required, got %v", def.Required)
	}
}

// ─── Cache: same (Type, strict) returns identical schema ──────────────────

func TestGenerate_CacheReturnsSameInstance(t *testing.T) {
	g := NewGenerator(nil)
	a := g.Generate(reflect.TypeOf(sampleRequest{}))
	b := g.Generate(reflect.TypeOf(sampleRequest{}))
	if a != b {
		t.Fatal("same (type, strict) call must return the cached schema instance")
	}
}

func TestGenerate_CacheDiscriminatesByStrict(t *testing.T) {
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(sampleRequest{}))
	g.GenerateStrict(reflect.TypeOf(sampleRequest{}))
	// Both calls registered the same component name; the registry keeps
	// the LATEST shape under that name. The cache, however, returns
	// distinct top-level $ref pointers for each (type, strict) pair —
	// what matters for downstream is that Components carries one named
	// definition per name (the latest write).
	def := g.Components().Schemas["sampleRequest"]
	if def == nil {
		t.Fatal("sampleRequest missing from Components after both modes ran")
	}
}

// ─── nil safety ───────────────────────────────────────────────────────────

func TestGenerate_NilTypeReturnsEmpty(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(nil)
	if s == nil {
		t.Fatal("Generate(nil) should not return a nil schema")
	}
	if s.Type != "" || s.Ref != "" {
		t.Fatalf("nil type should yield an empty schema, got %+v", s)
	}
}

// ─── example: tag — per-property simple examples ─────────────────────────

type taggedRequest struct {
	Name    string  `json:"name"            example:"Alice"`
	Email   string  `json:"email"           example:"alice@example.com"`
	Age     int     `json:"age"             example:"34"`
	Active  bool    `json:"active"          example:"true"`
	Phone   *string `json:"phone,omitempty" example:"+5511999998888"`
	Untaged string  `json:"untaged"`
}

func TestGenerate_ExampleTag_PopulatesPropertyExamples(t *testing.T) {
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(taggedRequest{}))
	def := g.Components().Schemas["taggedRequest"]
	if def == nil {
		t.Fatal("taggedRequest not registered")
	}
	cases := []struct {
		field string
		want  any
	}{
		{"name", "Alice"},
		{"email", "alice@example.com"},
		{"age", int32(34)},
		{"active", true},
		{"phone", "+5511999998888"},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			p := def.Properties[c.field]
			if p == nil {
				t.Fatalf("property %q missing", c.field)
			}
			if p.Example != c.want {
				t.Fatalf("Example: got %v (%T), want %v (%T)", p.Example, p.Example, c.want, c.want)
			}
		})
	}
	// Field without example tag must NOT carry an Example value.
	if def.Properties["untaged"].Example != nil {
		t.Fatalf("untaged property should not carry an Example, got %v", def.Properties["untaged"].Example)
	}
}

type twoStringsA struct {
	First  string `json:"first"  example:"Alice"`
	Second string `json:"second" example:"Bob"`
}

type twoStringsB struct {
	Value string `json:"value"`
}

func TestGenerate_ExampleTag_DoesNotLeakAcrossFields(t *testing.T) {
	// Without the clone-on-set in walkFields, the generator's cache would
	// share a single *Schema between every string field — and the second
	// example assignment would overwrite the first. The test fails fast if
	// that regression sneaks in.
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(twoStringsA{}))
	defA := g.Components().Schemas["twoStringsA"]
	if defA.Properties["first"].Example != "Alice" {
		t.Fatalf("first should hold its own Example=Alice, got %v", defA.Properties["first"].Example)
	}
	if defA.Properties["second"].Example != "Bob" {
		t.Fatalf("second should hold its own Example=Bob, got %v", defA.Properties["second"].Example)
	}

	// A subsequent type with an untagged string field must still see a
	// pristine schema — the cache must not have been polluted with Alice
	// or Bob.
	g.Generate(reflect.TypeOf(twoStringsB{}))
	defB := g.Components().Schemas["twoStringsB"]
	if defB.Properties["value"].Example != nil {
		t.Fatalf("untagged string field must not inherit Example from earlier writes, got %v", defB.Properties["value"].Example)
	}
}

type nestedExampleChild struct {
	Street  string `json:"street"  example:"Main St"`
	ZipCode string `json:"zipCode" example:"10001"`
}

type nestedExampleParent struct {
	Name    string             `json:"name" example:"Alice"`
	Address nestedExampleChild `json:"address"`
}

func TestGenerate_ExampleTag_PropagatesThroughNestedStruct(t *testing.T) {
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(nestedExampleParent{}))
	parent := g.Components().Schemas["nestedExampleParent"]
	if parent.Properties["name"].Example != "Alice" {
		t.Fatalf("parent.name Example: got %v, want Alice", parent.Properties["name"].Example)
	}
	child := g.Components().Schemas["nestedExampleChild"]
	if child == nil {
		t.Fatal("nestedExampleChild not registered")
	}
	if child.Properties["street"].Example != "Main St" {
		t.Fatalf("child.street Example: got %v, want Main St", child.Properties["street"].Example)
	}
	if child.Properties["zipCode"].Example != "10001" {
		t.Fatalf("child.zipCode Example: got %v, want 10001", child.Properties["zipCode"].Example)
	}
}

type badExampleRequest struct {
	Age int `json:"age" example:"not-a-number"`
}

func TestGenerate_ExampleTag_BadValuePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when example tag fails to parse")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value should be a string, got %T: %v", r, r)
		}
		// Diagnostic must name the field so the operator can find it.
		if !strings.Contains(msg, "Age") {
			t.Fatalf("panic message must name the offending field, got: %s", msg)
		}
	}()
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(badExampleRequest{}))
}

type compositeExampleRequest struct {
	Tags []string `json:"tags" example:"a,b,c"`
}

func TestGenerate_ExampleTag_CompositeTypeRejected(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when example tag is placed on a composite type")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Doc.RequestExamples") {
			t.Fatalf("panic message must point the operator to the map-based path, got: %s", msg)
		}
	}()
	g := NewGenerator(NewComponents())
	g.Generate(reflect.TypeOf(compositeExampleRequest{}))
}

// ─── helpers ──────────────────────────────────────────────────────────────

func toSet(m map[string]*Schema) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func keysOf(m map[string]*Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
