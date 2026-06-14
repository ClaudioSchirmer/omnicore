package openapi

import (
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// specPartialRequest declares the full partial-match allowlist so the spec
// generator must expand every operator into its own query parameter.
type specPartialRequest struct {
	Name *string `query:"name" filter:"eq,startswith,contains,ieq,ine,iin,inin,istartswith,icontains"`
}

// specNestedRequest exercises the nested embed group: every leaf inside
// AddrFilter surfaces in the spec with the parent's query prefix.
type specNestedRequest struct {
	Name      *string          `query:"name"  filter:"eq,startswith"`
	Addresses specAddrFilter   `query:"addresses"`
	Meta      *specMetaFilter  `query:"meta"`
	Limit     *int64           `query:"limit"`
}

type specAddrFilter struct {
	City    *string `query:"city"    filter:"eq,istartswith"`
	ZipCode *string `query:"zipCode" filter:"eq,startswith"`
}

type specMetaFilter struct {
	Tag *string `query:"tag" filter:"eq"`
}

func TestSpec_PartialOperatorsExpandToParameters(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specPartialRequest{}),
			ResponseType:  reflect.TypeOf(specListItem{}),
			SuccessStatus: fiber.StatusOK,
		},
		Doc{Summary: "List users"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	get := paths["/users"].(map[string]any)["get"].(map[string]any)
	params := get["parameters"].([]any)

	names := map[string]bool{}
	for _, p := range params {
		entry := p.(map[string]any)
		names[entry["name"].(string)] = true
	}

	// eq stays bare; every other operator gets the qkey + "." + op suffix.
	expected := []string{
		"name",
		"name.startswith",
		"name.contains",
		"name.ieq",
		"name.ine",
		"name.iin",
		"name.inin",
		"name.istartswith",
		"name.icontains",
	}
	for _, want := range expected {
		if !names[want] {
			t.Errorf("query parameter %q missing; got %v", want, names)
		}
	}
}

func TestSpec_NestedEmbedGroupExpandsToDottedParameters(t *testing.T) {
	reg := NewRegistry()
	Mount(reg, fiber.New(), fiber.MethodGet, "/users",
		func(c *fiber.Ctx) error { return nil },
		RouteSpec{
			RequestType:   reflect.TypeOf(specNestedRequest{}),
			ResponseType:  reflect.TypeOf(specListItem{}),
			SuccessStatus: fiber.StatusOK,
		},
		Doc{Summary: "List users with nested filters"})

	out := marshalSpec(t, NewSpec(Config{Title: "T", Version: "1"}, reg))
	paths := out["paths"].(map[string]any)
	get := paths["/users"].(map[string]any)["get"].(map[string]any)
	params := get["parameters"].([]any)

	names := map[string]bool{}
	for _, p := range params {
		entry := p.(map[string]any)
		names[entry["name"].(string)] = true
	}

	// Top-level + nested leaves with every declared operator expansion.
	expected := []string{
		"name",
		"name.startswith",
		"addresses.city",
		"addresses.city.istartswith",
		"addresses.zipCode",
		"addresses.zipCode.startswith",
		"meta.tag",
		"limit",
	}
	for _, want := range expected {
		if !names[want] {
			t.Errorf("query parameter %q missing; got %v", want, names)
		}
	}
	// Reverse check: the embed group's `query:"addresses"` itself MUST NOT
	// appear as a parameter (it is a prefix, not a leaf).
	if names["addresses"] {
		t.Errorf("embed group prefix should NOT surface as its own parameter; got %v", names)
	}
}
