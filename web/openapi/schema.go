// Package openapi assembles an OpenAPI 3.1.0 document from the same Go
// types the framework's HTTP wrappers already consume. The package is the
// documentation layer: it never alters runtime behavior, and a service
// that does not enable it pays nothing.
//
// This file ships the schema generator — the foundation phase. It walks a
// Go type via reflection and produces an in-memory Schema that downstream
// phases will marshal as part of /openapi.json. Wrapper integration,
// route registry, bootstrap wiring and the Swagger UI bundle land in
// subsequent rounds, on top of this generator.
package openapi

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// Schema models the subset of the OpenAPI 3.1.0 schema vocabulary the
// generator emits via reflection. It is the in-memory representation that
// the spec-assembly phase later marshals to map[string]any for
// /openapi.json. New keywords are added here as the generator gains
// coverage.
type Schema struct {
	Type                 string             `json:"type,omitempty"`
	Format               string             `json:"format,omitempty"`
	Nullable             bool               `json:"nullable,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	AdditionalProperties any                `json:"additionalProperties,omitempty"`
	Ref                  string             `json:"$ref,omitempty"`
	Description          string             `json:"description,omitempty"`
	Example              any                `json:"example,omitempty"`
}

// Components is the dedup registry that owns the named-struct schemas the
// generator encountered while walking a type graph. A named struct emits a
// `$ref: #/components/schemas/<Name>` reference on the parent property and
// a single inlined definition under Schemas — repeated references to the
// same type cost one entry, not N. Anonymous embedded structs are flattened
// inline, not referenced, so the Components map only carries types with a
// real Go name.
type Components struct {
	Schemas map[string]*Schema
}

// NewComponents constructs an empty registry. A Generator that produces
// schemas across multiple top-level types accumulates onto the same
// Components, so the final /openapi.json document carries one dedup pool
// per service.
func NewComponents() *Components {
	return &Components{Schemas: map[string]*Schema{}}
}

// Generator walks Go types via reflection and produces OpenAPI Schemas,
// memoizing each (reflect.Type, strict) pair so a service with N routes
// referencing the same DTO pays the inspection once. Components is shared:
// every Generate / GenerateStrict call enriches the same dedup pool the
// consumer passed to NewGenerator.
//
// strict toggles the required-field rule documented on Generate /
// GenerateStrict — kept on the call signature, not on Generator state, so
// the same instance can produce both shapes for the same DTO without a
// reset.
type Generator struct {
	cache      sync.Map // map[cacheKey]*Schema
	components *Components
}

type cacheKey struct {
	t      reflect.Type
	strict bool
}

// NewGenerator constructs a Generator backed by the given Components. Pass
// the same Components instance to multiple Generators (or chain Generate /
// GenerateStrict calls on the same Generator) when assembling a single
// OpenAPI document — every schema lands in the same dedup pool.
func NewGenerator(c *Components) *Generator {
	if c == nil {
		c = NewComponents()
	}
	return &Generator{components: c}
}

// Components returns the registry the Generator writes to. Callers consult
// it after their Generate calls to assemble the spec's components/schemas
// block.
func (g *Generator) Components() *Components { return g.components }

// Generate returns the lenient-mode schema for t: pointer fields are
// nullable + optional, non-pointer exported fields without `,omitempty`
// are required. Use it for Response DTOs and for Request DTOs of lenient
// handlers (PATCH, anything without the FullBody marker).
func (g *Generator) Generate(t reflect.Type) *Schema {
	return g.generate(t, false)
}

// GenerateStrict returns the strict-mode schema for t: every kept field is
// required regardless of pointer-ness or `,omitempty`. Use it for Request
// DTOs of handlers that embed pipeline.FullBody (the canonical PUT).
// Matches the runtime check the wrapper performs in
// handle_command_with_body.go via reflectExpectedJSONKeys.
func (g *Generator) GenerateStrict(t reflect.Type) *Schema {
	return g.generate(t, true)
}

// generate is the cached entry that drives the actual walk. Cache key is
// (type, strict) so the same DTO can yield both shapes for a service that
// uses it on a lenient route and a strict route simultaneously.
func (g *Generator) generate(t reflect.Type, strict bool) *Schema {
	if t == nil {
		return &Schema{}
	}
	key := cacheKey{t: t, strict: strict}
	if cached, ok := g.cache.Load(key); ok {
		return cached.(*Schema)
	}
	s := g.build(t, strict)
	g.cache.Store(key, s)
	return s
}

// wellKnown maps Go types whose JSON wire shape is fixed and cannot be
// inferred by walking exported fields (struct with unexported state and
// custom MarshalJSON) to their canonical OpenAPI schema. The registry is
// closed in this phase; consumer-defined value objects with custom
// MarshalJSON produce an incorrect schema today. The planned extension
// point (RegisterWellKnownType) ships alongside Mount in a later phase.
var wellKnown = map[reflect.Type]*Schema{
	reflect.TypeOf(time.Time{}): {Type: "string", Format: "date-time"},
	reflect.TypeOf(uuid.UUID{}): {Type: "string", Format: "uuid"},
	reflect.TypeOf(domain.ID{}): {Type: "string", Format: "uuid"},
}

// build dispatches on Kind without going through the cache — generate
// owns the cache. Pointer types unwrap to the inner type and mark the
// returned schema nullable; the inner schema is reused so a *T and T
// share the same $ref or inlined entry across different fields.
func (g *Generator) build(t reflect.Type, strict bool) *Schema {
	if t.Kind() == reflect.Pointer {
		inner := g.generate(t.Elem(), strict)
		// Clone so the nullable bit only applies to this position, not to
		// every other field referencing the same inner type.
		clone := *inner
		clone.Nullable = true
		return &clone
	}
	if s, ok := wellKnown[t]; ok {
		clone := *s
		return &clone
	}
	switch t.Kind() {
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return &Schema{Type: "integer", Format: "int32"}
	case reflect.Int64:
		return &Schema{Type: "integer", Format: "int64"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return &Schema{Type: "integer", Format: "int32"}
	case reflect.Uint64:
		return &Schema{Type: "integer", Format: "int64"}
	case reflect.Float32:
		return &Schema{Type: "number", Format: "float"}
	case reflect.Float64:
		return &Schema{Type: "number", Format: "double"}
	case reflect.Slice:
		// []byte round-trips as a base64 string per encoding/json — render
		// the JSON-Schema convention `string` + `format: byte`.
		if t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string", Format: "byte"}
		}
		return &Schema{Type: "array", Items: g.generate(t.Elem(), strict)}
	case reflect.Array:
		return &Schema{Type: "array", Items: g.generate(t.Elem(), strict)}
	case reflect.Map:
		// Only string-keyed maps survive JSON encoding. A map with an
		// `any` value type renders as a free-form object; any other value
		// type renders its own schema under additionalProperties.
		if t.Key().Kind() != reflect.String {
			return &Schema{Type: "object"}
		}
		if t.Elem().Kind() == reflect.Interface {
			return &Schema{Type: "object", AdditionalProperties: true}
		}
		return &Schema{Type: "object", AdditionalProperties: g.generate(t.Elem(), strict)}
	case reflect.Interface:
		// Untyped — emit an empty schema (any shape).
		return &Schema{}
	case reflect.Struct:
		return g.buildStruct(t, strict)
	default:
		return &Schema{}
	}
}

// buildStruct walks the exported fields of t producing an object schema.
// Named structs (carry a Go name + package path) register a single inlined
// definition under Components and return a $ref reference. Anonymous
// structs (struct{...} declared inline as a field type) are emitted
// inline without registration.
//
// A placeholder is inserted in Components BEFORE walking fields so a
// self-referential type (linked list, tree, anything that owns a *Self)
// does not recurse infinitely — the inner field finds the placeholder and
// emits the $ref on its way back up.
func (g *Generator) buildStruct(t reflect.Type, strict bool) *Schema {
	if name := t.Name(); name != "" && t.PkgPath() != "" {
		if _, exists := g.components.Schemas[name]; !exists {
			g.components.Schemas[name] = &Schema{} // recursion break
			g.components.Schemas[name] = g.assembleObject(t, strict)
		}
		return &Schema{Ref: "#/components/schemas/" + name}
	}
	return g.assembleObject(t, strict)
}

// assembleObject builds the inline object schema for t. Used by buildStruct
// both for the inlined-anonymous path and for the component-body path.
func (g *Generator) assembleObject(t reflect.Type, strict bool) *Schema {
	out := &Schema{Type: "object", Properties: map[string]*Schema{}}
	required := map[string]bool{}
	g.walkFields(t, strict, out.Properties, required)
	if len(required) > 0 {
		out.Required = sortedKeys(required)
	}
	return out
}

// walkFields populates properties + required with the body-relevant fields
// of t, recursing into anonymous embeds so their exported fields surface at
// the parent level (mirrors encoding/json's promotion behavior). The
// recursion only descends through anonymous fields; named struct fields
// turn into $ref properties via the regular build() path.
//
// Field skip rules match the framework's body-reflection convention
// (reflectExpectedJSONKeys in web/handle_command.go and extractAllowedKeys
// in web/handle_query.go), so the schema documents exactly what the wrapper
// allows on the wire:
//   - skip unexported fields
//   - skip fields with `json:"-"`
//   - skip fields with `path:"..."`  (URL-segment-bound, not in body)
//   - skip fields with `query:"..."` (query-string-bound, not in body)
//
// `example:"..."` tags on scalar / well-known fields populate the property
// schema's Example so the generated Swagger UI renders a concrete value
// instead of the type-default placeholder. The cached *Schema returned by
// generate() is cloned BEFORE the Example is attached — otherwise two
// fields sharing the same underlying type (two `string` fields with
// different example values) would corrupt each other through the cache.
func (g *Generator) walkFields(t reflect.Type, strict bool, properties map[string]*Schema, required map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			// Mirror encoding/json: even an unexported anonymous struct
			// field (the embedded type's name starts with a lowercase
			// letter) still promotes its EXPORTED inner fields into the
			// parent's JSON shape. Only non-struct anonymous fields of
			// unexported types are skipped — there is nothing meaningful
			// to promote.
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if !f.IsExported() && ft.Kind() != reflect.Struct {
				continue
			}
			if ft.Kind() == reflect.Struct {
				g.walkFields(ft, strict, properties, required)
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("path") != "" {
			continue
		}
		if f.Tag.Get("query") != "" {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		wireName := f.Name
		omitEmpty := false
		if jsonTag != "" {
			parts := strings.Split(jsonTag, ",")
			if parts[0] != "" {
				wireName = parts[0]
			}
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omitEmpty = true
				}
			}
		}
		sub := g.generate(f.Type, strict)
		if raw := f.Tag.Get("example"); raw != "" {
			value, err := parseExampleTag(f.Type, raw)
			if err != nil {
				panic(fmt.Sprintf("openapi: example tag on %s.%s (%q): %v", typeLabel(t), f.Name, raw, err))
			}
			clone := *sub
			clone.Example = value
			sub = &clone
		}
		properties[wireName] = sub
		if shouldRequire(f.Type, strict, omitEmpty) {
			required[wireName] = true
		}
	}
}

// shouldRequire encodes the per-field required rule:
//   - strict (FullBody marker on the handler): always required; pointer and
//     `,omitempty` are ignored. Matches the wrapper's strict-body check
//     which lists every kept field as expected.
//   - lenient: required when the type is non-pointer AND the json tag does
//     not carry `,omitempty`. Pointer or omitempty signals "this field is
//     optional by Go intent", which the consumer expects to see on the wire
//     contract.
func shouldRequire(t reflect.Type, strict, omitEmpty bool) bool {
	if strict {
		return true
	}
	if t.Kind() == reflect.Pointer {
		return false
	}
	if omitEmpty {
		return false
	}
	return true
}

// sortedKeys returns the map's keys in a stable order so the generated
// spec is byte-for-byte reproducible across runs (critical for snapshot
// tests in the bootstrap phase and for diff-friendly artifacts in CI).
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
