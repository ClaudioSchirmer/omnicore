package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

func newIntrospectableRegistry(on bool) (*Registry, *configuration.AppContext) {
	h := &fakeReadHandler{}
	reg := New(pipeline.New(translation.Default())).
		Register(QueryWithParams[execRequest]("users", "User", execResponse{}.FromResult, h)).
		EnableIntrospection(on)
	return reg, configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func TestIntrospection_SchemaQueryReturnsTypes(t *testing.T) {
	reg, ctx := newIntrospectableRegistry(true)
	resp := reg.Execute(ctx, `{ __schema { queryType { name } types { name kind } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("introspection errors: %+v", resp.Errors)
	}
	sch := resp.Data["__schema"].(map[string]any)
	if sch["queryType"].(map[string]any)["name"] != "Query" {
		t.Errorf("queryType.name = %v, want Query", sch["queryType"])
	}
	want := map[string]bool{"User": false, "UserConnection": false}
	for _, tt := range sch["types"].([]any) {
		if name, _ := tt.(map[string]any)["name"].(string); name != "" {
			if _, tracked := want[name]; tracked {
				want[name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("introspection types missing %q", name)
		}
	}
}

func TestIntrospection_TypeByNameExposesFields(t *testing.T) {
	reg, ctx := newIntrospectableRegistry(true)
	resp := reg.Execute(ctx, `{ __type(name: "User") { name kind fields { name } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	tp := resp.Data["__type"].(map[string]any)
	if tp["name"] != "User" || tp["kind"] != "OBJECT" {
		t.Errorf("__type = %v, want name=User kind=OBJECT", tp)
	}
	var names []string
	for _, f := range tp["fields"].([]any) {
		names = append(names, f.(map[string]any)["name"].(string))
	}
	hasAll := func(want ...string) bool {
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		for _, w := range want {
			if !set[w] {
				return false
			}
		}
		return true
	}
	if !hasAll("id", "name", "age") {
		t.Errorf("User fields = %v, want id/name/age", names)
	}
}

// TestIntrospection_QueryTypeOmitsMetaFields guards the GraphiQL-compatibility
// fix: the introspection meta-fields (`__schema`, `__type`, `__typename`) that
// gqlparser injects onto the Query definition MUST NOT surface in the Query
// type's `fields` list. The GraphQL spec forbids `__`-prefixed names in a
// type's fields, and GraphiQL's client-side schema validation hard-rejects a
// schema that declares them ("Name __schema must not begin with __").
func TestIntrospection_QueryTypeOmitsMetaFields(t *testing.T) {
	reg, ctx := newIntrospectableRegistry(true)
	resp := reg.Execute(ctx, `{ __schema { types { name fields { name } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("introspection errors: %+v", resp.Errors)
	}
	var queryFields []string
	for _, tt := range resp.Data["__schema"].(map[string]any)["types"].([]any) {
		tm := tt.(map[string]any)
		if tm["name"] != "Query" {
			continue
		}
		for _, f := range tm["fields"].([]any) {
			queryFields = append(queryFields, f.(map[string]any)["name"].(string))
		}
	}
	if len(queryFields) == 0 {
		t.Fatal("Query type not found in introspection types")
	}
	for _, name := range queryFields {
		if len(name) >= 2 && name[:2] == "__" {
			t.Errorf("Query.fields leaks introspection meta-field %q (GraphiQL rejects __-prefixed field names)", name)
		}
	}
	// the real field must still be present
	found := false
	for _, name := range queryFields {
		if name == "users" {
			found = true
		}
	}
	if !found {
		t.Errorf("Query.fields = %v, want it to contain the real field 'users'", queryFields)
	}
}

func TestIntrospection_DisabledFallsThroughToError(t *testing.T) {
	reg, ctx := newIntrospectableRegistry(false)
	resp := reg.Execute(ctx, `{ __schema { queryType { name } } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("with introspection disabled, __schema must not resolve")
	}
}

// TestIntrospection_InputFieldDefaultValueSurfaces — __InputValue.defaultValue
// renders the declared SDL default as its GraphQL literal (spec shape GraphiQL
// and codegen read); UserOrder.direction carries `ASC`, the field itself none.
func TestIntrospection_InputFieldDefaultValueSurfaces(t *testing.T) {
	h := &fakeReadHandler{}
	reg, ctx := newExecRegistry(h)
	reg.EnableIntrospection(true)

	resp := reg.Execute(ctx, `{ __type(name: "UserOrder") { inputFields { name defaultValue } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	typ := resp.Data["__type"].(map[string]any)
	byName := map[string]any{}
	for _, f := range typ["inputFields"].([]any) {
		m := f.(map[string]any)
		byName[m["name"].(string)] = m["defaultValue"]
	}
	if got := byName["direction"]; got != "ASC" {
		t.Errorf("direction.defaultValue = %v, want ASC", got)
	}
	if got := byName["field"]; got != nil {
		t.Errorf("field.defaultValue = %v, want null (no default declared)", got)
	}
}
