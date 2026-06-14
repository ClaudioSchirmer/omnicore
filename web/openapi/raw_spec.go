package openapi

import "reflect"

// ParameterIn enumerates the OpenAPI parameter locations the framework
// emits. Cookie parameters are intentionally out of scope — the canonical
// stack relies on bearer tokens in the Authorization header, not cookies.
type ParameterIn string

const (
	// InPath denotes a URL-path segment (e.g. `:id` in `/users/:id`).
	// Required is always true for path parameters per the OpenAPI spec —
	// MountRaw normalizes the Parameter.Required flag to true at
	// registration when In==InPath.
	InPath ParameterIn = "path"
	// InQuery denotes a URL query-string key.
	InQuery ParameterIn = "query"
	// InHeader denotes an HTTP request header (Accept, X-Tenant-ID, …).
	InHeader ParameterIn = "header"
)

// Parameter describes one path / query / header parameter accepted by a
// MountRaw'd route. The schema-assembly phase folds a slice of these
// into the operation's `parameters[]` block. Type drives the schema:
// nil defaults to `{type: string}` (the most common path/query case);
// a non-nil reflect.Type runs through the Schema generator so structs,
// enums-as-strings, and pointer-nullable variants all behave consistently
// with the canonical wrappers.
type Parameter struct {
	In          ParameterIn
	Name        string
	Description string
	// Required marks the parameter as mandatory. Ignored when In==InPath
	// (path parameters are always required per OpenAPI 3.1 §4.4.4).
	Required bool
	// Type is the Go type the parameter carries on the wire. nil →
	// `{type: string}` shortcut for the typical path/header/free-text
	// case. Pass reflect.TypeOf(int64(0)), reflect.TypeOf(uuid.UUID{}),
	// etc. for typed parameters.
	Type reflect.Type
}

// PathParam is the most common Parameter shape: a required string-typed
// URL segment. Saves the four-field struct literal at every MountRaw
// site that just wants to declare a path segment.
//
//	openapi.PathParam("id", "Identifier of the resource")
func PathParam(name, description string) Parameter {
	return Parameter{
		In:          InPath,
		Name:        name,
		Description: description,
		Required:    true,
	}
}

// QueryParam is the convenience constructor for query parameters whose
// type is a single Go primitive or struct. Required defaults to false —
// the consumer flips it explicitly when needed.
//
//	openapi.QueryParam("limit", "Page size", reflect.TypeOf(int64(0)))
func QueryParam(name, description string, t reflect.Type) Parameter {
	return Parameter{
		In:          InQuery,
		Name:        name,
		Description: description,
		Type:        t,
	}
}

// RequestBody describes a body-carrying route's expected payload. nil
// RequestBody on a RawSpec signals "this route has no body" (Whoami,
// most GET endpoints, the /echo/sse SSE upstream, etc.). When non-nil:
// Type runs through the Schema generator; ContentType defaults to
// application/json when empty.
//
// Examples carries the exemplar payloads the framework folds into the
// Media Type Object's `examples` map. MountRaw validates every Value
// at boot via json-round-trip against Type (when Type is non-nil) —
// the same strict check the canonical wrappers run on
// Doc.RequestExamples. When Type is nil, examples are checked only
// for JSON validity.
type RequestBody struct {
	Description string
	Required    bool
	ContentType string       // default "application/json"
	Type        reflect.Type // type whose schema describes the body

	Examples map[string]Example
}

// ResponseOf is the type-parameterized companion of RouteSpecOf for raw
// route responses. It builds a ResponseSpec from a single type
// parameter, sparing the consumer the `reflect.TypeOf(T{})` invocation
// at every MountRaw call site:
//
//	// Before:
//	Responses: map[int]fwopenapi.ResponseSpec{
//	    200: {
//	        Description: "Authenticated identity",
//	        Type:        reflect.TypeOf(WhoamiResponse{}),
//	    },
//	}
//
//	// After:
//	Responses: map[int]fwopenapi.ResponseSpec{
//	    200: fwopenapi.ResponseOf[WhoamiResponse]("Authenticated identity"),
//	}
//
// For ResponseSpecs that need ContentType / Examples, start from this
// helper and assign the extra fields before storing:
//
//	spec := fwopenapi.ResponseOf[WhoamiResponse]("...")
//	spec.Examples = map[string]fwopenapi.Example{"alice": {...}}
//	Responses: map[int]fwopenapi.ResponseSpec{200: spec}
//
// Use the ResponseSpec struct literal directly when the consumer's
// preference is to keep every field on screen at once — both paths
// coexist; the helper is the common-case shortcut.
func ResponseOf[T any](description string) ResponseSpec {
	return ResponseSpec{
		Description: description,
		Type:        reflect.TypeOf((*T)(nil)).Elem(),
	}
}

// ResponseSpec describes one response status. Type nil signals "no body"
// (typical 204 No Content or pure-error envelope reuse). ContentType
// defaults to application/json when empty.
//
// The spec-assembly phase auto-adds the standard error responses
// (400/401/403/404/409/422/500/503) based on auth.mode and on the
// route's security spec — consumers normally declare only the success
// statuses + any genuinely custom non-2xx (e.g. 502 in
// /showcase/keycloak/admin/:id).
//
// Examples carries the exemplar payloads the framework folds into the
// Media Type Object's `examples` map for this response status. MountRaw
// validates every Value at boot via json-round-trip against Type (when
// Type is non-nil); when Type is nil, examples are checked only for
// JSON validity.
type ResponseSpec struct {
	Description string
	ContentType string       // default "application/json"
	Type        reflect.Type // nil → empty body

	Examples map[string]Example
}

// RawSpec describes a route whose request/response shape cannot be
// extracted from a typed Request DTO + ResponseProjection pair. The
// canonical use cases live in the example service: Whoami (returns a
// `fiber.Map`), Echo (raw bytes, multipart, SSE), Showcase Keycloak /
// HTTPClient (vendor-shaped DTOs the consumer assembles ad-hoc).
//
// MountRaw stores this verbatim in the Registry; the spec-assembly
// phase converts it to OpenAPI in a later round. Prose fields mirror
// Doc (Summary/Description/OperationID/Tags/Deprecated/Hidden/Public)
// so a Raw operation needs no separate Doc value.
type RawSpec struct {
	Summary     string
	Description string
	OperationID string
	Tags        []string
	Deprecated  bool
	// Hidden excludes the operation from the generated spec entirely.
	// Use for internal/diagnostic routes that should not appear in the
	// public surface (Echo upstreams, debug toggles).
	Hidden bool
	// Public bypasses the bearerAuth security entry. Under
	// `auth.mode: jwt`, the spec assembler decorates every operation
	// with `security: [{bearerAuth: []}]` unless this flag is true OR
	// the route appears in `auth.publicRoutes`.
	Public bool

	Parameters  []Parameter
	RequestBody *RequestBody
	// Responses is keyed by HTTP status. The spec assembler walks the
	// map in numeric-ascending order so the rendered spec stays
	// deterministic. The standard error envelopes (400/401/.../500)
	// are added automatically per the framework's Semantic→status
	// table; declare entries here only for genuine deviations.
	Responses map[int]ResponseSpec

	// RequiredPermission is populated by MountRaw when the consumer
	// attaches the RequirePermission option. Same semantics as the
	// matching field on RouteSpec. Set exclusively via the
	// RequirePermission option — consumers do not write to it directly.
	RequiredPermission string
}
