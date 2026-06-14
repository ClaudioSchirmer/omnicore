package openapi

import "reflect"

// RouteSpecOf is the cosmetic helper manual-with-pipeline consumers use
// to produce a RouteSpec from the Request / Response DTOs alone,
// mirroring what the framework's *Spec siblings (HandleCommandWithBodySpec,
// etc.) produce automatically on the canonical path. It exists so the
// call site does not have to spell out `reflect.TypeOf((*T)(nil)).Elem()`
// or `reflect.TypeOf(T{})` per route — the two type parameters carry
// the shapes and the helper builds the struct.
//
// The "no response body" case (DELETE → 204, or any state-transition
// endpoint that returns the canonical envelope without a `data` field)
// is expressed by passing fwresponses.None as TResp; the spec assembler
// already recognizes that sentinel and emits the envelope accordingly,
// matching the wrappers' runtime detection (see web/handle_command.go).
//
// Strict / HasPathID stay zero because the manual path consumes neither
// signal — there is no FullBody marker to type-assert against (the
// handler owns its own body parsing) and there is no auto-`:id` binding
// (the handler resolves identifiers via BindPath / c.Params directly).
// Consumers that need either flag declare it on a hand-built RouteSpec
// literal — the helper is the common-case shortcut, not the only path.
//
// Canonical pairing on a manual route:
//
//	fwopenapi.Mount(d.OpenAPIRegistry, g, fiber.MethodPost, "/",
//	    customInsertUser(d.Pipeline, repo, d.Auditor, svc),
//	    fwopenapi.RouteSpecOf[requests.InsertUserCustomRequest, responses.UserCustomResponse](
//	        fiber.StatusCreated),
//	    fwopenapi.Doc{Summary: "Create a user (manual showcase)", Tags: tags})
func RouteSpecOf[TReq, TResp any](status int) RouteSpec {
	return RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: status,
	}
}

// RouteSpecOfPaged is the paged sibling of RouteSpecOf — manual mounts
// that emit the canonical paged success envelope (data is `[]TResp` and
// `pagination: PaginationInfo` sits at the top level) use this helper so
// the spec assembler renders the envelope shape the wire actually
// produces. Equivalent to RouteSpecOf followed by Paged:true; kept as a
// dedicated function so the call site reads as "this is the paged
// route" without spelling the flag out.
//
// HandleQueryWithParamsSpec on the canonical wrapper path already sets
// Paged:true automatically, so the canonical surface needs no change.
// Use this only on hand-rolled paged routes that delegate the success
// branch to fwweb.RespondPaged.
//
// Pairing TResp with the framework's fwresponses.None (or any zero-data
// projection) is a semantic contradiction — paging requires per-item
// shape — and panics at Mount time.
//
// Canonical pairing on a manual paged route:
//
//	fwopenapi.Mount(d.OpenAPIRegistry, g, fiber.MethodGet, "/",
//	    customListUsers(d.Pipeline, h, parser),
//	    fwopenapi.RouteSpecOfPaged[requests.FindUsersCustomRequest, requests.FindUsersCustomResponse](
//	        fiber.StatusOK),
//	    fwopenapi.Doc{Summary: "List users (manual showcase)", Tags: tags})
func RouteSpecOfPaged[TReq, TResp any](status int) RouteSpec {
	spec := RouteSpecOf[TReq, TResp](status)
	spec.Paged = true
	return spec
}

// RouteSpec is the declarative side-car the canonical wrappers attach to
// every route they document. Two consumers populate it:
//
//   - The framework's *Spec sibling wrappers (HandleCommandWithBodySpec,
//     HandleQueryWithIDSpec, etc.) — they observe TReq / TResp / the
//     FullBody marker on the handler and produce the RouteSpec
//     automatically.
//   - Manual-with-pipeline consumers — they hand-roll a fiber.Handler
//     around their own BodyParser / ParseCriteria / BindPath and pass the
//     same RouteSpec the wrappers would have produced, pointing to the
//     Request / Response DTOs they already declared. The RouteSpecOf
//     helper above produces this struct from the two type parameters
//     alone so the call site stays free of reflect.TypeOf(...) noise.
type RouteSpec struct {
	// RequestType is the body shape declared on the wire. nil for
	// bodyless routes (the framework's HandleCommandWithID family — and
	// any manual handler that does not parse a request body).
	RequestType reflect.Type

	// ResponseType is the wire shape of a successful response, projected
	// from TResult by the wrapper's responseProjection. The schema
	// generator handles `responses.None` like any other struct (empty
	// object); the spec-assembly phase detects it specifically and emits
	// the success envelope WITHOUT a `data` field — matching the runtime
	// behavior of respondWithProjection.
	ResponseType reflect.Type

	// SuccessStatus is the HTTP status code emitted on success.
	SuccessStatus int

	// Strict toggles the FullBody-marker semantic on the generated body
	// schema. strict → every kept field is required; lenient → only
	// non-pointer non-`,omitempty` fields are required.
	Strict bool

	// HasPathID is true when the wrapper auto-binds the Fiber `:id`
	// segment via the pipeline.CommandWithID / queries.FindByIDQuery
	// interface. Combined with the route's Fiber path during spec
	// assembly to emit the corresponding `parameters[].in=path` entry —
	// path: tags on the Request DTO produce additional path parameters,
	// independent of this flag.
	HasPathID bool

	// RequiredPermission is populated by Mount when the consumer attaches
	// the RequirePermission option. Empty string means "no Layer 1 gate
	// declared" (the route may still emit 403 via Layer 2 BuildRules or
	// Layer 3 tenant mismatch). The OpenAPI generator reads this field
	// to append the "**Required permission:** `<p>`" line to the
	// operation description — but only when the runtime gate is
	// actually enforcing (`auth.mode: jwt` AND
	// `auth.authorization.enabled: true`). Under disabled or
	// jwt-without-authz the value stays here for introspection while the
	// description suffix is omitted, so the spec never claims a
	// constraint the server is not honoring. Set exclusively via the
	// RequirePermission option — consumers do not write to it directly.
	RequiredPermission string

	// Paged is true when the route emits the canonical paged success
	// envelope: the success response body carries `data` typed as an
	// array of ResponseType items AND a `pagination` property at the top
	// level (PaginationInfo schema — has_next, has_prev, next_cursor,
	// prev_cursor, total). When false, the envelope's `data` is a single
	// item shaped like ResponseType and no `pagination` is emitted —
	// matching the runtime behavior of fwweb.RespondPaged vs
	// fwweb.RespondWithSuccess.
	//
	// HandleQueryWithParamsSpec sets it automatically; HandleQueryWithID
	// keeps it false. Manual mounts opt in via the RouteSpecOfPaged
	// helper. Paged:true paired with a nil ResponseType or
	// responses.None is a semantic contradiction — paging requires
	// per-item shape — and panics at Mount time.
	Paged bool
}

// Doc carries the prose the spec-assembly phase folds into the
// Operation Object: human-readable summary, longer description,
// canonical operationId, tag list, deprecation marker, and the two
// visibility toggles (Hidden — exclude from the spec entirely; Public —
// skip the `security: [{bearerAuth: []}]` entry when the service is
// running under `auth.mode: jwt`).
//
// RequestExamples / ResponseExamples carry the per-route exemplar
// payloads the framework folds into the Media Type Object's `examples`
// map (OpenAPI 3.x). The simple-case shortcut for one example per
// scalar field is the schema generator's `example:"..."` struct tag —
// these maps are the rich-case path for N exemplos or shapes a single
// tag cannot express (composite payloads, multi-variant scenarios).
//
// Success-status response examples are wrapped automatically in the
// canonical Response envelope (`success:true`, `status`, `description`,
// `data: <value>`). Examples on non-success statuses are emitted raw —
// the consumer fully controls the envelope (typically `success:false`
// with their own `errors[]` shape). The framework's default
// per-status error example (DefaultErrorExample) is auto-merged under
// the key `"default"` whenever the consumer declares any other
// examples on that status; declaring `"default"` explicitly overrides
// the canonical entry, and declaring it with an empty Value removes
// the canonical entry from the rendered spec.
//
// Statuses declared via ResponseExamples that are NOT in the
// framework's auto-added error envelope set (400/401/403/404/422/500
// plus the success status) auto-create a response entry on the rendered
// spec, reusing the shared ErrorEnvelope schema and the consumer's
// examples — typical case is 409 Conflict surfaced by a service
// notification that overrides Semantic(). For those, the `default`
// auto-merge is skipped because no DefaultErrorExample entry exists for
// the status; the consumer's examples render as-is.
type Doc struct {
	Summary     string
	Description string
	OperationID string
	Tags        []string
	Deprecated  bool
	Hidden      bool
	Public      bool

	// RequestExamples names the exemplar request payloads attached to the
	// operation's `requestBody.content.application/json.examples` block.
	// Keys are the example identifiers Swagger UI shows in the dropdown.
	// Mount validates every Value at boot via json-round-trip against
	// RouteSpec.RequestType — a typo on a field name aborts the boot with
	// a diagnostic naming (route, example, cause).
	RequestExamples map[string]Example

	// ResponseExamples names the exemplar response payloads attached to
	// each status's `content.application/json.examples` block. The outer
	// map is keyed by HTTP status code. Mount validates every
	// success-status Value at boot via json-round-trip against
	// RouteSpec.ResponseType; non-success-status Values are checked only
	// for JSON validity (the ErrorEnvelope shape is shared and accepts
	// arbitrary `errors[]` content).
	ResponseExamples map[int]map[string]Example
}
