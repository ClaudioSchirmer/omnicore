package openapi

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ─── raw_spec.go: PathParam convenience ──────────────────────────────────────

func TestPathParam(t *testing.T) {
	p := PathParam("id", "Resource identifier")
	if p.In != InPath {
		t.Errorf("In = %q, want path", p.In)
	}
	if p.Name != "id" || p.Description != "Resource identifier" {
		t.Errorf("unexpected fields: %+v", p)
	}
	if !p.Required {
		t.Error("path parameter must be Required")
	}
	if p.Type != nil {
		t.Errorf("PathParam Type should be nil (defaults to string), got %v", p.Type)
	}
}

// ─── spec_params.go: paramSchema / queryEntry / rawParameters / hasBodyFields ─

func TestParamSchema_NilDefaultsToString(t *testing.T) {
	gen := NewGenerator(nil)
	got := paramSchema(nil, gen)
	m, ok := got.(map[string]any)
	if !ok || m["type"] != "string" {
		t.Fatalf("nil Type must default to {type:string}, got %#v", got)
	}
}

func TestParamSchema_TypedRunsGenerator(t *testing.T) {
	gen := NewGenerator(nil)
	got := paramSchema(reflect.TypeOf(int64(0)), gen)
	s, ok := got.(*Schema)
	if !ok {
		t.Fatalf("typed param should produce a *Schema, got %T", got)
	}
	if s.Type != "integer" {
		t.Fatalf("int64 param schema type = %q, want integer", s.Type)
	}
}

func TestQueryEntry_RequiredOnlyForNonPointer(t *testing.T) {
	type holder struct {
		Req string  `query:"req" description:"a required key"`
		Opt *string `query:"opt"`
	}
	tp := reflect.TypeOf(holder{})
	gen := NewGenerator(nil)

	reqField := tp.Field(0)
	e := queryEntry("req", gen.Generate(reqField.Type), reqField)
	if e["in"] != "query" || e["name"] != "req" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e["required"] != true {
		t.Errorf("non-pointer query field must be required: %+v", e)
	}
	if e["description"] != "a required key" {
		t.Errorf("description tag not propagated: %+v", e)
	}

	optField := tp.Field(1)
	o := queryEntry("opt", gen.Generate(optField.Type), optField)
	if _, present := o["required"]; present {
		t.Errorf("pointer query field must be optional (no required key): %+v", o)
	}
}

func TestRawParameters(t *testing.T) {
	gen := NewGenerator(nil)
	op := Operation{
		Raw: &RawSpec{
			Parameters: []Parameter{
				{In: InPath, Name: "id", Required: true, Description: "the id"},
				{In: InQuery, Name: "limit", Type: reflect.TypeOf(int64(0))},
			},
		},
	}
	out := rawParameters(op, gen)
	if len(out) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(out))
	}
	if out[0]["in"] != "path" || out[0]["name"] != "id" || out[0]["required"] != true {
		t.Fatalf("path param wrong: %+v", out[0])
	}
	if out[0]["description"] != "the id" {
		t.Fatalf("description not carried: %+v", out[0])
	}
	// path param with nil Type defaults to {type:string}
	if m, _ := out[0]["schema"].(map[string]any); m["type"] != "string" {
		t.Fatalf("nil-type param schema = %#v", out[0]["schema"])
	}
	// query param without Required must omit the flag and carry a typed schema
	if _, present := out[1]["required"]; present {
		t.Fatalf("non-required query param must omit required: %+v", out[1])
	}
	if s, _ := out[1]["schema"].(*Schema); s == nil || s.Type != "integer" {
		t.Fatalf("typed query schema = %#v", out[1]["schema"])
	}
}

func TestHasBodyFields(t *testing.T) {
	type embedded struct {
		Inner string `json:"inner"`
	}
	cases := []struct {
		name string
		typ  reflect.Type
		want bool
	}{
		{"nil", nil, false},
		{"non-struct", reflect.TypeOf(42), false},
		{"only-path-and-query", reflect.TypeOf(struct {
			ID   string `path:"id"`
			Name string `query:"name"`
			Skip string `json:"-"`
		}{}), false},
		{"has-a-body-field", reflect.TypeOf(struct {
			ID   string `path:"id"`
			Body string `json:"body"`
		}{}), true},
		{"pointer-struct", reflect.TypeOf(&struct {
			Body string `json:"body"`
		}{}), true},
		{"unexported-only", reflect.TypeOf(struct {
			hidden string
		}{}), false},
		{"embedded-with-body", reflect.TypeOf(struct {
			embedded
		}{}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasBodyFields(c.typ); got != c.want {
				t.Errorf("hasBodyFields(%v) = %v, want %v", c.typ, got, c.want)
			}
		})
	}
}

// ─── example_validate.go: renderExample ──────────────────────────────────────

func TestRenderExample(t *testing.T) {
	// value-only: only "value" key emitted (summary/description omitted)
	out := renderExample(rawExample{Raw: json.RawMessage(`{"a":1}`)})
	if _, ok := out["summary"]; ok {
		t.Errorf("empty summary should be omitted: %+v", out)
	}
	if _, ok := out["description"]; ok {
		t.Errorf("empty description should be omitted: %+v", out)
	}
	val, ok := out["value"].(map[string]any)
	if !ok || val["a"].(float64) != 1 {
		t.Fatalf("value should decode to native JSON, got %#v", out["value"])
	}

	// full: summary + description present, no raw → no value key
	full := renderExample(rawExample{Summary: "s", Description: "d"})
	if full["summary"] != "s" || full["description"] != "d" {
		t.Fatalf("summary/description not carried: %+v", full)
	}
	if _, ok := full["value"]; ok {
		t.Errorf("nil Raw must omit the value key: %+v", full)
	}
}

// ─── spec.go: paginationInfoExample / hasPathInParameters / ensurePaginationInfo ─

func TestPaginationInfoExample(t *testing.T) {
	ex := paginationInfoExample()
	if ex["hasNextPage"] != false || ex["hasPreviousPage"] != false || ex["totalCount"] != 1 {
		t.Fatalf("unexpected pagination example: %+v", ex)
	}
}

func TestHasPathInParameters(t *testing.T) {
	if hasPathInParameters([]Parameter{{In: InQuery, Name: "q"}}) {
		t.Error("query-only params must report no path param")
	}
	if !hasPathInParameters([]Parameter{{In: InQuery}, {In: InPath, Name: "id"}}) {
		t.Error("a path param must be detected")
	}
	if hasPathInParameters(nil) {
		t.Error("nil params must report no path param")
	}
}

func TestEnsurePaginationInfo(t *testing.T) {
	c := NewComponents()
	ensurePaginationInfo(c)
	s, ok := c.Schemas["PaginationInfo"]
	if !ok {
		t.Fatal("PaginationInfo schema not registered")
	}
	if s.Type != "object" {
		t.Fatalf("PaginationInfo type = %q", s.Type)
	}
	for _, k := range []string{"hasNextPage", "hasPreviousPage", "endCursor", "startCursor", "totalCount"} {
		if _, ok := s.Properties[k]; !ok {
			t.Errorf("missing property %q", k)
		}
	}
	// idempotent — second call must not panic or replace
	ensurePaginationInfo(c)
	if c.Schemas["PaginationInfo"] != s {
		t.Error("ensurePaginationInfo must be idempotent (same instance)")
	}
}

// ─── spec.go: MarshalJSON via json.Marshal ───────────────────────────────────

func TestSpec_MarshalJSON(t *testing.T) {
	spec := NewSpec(Config{Title: "T", Version: "1.0.0"}, NewRegistry())
	raw, err := json.Marshal(spec) // invokes (*Spec).MarshalJSON
	if err != nil {
		t.Fatalf("json.Marshal(spec): %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", doc["openapi"])
	}
}
