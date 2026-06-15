package web

import (
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

// Operator constants for filter declarations in a Request DTO's `filter:"..."`
// struct tag. The Op prefix avoids collision with any consumer-defined Op
// values (e.g. an OpenAPI operation enum) and groups them as a coherent set.
//
// The string operators come in case-sensitive and case-insensitive variants
// (prefixed with `i`): a field declaring `filter:"eq,ieq,startswith,istartswith"`
// accepts `?name=Bob` (exact), `?name.ieq=bob` (case-insensitive equality),
// `?name.startswith=Bob` (prefix), and `?name.istartswith=bob` (case-insensitive
// prefix) — each is opt-in per call. Numeric operators (gte/lte/gt/lt) have no
// `i` variant by design — case-folding has no meaning on ordinal comparisons.
const (
	OpEq           = "eq"
	OpNe           = "ne"
	OpIn           = "in"
	OpNin          = "nin"
	OpGte          = "gte"
	OpLte          = "lte"
	OpGt           = "gt"
	OpLt           = "lt"
	OpStartsWith   = "startswith"
	OpContains     = "contains"
	OpIEq          = "ieq"
	OpINe          = "ine"
	OpIIn          = "iin"
	OpINin         = "inin"
	OpIStartsWith  = "istartswith"
	OpIContains    = "icontains"
)

// HasToParamsQuery is the contract for Request DTOs that produce a
// FindByParamsQuery. The wrapper parses the HTTP query string into a
// ReadCriteria (filters + pagination) and forwards it verbatim to ToQuery.
// The web→application boundary is dumb mapping; *AppContext is consumed by
// the Query's ToCriteria(ctx) downstream, where identity-derived overlays
// (tenant id, owner id) layer onto the wire criteria.
type HasToParamsQuery[TQ queries.FindByParamsQuery] interface {
	ToQuery(criteria queries.ReadCriteria) TQ
}

// HasToIDQuery is the contract for Request DTOs that produce a FindByIDQuery.
// Only the `includeArchived` reserved query parameter is parsed into the Request
// via Fiber's QueryParser; the rest of the wire state is the path id (injected
// by the wrapper post-ToQuery). The web→application boundary is dumb mapping;
// *AppContext is consumed by the Query's ToCriteria(ctx) downstream.
type HasToIDQuery[TQ queries.FindByIDQuery] interface {
	ToQuery() TQ
}

// HandleQueryWithParams creates a fiber.Handler for paged list endpoints. It
// owns the wire format (query-string parsing, allowlist enforcement, JSON
// envelope with top-level pagination); the application layer stays Fiber-agnostic.
//
// Flow:
//  1. extractAllowedKeys(sample) — inspects TReq's `query:"X" filter:"ops"`
//     tags. Cached by reflect.Type.
//  2. Walks the query string; rejects any key outside the allowlist or any
//     operator outside the declared list with 400 SchemaViolationNotification.
//  3. Builds queries.ReadCriteria with filters + pagination controls.
//  4. req.ToQuery(criteria) — pure body mapping at the wire boundary.
//     AppContext-derived filters layer onto the criteria inside the
//     Query's ToCriteria(ctx), consumed by the handler.
//  5. Dispatch via Pipeline.
//  6. On Success — project each page.Items doc via projector, emit Data=[]R
//     + top-level Pagination. On Failure — RespondFromResult standard path
//     (translates notifications).
//
// HTTP semantics:
//   - 200 → page rendered
//   - 400 → query-string key outside allowlist OR operator outside declared list
//   - other 4xx → custom semantics via Notification.Semantic()
//
// projector is mandatory — pass responses.RawDoc to keep the raw view doc
// shape on the wire, or a consumer-defined R{}.FromDoc to declare a typed
// wire contract:
//
//	users.Get("/", fwweb.HandleQueryWithParams(d.Pipeline,
//	    requests.FindUsersByParamsRequest{},
//	    requests.FindUsersByParamsResponse{}.FromDoc,
//	    &handlers.FindByParamsQueryHandler[*queries.FindUserByParamsQuery]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
func HandleQueryWithParams[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery, R any](
	pipe *pipeline.Pipeline,
	sample TReq,
	projector func(map[string]any) R,
	h pipeline.Handler[TQ, queries.Page],
) fiber.Handler {
	_ = sample
	reqType := reflect.TypeOf(sample)
	if reqType.Kind() == reflect.Pointer {
		reqType = reqType.Elem()
	}
	schema := extractAllowedKeys(reqType)
	pathSchema := inspectPathTags(reqType)
	// projSchema is the Response-side mapping (wire path → doc path) used to
	// validate and translate `?fields=` AND `?sort=` values when the Request
	// DTO opts in to either parameter AND the Response is a struct. Built
	// once and shared between the two reserved keys.
	//
	// The fields-specific boot guard (every field *T + ,omitempty, recursive)
	// only fires when the Request DTO declared `query:"fields"` — sort has
	// no analogous structural requirement on the Response (it consumes only
	// the wire→doc path map). When the Response is map[string]any (RawDoc-
	// style projectors), projSchema stays nil and both keys fall back to
	// pass-through mode at the buildCriteria layer.
	var projSchema *projectionSchema
	fieldsOptIn := schema.reserved["fields"]
	sortOptIn := schema.reserved["sort"]
	if fieldsOptIn || sortOptIn {
		respType := reflect.TypeOf((*R)(nil)).Elem()
		for respType.Kind() == reflect.Pointer {
			respType = respType.Elem()
		}
		if respType.Kind() == reflect.Struct {
			if fieldsOptIn {
				if errs := validateFieldsResponse(respType); len(errs) > 0 {
					panic(formatFieldsResponseGuard(respType, errs))
				}
			}
			projSchema = extractProjectionSchema(respType)
		}
	}
	// Boot-time advisory when the Request DTO accepts ?sort=. The framework
	// has no way to verify, from the wrapper, that the Mongo view declares
	// indexes covering the sortable wire paths — the ViewDefinition lives in
	// a separate construction site (ReadableFeature.Views()). The warning
	// lists every sortable wire path discovered on the Response so the
	// operator can compare it against the view's .Indexes(...) declaration
	// during the same boot.
	if sortOptIn && projSchema != nil {
		paths := make([]string, 0, len(projSchema.paths))
		for wirePath := range projSchema.paths {
			paths = append(paths, wirePath)
		}
		sort.Strings(paths)
		slog.Warn("query.sort.opt-in: endpoint accepts ?sort=; verify Mongo indexes cover the sortable wire paths to avoid performance degradation on large collections",
			"request", reqType.String(),
			"sortable_wire_paths", paths)
	}
	return func(c fiber.Ctx) error {
		crit, badField, ok := buildCriteria(c, schema, projSchema)
		if !ok {
			return respondSchemaViolation[queries.Page](c, pipe, badField)
		}
		var req TReq
		if bad, ok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !ok {
			return respondSchemaViolation[queries.Page](c, pipe, bad)
		}
		appCtx := AppContext(c)
		appCtx.SetParent(c)
		q := req.ToQuery(crit)
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if result.IsSuccess() {
			return RespondPaged(c, fiber.StatusOK, result.Value(), projector)
		}
		return RespondFromResult(c, result, fiber.StatusOK)
	}
}

// HandleQueryWithID creates a fiber.Handler for read-by-id endpoints. The
// only reserved query-string parameter is `?includeArchived=true`; anything
// else produces 400. The path id is injected into the Query via SetPathID
// after ToQuery, mirroring HandleCommandWithBodyID on the write side.
//
// projector is mandatory — pass responses.RawDoc to keep the raw view doc
// shape on the wire, or a consumer-defined R{}.FromDoc to declare a typed
// wire contract:
//
//	users.Get("/:id", fwweb.HandleQueryWithID(d.Pipeline,
//	    requests.FindUserByIDRequest{},
//	    requests.FindUserByIDResponse{}.FromDoc,
//	    &handlers.FindByIDQueryHandler[*queries.FindUserByIDQuery]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
func HandleQueryWithID[TReq HasToIDQuery[TQ], TQ queries.FindByIDQuery, R any](
	pipe *pipeline.Pipeline,
	sample TReq,
	projector func(map[string]any) R,
	h pipeline.Handler[TQ, map[string]any],
) fiber.Handler {
	_ = sample
	reqType := reflect.TypeOf(sample)
	if reqType.Kind() == reflect.Pointer {
		reqType = reqType.Elem()
	}
	pathSchema := inspectPathTags(reqType)
	if hasPathSegment(reqType, "id") {
		panic(formatPathIDConflict("HandleQueryWithID", reqType))
	}
	return func(c fiber.Ctx) error {
		if bad, ok := validateByIDQuery(c); !ok {
			return respondSchemaViolation[map[string]any](c, pipe, bad)
		}
		var req TReq
		if err := c.Bind().Query(&req); err != nil {
			return respondSchemaViolation[map[string]any](c, pipe, "includeArchived")
		}
		if bad, ok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !ok {
			return respondSchemaViolation[map[string]any](c, pipe, bad)
		}
		appCtx := AppContext(c)
		appCtx.SetParent(c)
		q := req.ToQuery()
		q.SetPathID(c.Params("id"))
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if result.IsSuccess() {
			return RespondWithSuccess(c, fiber.StatusOK, projector(result.Value()))
		}
		return RespondFromResult(c, result, fiber.StatusOK)
	}
}

// ParseCriteria walks c's query string and validates it against the allowlist
// declared by requestDTO's `query:"..." filter:"..."` tags — the same
// reflection-based schema HandleQueryWithParams uses internally. Returns the
// assembled ReadCriteria on success.
//
// On the first violation (unknown key OR operator outside the declared list)
// returns (zero, badKey, false). Callers forward badKey to
// RespondSchemaViolation to emit the canonical 400 envelope, keeping the wire
// shape consistent with the auto path:
//
//	crit, badField, ok := fwweb.ParseCriteria(c, requests.FindUsersCustomRequest{})
//	if !ok {
//	    return fwweb.RespondSchemaViolation(c, pipe, badField)
//	}
//
// Use this helper when the manual handler has no typed Response (RawDoc-style
// projections, vendor-shaped envelopes) OR the Request DTO does not opt into
// `?fields=` / `?sort=`. Manual handlers that declare a typed Response AND
// opt into either reserved key should construct a [QueryParser] at Mount
// time instead — that path runs the same boot-time guards (sparse-render
// contract on the Response + sortable-paths advisory) the canonical
// HandleQueryWithParams wrapper enforces, plus wire→doc translation at
// runtime. ParseCriteria stays as the un-typed escape hatch; it does not
// build a projection schema, so a stray `?fields=` token lands in
// pass-through mode (no allowlist, no `_id:0` auto-exclusion) and a stray
// `?sort=` token lands verbatim (no snake_case translation).
//
// Manual handlers that prefer to assemble the criteria differently can ignore
// both helpers and build the ReadCriteria by hand — the framework does not
// require either.
func ParseCriteria(c fiber.Ctx, requestDTO any) (queries.ReadCriteria, string, bool) {
	schema := extractAllowedKeys(reflect.TypeOf(requestDTO))
	// Manual handlers do not declare a typed Response to the wrapper, so
	// the projection schema is nil — `?fields=` works in pass-through mode
	// (each comma-separated token becomes an inclusion entry verbatim; no
	// allowlist, no wire→doc translation, no `_id:0` auto-exclusion). The
	// canonical surface goes through HandleQueryWithParams (or QueryParser
	// in manual mounts), which gates the param behind a typed Response +
	// boot guard.
	return buildCriteria(c, schema, nil)
}

// QueryParser is the typed Mount-time-constructed parser for manual query
// handlers whose Request DTO opts into `?fields=` / `?sort=` AND that
// declare a typed Response. It closes the asymmetry [ParseCriteria] carries
// against the canonical [HandleQueryWithParams] wrapper:
//
//   - The construction (in [NewQueryParser]) runs the exact same boot scan
//     the canonical wrapper runs at lines parallel to its sample-driven
//     reflection: [validateFieldsResponse] panics on Responses that violate
//     the sparse-render contract (every field at every depth must be *T or
//     a slice/map with `,omitempty`); [extractProjectionSchema] builds the
//     wire→doc path map; an `slog.Warn` advisory enumerates the sortable
//     wire paths when the Request opts into `?sort=` so the operator can
//     compare them against the Mongo view's declared indexes during the
//     same boot.
//   - The [QueryParser.Parse] call walks the request query string against
//     the cached schema + projection — runtime allowlist + wire→doc
//     translation are enabled when applicable, so a `?fields=addresses.zipCode`
//     token translates to `{addresses.zip_code: 1}` and an unknown
//     `?fields=bogus` surfaces as `fields[bogus]` on the canonical 400
//     envelope, exactly as the auto wrapper.
//
// Lifecycle: construct once at Mount (Resp's reflect.Type drives the boot
// scan), then reuse the same instance for every request. The same cached
// schemas the canonical wrapper memoizes are reused — there is no per-route
// memory penalty for going through the manual surface.
//
// Example:
//
//	listParser := fwweb.NewQueryParser[requests.FindUsersCustomRequest,
//	                                   requests.FindUsersCustomResponse]()
//	// ... per-request handler:
//	crit, badField, ok := listParser.Parse(c)
//	if !ok {
//	    return fwweb.RespondSchemaViolation(c, pipe, badField)
//	}
//
// When Resp is `map[string]any` (RawDoc-style projector) the parser
// degrades cleanly: projSchema stays nil and Parse behaves identically to
// [ParseCriteria]. The construction is still safe to call — no panic, no
// warn — so consumers can adopt it uniformly across mounts regardless of
// whether the Response is typed.
type QueryParser[Req any, Resp any] struct {
	schema     *requestSchema
	projSchema *projectionSchema
}

// NewQueryParser builds a [QueryParser] at Mount time. Runs the boot scan
// detailed on the type doc: schema extraction, fields-side structural
// guard (panic on violation), projection-schema build, sortable-paths
// advisory. Panics on the same condition [HandleQueryWithParams] panics:
// the Request DTO declares `query:"fields"` AND the Response shape
// violates the sparse-render contract.
func NewQueryParser[Req any, Resp any]() *QueryParser[Req, Resp] {
	reqType := reflect.TypeOf((*Req)(nil)).Elem()
	for reqType.Kind() == reflect.Pointer {
		reqType = reqType.Elem()
	}
	schema := extractAllowedKeys(reqType)

	var projSchema *projectionSchema
	fieldsOptIn := schema.reserved["fields"]
	sortOptIn := schema.reserved["sort"]
	if fieldsOptIn || sortOptIn {
		respType := reflect.TypeOf((*Resp)(nil)).Elem()
		for respType.Kind() == reflect.Pointer {
			respType = respType.Elem()
		}
		if respType.Kind() == reflect.Struct {
			if fieldsOptIn {
				if errs := validateFieldsResponse(respType); len(errs) > 0 {
					panic(formatFieldsResponseGuard(respType, errs))
				}
			}
			projSchema = extractProjectionSchema(respType)
		}
	}
	if sortOptIn && projSchema != nil {
		paths := make([]string, 0, len(projSchema.paths))
		for wirePath := range projSchema.paths {
			paths = append(paths, wirePath)
		}
		sort.Strings(paths)
		slog.Warn("query.sort.opt-in: endpoint accepts ?sort=; verify Mongo indexes cover the sortable wire paths to avoid performance degradation on large collections",
			"request", reqType.String(),
			"sortable_wire_paths", paths)
	}
	return &QueryParser[Req, Resp]{schema: schema, projSchema: projSchema}
}

// Parse walks c's query string against the cached schema + projection
// captured at construction. Same return shape as [ParseCriteria]
// — (criteria, "", true) on success or (zero, badKey, false) on the first
// violation. Forward badKey to [RespondSchemaViolation] to emit the
// canonical 400 envelope:
//
//	crit, badField, ok := parser.Parse(c)
//	if !ok {
//	    return fwweb.RespondSchemaViolation(c, pipe, badField)
//	}
func (p *QueryParser[Req, Resp]) Parse(c fiber.Ctx) (queries.ReadCriteria, string, bool) {
	return buildCriteria(c, p.schema, p.projSchema)
}

// RespondSchemaViolation emits the canonical 400 envelope carrying
// SchemaViolationNotification (semantic Schema, context "Schema") for the
// given bad field. Manual query handlers that opt out of
// HandleQueryWithParams should use it to reject unknown query keys uniformly
// with the wrapper:
//
//	crit, badField, ok := fwweb.ParseCriteria(c, requestDTO)
//	if !ok {
//	    return fwweb.RespondSchemaViolation(c, pipe, badField)
//	}
//
// Pass an empty field when the violation is body-shaped rather than
// field-scoped (malformed JSON, missing root object). The wire envelope
// stays the same; only the per-field name is omitted.
func RespondSchemaViolation(c fiber.Ctx, pipe *pipeline.Pipeline, field string) error {
	return respondSchemaViolation[any](c, pipe, field)
}

// ProjectPage walks page.Items applying fn per document and returns the
// projected items + a PaginationInfo ready to be placed on
// Response.Pagination. Used by RespondPaged and by manual paged handlers
// that want to assemble the envelope by hand.
//
// When page.OnlyTotal is true, ProjectPage is the wrong primitive — there
// are no items to project and the pagination shape is dedicated. Callers
// should branch on page.OnlyTotal themselves (or use RespondPaged, which
// branches internally).
func ProjectPage[R any](page queries.Page, fn func(map[string]any) R) ([]R, *PaginationInfo) {
	items := make([]R, len(page.Items))
	for i, doc := range page.Items {
		items[i] = fn(doc)
	}
	return items, &PaginationInfo{
		HasNext:    page.HasNext,
		HasPrev:    page.HasPrev,
		NextCursor: page.NextCursor,
		PrevCursor: page.PrevCursor,
		Total:      page.Total,
	}
}

// RespondPaged emits the canonical paged success envelope: Data carries the
// projected items, Pagination carries the cursor envelope at the top level.
// Convenience wrapper around ProjectPage for the simple case where the
// handler returns a queries.Page directly and the consumer just wants the
// projection + envelope assembled.
//
// When page.OnlyTotal is true, the envelope flips to the count-only shape:
// Data is omitted entirely and Pagination is a TotalOnlyPagination carrying
// solely Total. The listing-only fields (has_next/has_prev/cursors) are not
// emitted — they would carry zero-value noise that misleads consumers in
// count mode.
func RespondPaged[R any](c fiber.Ctx, status int, page queries.Page, fn func(map[string]any) R) error {
	if page.OnlyTotal {
		return Respond(c, Response{
			Success:     true,
			Status:      status,
			Description: http.StatusText(status),
			Pagination:  &TotalOnlyPagination{Total: page.Total},
		})
	}
	items, pagination := ProjectPage(page, fn)
	return Respond(c, Response{
		Success:     true,
		Status:      status,
		Description: http.StatusText(status),
		Data:        items,
		Pagination:  pagination,
	})
}

// reservedParamKeys are the wire keys recognized as pagination/projection
// controls on the params endpoint. They never carry an operator suffix.
var reservedParamKeys = map[string]bool{
	"limit":           true,
	"after":           true,
	"before":          true,
	"sort":            true,
	"fields":          true,
	"search":          true,
	"includeArchived": true,
	"onlyTotal":       true,
}

// onlyTotalConflicts is the set of reserved keys whose semantics are
// incompatible with the count-only mode triggered by `?onlyTotal=true`. They
// shape items / cursors / projection — none of which exist in a response that
// carries solely `pagination.total`. The wrapper rejects the combination with
// 400 SchemaViolationNotification on the first conflict so the consumer
// surfaces the bug immediately (silent ignore would hide it). Filter leaves
// (declared via `query:"X" filter:"ops"` on the Request DTO) + `search` +
// `includeArchived` stay valid in count mode — counting a filtered subset is
// the canonical use case.
var onlyTotalConflicts = map[string]bool{
	"fields": true,
	"sort":   true,
	"limit":  true,
	"after":  true,
	"before": true,
}

// filterSpec lists the operators declared by a single filter leaf via the
// `filter:"eq,in"` struct tag, together with the document path the leaf maps
// to and the leaf's declared Go base kind used to coerce wire values into
// typed criteria. docPath defaults to the wire key (dotted, including the
// embed prefix) and is overridden segment-by-segment by `view:"name"` tags
// on the way down. goKind drives value coercion at runtime — a `*string`
// leaf keeps "95014" as the literal string "95014" (no silent int parse),
// matching the column type Mongo stored; a `*int64` leaf parses "25" into
// int64(25) so the criteria type matches the field's stored type.
type filterSpec struct {
	ops     map[string]bool
	docPath string
	goKind  reflect.Kind
}

// requestSchema is the cached reflection result for a Request DTO type:
// every accepted wire key (flat or dotted) → its filterSpec, plus the
// reserved pagination/control set (top-level only — embed groups carry
// filter leaves, not pagination keys).
type requestSchema struct {
	filters  map[string]filterSpec
	reserved map[string]bool
}

// schemaCache memoizes extractAllowedKeys by reflect.Type. The first wrapper
// construction pays the inspection; later wrappers for the same Request DTO
// reuse the schema.
var schemaCache sync.Map // map[reflect.Type]*requestSchema

// extractAllowedKeys inspects a Request DTO's struct tags to produce its
// schema. Three field kinds per level:
//
//   - leaf filter — `query:"X" filter:"ops..."` declares wire key X (prefixed
//     by parent embed when nested) and the operator allowlist. Optional
//     `view:"docName"` overrides the doc field at this segment.
//   - reserved control — `query:"limit"` (etc.) with no `filter:` tag. Only
//     honored at the TOP LEVEL; reserved keys inside an embed group are
//     ignored at runtime (the framework's reserved set is endpoint-wide,
//     not per-embed).
//   - embed group — `query:"prefix"` on a struct-typed field with no
//     `filter:` tag. Recurses into the inner type, prefixing both wire keys
//     and doc paths. Carries an optional `view:"docPrefix"` to rename the
//     doc-side prefix independently of the wire prefix (mirrors `view:` at
//     the leaf level).
//
// Pointer-to-struct is supported transparently — both pointer and value
// nested groups recurse the underlying struct type.
func extractAllowedKeys(t reflect.Type) *requestSchema {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if v, ok := schemaCache.Load(t); ok {
		return v.(*requestSchema)
	}
	s := &requestSchema{
		filters:  map[string]filterSpec{},
		reserved: map[string]bool{},
	}
	walkSchemaLevel(t, "", "", s, true)
	schemaCache.Store(t, s)
	return s
}

// walkSchemaLevel recurses through a Request DTO type, accumulating leaf
// filters into s.filters keyed by the dotted wire path. wirePrefix and
// docPrefix carry the path built so far; topLevel gates the recognition of
// reserved pagination keys (only honored at depth 0).
func walkSchemaLevel(t reflect.Type, wirePrefix, docPrefix string, s *requestSchema, topLevel bool) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		qkey := f.Tag.Get("query")
		if qkey == "" {
			continue
		}
		wirePath := joinPath(wirePrefix, qkey)
		// Default doc segment uses the framework's snake-case convention
		// (zipCode → zip_code), matching the auto-inferred Postgres column
		// name. The `view:` tag overrides verbatim for legacy schemas or
		// vendor-shaped projections that diverge from the convention.
		docSegment := domain.PascalToSnake(qkey)
		if v := f.Tag.Get("view"); v != "" {
			docSegment = v
		}
		docPath := joinPath(docPrefix, docSegment)

		ftag := f.Tag.Get("filter")
		if ftag != "" {
			ops := map[string]bool{}
			for _, op := range strings.Split(ftag, ",") {
				ops[strings.TrimSpace(op)] = true
			}
			// Capture the leaf's base kind for type-driven value coercion.
			// Pointer indirection is collapsed — `*string` leaves coerce
			// identically to `string`. Composite kinds (slices, structs)
			// fall back to string at the coercion site below.
			leafType := f.Type
			for leafType.Kind() == reflect.Pointer {
				leafType = leafType.Elem()
			}
			s.filters[wirePath] = filterSpec{ops: ops, docPath: docPath, goKind: leafType.Kind()}
			continue
		}

		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			// Embed group — recurse with the prefixes extended. View at the
			// embed renames the doc-side prefix without touching the wire.
			walkSchemaLevel(ft, wirePath, docPath, s, false)
			continue
		}

		// Reserved pagination/control keys are recognized only at the top
		// level. An embed group's "limit" (or similar) is silently ignored
		// — only the endpoint-wide reserved set is honored.
		if topLevel {
			s.reserved[qkey] = true
		}
	}
}

// joinPath concatenates two non-empty segments with a single dot, returning
// either one verbatim when the other is empty.
func joinPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	if segment == "" {
		return prefix
	}
	return prefix + "." + segment
}

// parseKeyAgainstSchema resolves a wire key into (wirePath, op). The logic
// is whole-key-first: if the literal key is a declared filter, no operator
// is peeled (handles fields whose name happens to match an op, e.g. a leaf
// declared `query:"in"`). Otherwise, if the key ends in a known operator
// suffix and the remaining prefix is a declared filter, the suffix is
// honored as the operator. Returns ("", "") when the key matches neither —
// caller surfaces the wire key verbatim in the 400 envelope.
func parseKeyAgainstSchema(key string, s *requestSchema) (string, string) {
	if _, ok := s.filters[key]; ok {
		return key, ""
	}
	idx := strings.LastIndexByte(key, '.')
	if idx < 0 {
		return "", ""
	}
	wirePath, op := key[:idx], key[idx+1:]
	if !knownOps[op] {
		return "", ""
	}
	if _, ok := s.filters[wirePath]; !ok {
		return "", ""
	}
	return wirePath, op
}

// knownOps is the membership set of every declared operator constant. Drives
// parseKeyAgainstSchema's "peel only if last segment is a known op" rule —
// keeping it in sync with the OpXxx constants below is the contract.
var knownOps = map[string]bool{
	OpEq:          true,
	OpNe:          true,
	OpIn:          true,
	OpNin:         true,
	OpGte:         true,
	OpLte:         true,
	OpGt:          true,
	OpLt:          true,
	OpStartsWith:  true,
	OpContains:    true,
	OpIEq:         true,
	OpINe:         true,
	OpIIn:         true,
	OpINin:        true,
	OpIStartsWith: true,
	OpIContains:   true,
}

// buildCriteria walks the query string, validates each key against the schema,
// and produces ReadCriteria. Returns (criteria, "", true) on success or
// (zero, badKey, false) on the first violation (unknown wire path OR operator
// outside the declared list for that path).
//
// projSchema is consulted only when the wire carries `?fields=`. When
// non-nil, each comma-separated token is validated against the Response
// DTO's declared wire paths and translated to the corresponding doc path
// (PascalToSnake by default, `view:` override). An unknown token surfaces
// the bad field on the canonical 400 envelope as `fields[<token>]`. Top-
// level `id` triggers the framework's auto-exclusion: when the consumer
// did NOT request `id`, the projection adds `_id: 0` so Mongo's default
// `_id` inclusion is dropped from the wire shape. When projSchema is nil,
// the legacy pass-through behavior applies: every token becomes an
// inclusion entry verbatim (no allowlist, no translation).
func buildCriteria(c fiber.Ctx, s *requestSchema, projSchema *projectionSchema) (queries.ReadCriteria, string, bool) {
	crit := queries.ReadCriteria{Filter: map[string]any{}}
	// Pre-scan onlyTotal: VisitAll's iteration order is non-deterministic, so
	// we cannot rely on observing onlyTotal before a conflicting key. Reading
	// it explicitly up front lets the loop reject conflicts on first sight.
	// The flag is only honored when the Request DTO declared `query:"onlyTotal"`
	// (s.reserved gates opt-in, same posture as `includeArchived` / `search` /
	// `fields` / `sort`).
	onlyTotalOn := s.reserved["onlyTotal"] && c.Query("onlyTotal") == "true"
	if onlyTotalOn {
		crit.OnlyTotal = true
	}
	var badField string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, v []byte) {
		if !ok {
			return
		}
		key := string(k)
		val := string(v)

		// Conflict matrix for the count-only mode. Filter leaves and the
		// still-meaningful reserved keys (`search`, `includeArchived`) flow
		// through unaffected — counting a filtered subset is the canonical
		// use case.
		if onlyTotalOn && onlyTotalConflicts[key] {
			badField = "onlyTotal[" + key + "]"
			ok = false
			return
		}

		if s.reserved[key] && reservedParamKeys[key] {
			if key == "onlyTotal" {
				// Already absorbed by the pre-scan; nothing else to apply.
				return
			}
			if key == "fields" {
				proj, wireSet, bad, projOk := parseProjection(val, projSchema)
				if !projOk {
					badField = "fields[" + bad + "]"
					ok = false
					return
				}
				if projSchema != nil && !wireSet["id"] {
					// Mongo always returns `_id` unless explicitly excluded.
					// The consumer did not request `id` (wire); drop it so
					// the typed Response (every field pointer+omitempty) does
					// not render an `id` the caller did not ask for.
					proj["_id"] = 0
				}
				crit.Projection = proj
				return
			}
			if key == "sort" {
				sortFields, bad, sortOk := parseSortWithSchema(val, projSchema)
				if !sortOk {
					badField = "sort[" + bad + "]"
					ok = false
					return
				}
				crit.Sort = sortFields
				return
			}
			if bad, paramOk := applyReservedParam(&crit, key, val); !paramOk {
				badField = bad
				ok = false
				return
			}
			return
		}

		wirePath, op := parseKeyAgainstSchema(key, s)
		if wirePath == "" {
			badField = key
			ok = false
			return
		}
		spec := s.filters[wirePath]

		effective := op
		if effective == "" {
			effective = OpEq
		}
		if !spec.ops[effective] {
			badField = key
			ok = false
			return
		}
		applyFilterParam(crit.Filter, spec, op, val)
	})
	if !ok {
		return crit, badField, ok
	}
	// Post-loop cursor consistency checks. Doing them here (rather than
	// inline in applyReservedParam) lets `?after` and `?sort` land in any
	// URL order — the loop just records both, this block reconciles.
	if crit.After != "" && crit.Before != "" {
		return crit, "after,before", false
	}
	if crit.After != "" {
		if bad, cursorOk := validateCursorAgainstCriteria(crit.After, crit, "after"); !cursorOk {
			return crit, bad, false
		}
	}
	if crit.Before != "" {
		if bad, cursorOk := validateCursorAgainstCriteria(crit.Before, crit, "before"); !cursorOk {
			return crit, bad, false
		}
	}
	return crit, "", true
}

// validateCursorAgainstCriteria decodes the cursor and asserts two
// alignments with the current criteria:
//
//   - tuple length: len(K)-1 == len(Sort) (the trailing K element is always
//     _id). A structural pre-check — protects against malformed cursors
//     before the reader's keyset builder indexes the tuple.
//   - context hash: cursor.H == HashContext(filter, sort, search, archived).
//     Covers every listing axis. A mismatch means the consumer changed any
//     of filter / sort / search / includeArchived mid-navigation.
//
// Either case rejects with 400 SchemaViolationNotification on the cursor's
// wire key so the consumer requests page 1 of the new context before
// navigating instead of silently honoring the keyset boundary against a
// different result set.
func validateCursorAgainstCriteria(cursorStr string, crit queries.ReadCriteria, wireKey string) (string, bool) {
	cursor, err := queries.DecodeCursor(cursorStr)
	if err != nil {
		return wireKey, false
	}
	if len(cursor.K)-1 != len(crit.Sort) {
		return wireKey, false
	}
	if cursor.H != queries.HashContext(crit.Filter, crit.Sort, crit.Search, crit.IncludeArchived) {
		return wireKey, false
	}
	return "", true
}

// validateByIDQuery enforces the by-id allowlist: only `includeArchived` is
// allowed. Returns ("", true) on a clean query string, (badKey, false) otherwise.
func validateByIDQuery(c fiber.Ctx) (string, bool) {
	var bad string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, _ []byte) {
		if !ok {
			return
		}
		if string(k) != "includeArchived" {
			bad = string(k)
			ok = false
		}
	})
	return bad, ok
}

// applyReservedParam materializes a known pagination/control key into the
// criteria, validating its shape strictly. Returns ("", true) on success or
// (badField, false) on the first violation so the wrapper can short-circuit
// to the canonical 400 envelope:
//
//   - `?limit=` must parse as int64 AND be > 0. Non-numeric, negative, or
//     zero values reject with badField="limit". The per-view ceiling is
//     enforced at read time by MongoViewReader (where the view name is
//     known); the wrapper validates only the wire shape.
//   - `?after=` and `?before=` must decode under the cursor schema
//     (queries.DecodeCursor); a malformed cursor rejects with the matching
//     badField. Tuple-length alignment against `?sort=` happens after the
//     full query loop so both keys can land in any URL order.
//
// `fields` and `sort` are NOT routed here — both are handled inline in
// buildCriteria so they can consume the Response-side projSchema for
// allowlist validation + wire→doc translation. Calling applyReservedParam
// with key="fields" or key="sort" is a no-op (the switch has no matching
// arm).
func applyReservedParam(crit *queries.ReadCriteria, key, val string) (string, bool) {
	switch key {
	case "limit":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n <= 0 {
			return "limit", false
		}
		crit.Limit = n
	case "after":
		if _, err := queries.DecodeCursor(val); err != nil {
			return "after", false
		}
		crit.After = val
	case "before":
		if _, err := queries.DecodeCursor(val); err != nil {
			return "before", false
		}
		crit.Before = val
	case "search":
		crit.Search = val
	case "includeArchived":
		crit.IncludeArchived = val == "true"
	}
	return "", true
}

// applyFilterParam writes a single filter into the criteria map under the
// operator declared on the wire. Empty op maps to equality; the others use
// Mongo-style operator keys ($in, $gte, …) because that is what the canonical
// MongoViewReader consumes verbatim.
//
// Value coercion is driven by spec.goKind (the leaf's declared Go base
// type). A `*string` leaf keeps "95014" as the literal string "95014"; a
// `*int64` leaf coerces "25" into int64(25). This matches the column type
// the read-side adapter stored so `eq` / `in` / `gte` queries hit the
// canonical index without silent type mismatches.
//
// Partial-match operators (`startswith`, `contains`) and case-insensitive
// variants (`ieq`, `ine`, `istartswith`, `icontains`) emit Mongo `$regex`
// sub-documents at field level — every metacharacter in the user-supplied
// value is escaped via regexp.QuoteMeta so the wire input is treated as a
// literal. The list variants (`iin`, `inin`) emit a queries.RegexMatchList
// sentinel because Mongo `$in` requires the native bson.Regex type, which
// MongoViewReader assembles via translateFilter.
//
// When the same field receives more than one operator on the same call
// (e.g. `?name.startswith=Bob&name.icontains=ob`), the clauses are folded
// into a queries.MultiClause sentinel via mergeClause — the canonical
// MongoViewReader expands MultiClause into a top-level `$and` array so
// every declared operator is honored simultaneously instead of having only
// the last one survive on the map.
func applyFilterParam(filter map[string]any, spec filterSpec, op, value string) {
	field := spec.docPath
	var clause any
	switch op {
	case "", OpEq:
		clause = coerceValue(value, spec.goKind)
	case OpIn:
		clause = map[string]any{"$in": coerceList(value, spec.goKind)}
	case OpNin:
		clause = map[string]any{"$nin": coerceList(value, spec.goKind)}
	case OpNe:
		clause = map[string]any{"$ne": coerceValue(value, spec.goKind)}
	case OpGte:
		clause = map[string]any{"$gte": coerceValue(value, spec.goKind)}
	case OpLte:
		clause = map[string]any{"$lte": coerceValue(value, spec.goKind)}
	case OpGt:
		clause = map[string]any{"$gt": coerceValue(value, spec.goKind)}
	case OpLt:
		clause = map[string]any{"$lt": coerceValue(value, spec.goKind)}
	case OpStartsWith:
		clause = map[string]any{"$regex": "^" + regexp.QuoteMeta(value)}
	case OpContains:
		clause = map[string]any{"$regex": regexp.QuoteMeta(value)}
	case OpIEq:
		clause = map[string]any{"$regex": "^" + regexp.QuoteMeta(value) + "$", "$options": "i"}
	case OpINe:
		clause = map[string]any{"$not": map[string]any{"$regex": "^" + regexp.QuoteMeta(value) + "$", "$options": "i"}}
	case OpIStartsWith:
		clause = map[string]any{"$regex": "^" + regexp.QuoteMeta(value), "$options": "i"}
	case OpIContains:
		clause = map[string]any{"$regex": regexp.QuoteMeta(value), "$options": "i"}
	case OpIIn:
		clause = queries.RegexMatchList{Patterns: quoteList(value, true), CaseInsensitive: true}
	case OpINin:
		clause = queries.RegexMatchList{Patterns: quoteList(value, true), CaseInsensitive: true, Negate: true}
	default:
		return
	}
	mergeClause(filter, field, clause)
}

// mergeClause folds a new clause into the criteria map under field. The
// first clause for a field lands as a plain value (scalar for `eq`, the
// operator sub-document for the variants); a second clause for the same
// field promotes both into queries.MultiClause; further clauses append to
// the existing MultiClause. The canonical MongoViewReader expands
// MultiClause into a top-level `$and` array — every declared operator is
// honored simultaneously instead of having only the last write on the map
// survive.
func mergeClause(filter map[string]any, field string, clause any) {
	existing, ok := filter[field]
	if !ok {
		filter[field] = clause
		return
	}
	if mc, isMulti := existing.(queries.MultiClause); isMulti {
		mc.Clauses = append(mc.Clauses, clause)
		filter[field] = mc
		return
	}
	filter[field] = queries.MultiClause{Clauses: []any{existing, clause}}
}

// quoteList splits a comma-separated value and applies regexp.QuoteMeta to
// each entry, optionally wrapping with ^...$ to preserve the equality
// semantic of the `iin` / `inin` operators (each pattern matches the whole
// value, not a substring).
func quoteList(value string, anchored bool) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		q := regexp.QuoteMeta(p)
		if anchored {
			q = "^" + q + "$"
		}
		out = append(out, q)
	}
	return out
}

// parseSortWithSchema turns a comma-separated wire value into a list of
// SortField entries. Each token may carry a `-` prefix (descending);
// otherwise ascending. When projSchema is non-nil, the wire name (without
// the prefix) is validated against the Response DTO's declared paths and
// translated to the corresponding doc path (PascalToSnake by default;
// `view:` override; nested paths walked segment-by-segment). An unknown
// token returns the verbatim wire token (including any `-` prefix) so the
// caller can surface it on the canonical 400 envelope as `sort[<token>]`.
// When projSchema is nil — manual handlers via ParseCriteria, or wrappers
// paired with a RawDoc-style projector that carries no typed Response —
// tokens become SortField entries verbatim (no allowlist, no translation).
func parseSortWithSchema(s string, projSchema *projectionSchema) (sortFields []queries.SortField, badToken string, ok bool) {
	if s == "" {
		return nil, "", true
	}
	tokens := strings.Split(s, ",")
	sortFields = make([]queries.SortField, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		desc := false
		wireName := t
		if strings.HasPrefix(t, "-") {
			desc = true
			wireName = t[1:]
		}
		if projSchema == nil {
			sortFields = append(sortFields, queries.SortField{Field: wireName, Desc: desc})
			continue
		}
		docPath, allowed := projSchema.paths[wireName]
		if !allowed {
			return nil, t, false
		}
		sortFields = append(sortFields, queries.SortField{Field: docPath, Desc: desc})
	}
	return sortFields, "", true
}

// parseProjection turns a comma-separated wire value into a Mongo-shaped
// projection map keyed by doc path (value=1 for inclusion). When projSchema
// is non-nil, each token is validated against the Response DTO's declared
// wire paths and translated to the corresponding doc path (PascalToSnake by
// default; `view:` override; nested paths walked segment-by-segment). An
// unknown token returns (nil, nil, token, false). When projSchema is nil
// (manual handlers via ParseCriteria), tokens become inclusion entries
// verbatim — legacy pass-through.
//
// wireSet returns which wire names appeared in the input; the caller uses
// it to drive the top-level `id` auto-exclusion (the framework adds
// `_id: 0` when `id` is absent from the wire set).
func parseProjection(s string, projSchema *projectionSchema) (proj map[string]int, wireSet map[string]bool, badToken string, ok bool) {
	if s == "" {
		return nil, nil, "", true
	}
	tokens := strings.Split(s, ",")
	proj = make(map[string]int, len(tokens))
	wireSet = make(map[string]bool, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if projSchema == nil {
			proj[t] = 1
			wireSet[t] = true
			continue
		}
		docPath, allowed := projSchema.paths[t]
		if !allowed {
			return nil, nil, t, false
		}
		proj[docPath] = 1
		wireSet[t] = true
	}
	return proj, wireSet, "", true
}

// coerceList splits a comma-separated wire value and coerces each element
// to spec.goKind. Used by the in/nin operators where the wire is one key
// carrying multiple values.
func coerceList(value string, kind reflect.Kind) []any {
	vals := strings.Split(value, ",")
	items := make([]any, len(vals))
	for i, v := range vals {
		items[i] = coerceValue(strings.TrimSpace(v), kind)
	}
	return items
}

// coerceValue converts a wire string into the Go type declared by the leaf.
// String-typed leaves keep the value verbatim (no silent int/float parse —
// "95014" stays "95014" so it matches a string-typed Mongo field). Numeric
// leaves attempt the matching parse; on parse failure the value falls back
// to the string verbatim so the downstream query simply returns zero hits
// instead of crashing the wrapper. The kind is the leaf's base kind after
// pointer stripping (collected in walkSchemaLevel).
func coerceValue(s string, kind reflect.Kind) any {
	switch kind {
	case reflect.String:
		return s
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		return s
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
		return s
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		return s
	case reflect.Bool:
		if s == "true" {
			return true
		}
		if s == "false" {
			return false
		}
		return s
	default:
		// Unknown / composite kinds (slice, struct surrogates) — pass through
		// as string. The walker only stores scalar leaves today, so this
		// branch is defensive.
		return s
	}
}
