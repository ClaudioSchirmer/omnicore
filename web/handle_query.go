package web

import (
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strconv"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/gofiber/fiber/v3"
)

// HasToParamsQuery is the contract for Request DTOs that produce a
// QueryWithParams. The wrapper parses the HTTP query string into a
// ReadCriteria (filters + pagination) and forwards it verbatim to ToQuery.
// The web→application boundary is dumb mapping; *AppContext is consumed by
// the Query's ToCriteria(ctx) downstream, where identity-derived overlays
// (tenant id, owner id) layer onto the wire criteria.
type HasToParamsQuery[TQ queries.QueryWithParams] interface {
	ToQuery(criteria queries.ReadCriteria) TQ
}

// HasToIDQuery is the contract for Request DTOs that produce a QueryByID.
// Only the `includeArchived` reserved query parameter is parsed into the Request
// via Fiber's QueryParser; the rest of the wire state is the path id (injected
// by the wrapper post-ToQuery). The web→application boundary is dumb mapping;
// *AppContext is consumed by the Query's ToCriteria(ctx) downstream.
type HasToIDQuery[TQ queries.QueryByID] interface {
	ToQuery() TQ
}

// QueryWithParams creates a fiber.Handler for paged list endpoints. It
// owns the wire format (query-string parsing, allowlist enforcement, JSON
// envelope with top-level pagination); the application layer stays Fiber-agnostic.
//
// Flow:
//  1. queryschema.ExtractRequestSchema(sample) — inspects TReq's
//     `query:"X" filter:"ops"` tags. Cached by reflect.Type.
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
//	users.Get("/", fwweb.QueryWithParams(d.Pipeline,
//	    requests.FindUsersByParamsRequest{},
//	    requests.FindUsersByParamsResponse{}.FromDoc,
//	    &handlers.FindByParamsQueryHandler[*queries.FindUserByParamsQuery]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
func QueryWithParams[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams, R any](
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
	schema := queryschema.ExtractRequestSchema(reqType)
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
	var projSchema *queryschema.ProjectionSchema
	fieldsOptIn := schema.Reserved["fields"]
	sortOptIn := schema.Reserved["sort"]
	if fieldsOptIn || sortOptIn {
		respType := reflect.TypeOf((*R)(nil)).Elem()
		for respType.Kind() == reflect.Pointer {
			respType = respType.Elem()
		}
		if respType.Kind() == reflect.Struct {
			if fieldsOptIn {
				if errs := queryschema.ValidateFieldsResponse(respType); len(errs) > 0 {
					panic(queryschema.FormatFieldsResponseGuard(respType, errs))
				}
			}
			projSchema = queryschema.ExtractProjectionSchema(respType)
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
		paths := make([]string, 0, len(projSchema.Paths))
		for wirePath := range projSchema.Paths {
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
		appCtx.SetParentIfAbsent(c)
		q := req.ToQuery(crit)
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if result.IsSuccess() {
			return RespondPaged(c, fiber.StatusOK, result.Value(), projector)
		}
		return RespondFromResult(c, result, fiber.StatusOK)
	}
}

// QueryByID creates a fiber.Handler for read-by-id endpoints. The
// only reserved query-string parameter is `?includeArchived=true`; anything
// else produces 400. The path id is injected into the Query via SetPathID
// after ToQuery, mirroring CommandWithBodyID on the write side.
//
// projector is mandatory — pass responses.RawDoc to keep the raw view doc
// shape on the wire, or a consumer-defined R{}.FromDoc to declare a typed
// wire contract:
//
//	users.Get("/:id", fwweb.QueryByID(d.Pipeline,
//	    requests.FindUserByIDRequest{},
//	    requests.FindUserByIDResponse{}.FromDoc,
//	    &handlers.FindByIDQueryHandler[*queries.FindUserByIDQuery]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
func QueryByID[TReq HasToIDQuery[TQ], TQ queries.QueryByID, R any](
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
		panic(formatPathIDConflict("QueryByID", reqType))
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
		appCtx.SetParentIfAbsent(c)
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
// reflection-based schema QueryWithParams uses internally. Returns the
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
// QueryWithParams wrapper enforces, plus wire→doc translation at
// runtime. ParseCriteria stays as the un-typed escape hatch; it does not
// build a projection schema, so a stray `?fields=` token lands in
// pass-through mode (no allowlist, no `_id:0` auto-exclusion) and a stray
// `?sort=` token lands verbatim (no snake_case translation).
//
// Manual handlers that prefer to assemble the criteria differently can ignore
// both helpers and build the ReadCriteria by hand — the framework does not
// require either.
func ParseCriteria(c fiber.Ctx, requestDTO any) (queries.ReadCriteria, string, bool) {
	schema := queryschema.ExtractRequestSchema(reflect.TypeOf(requestDTO))
	// Manual handlers do not declare a typed Response to the wrapper, so
	// the projection schema is nil — `?fields=` works in pass-through mode
	// (each comma-separated token becomes an inclusion entry verbatim; no
	// allowlist, no wire→doc translation, no `_id:0` auto-exclusion). The
	// canonical surface goes through QueryWithParams (or QueryParser
	// in manual mounts), which gates the param behind a typed Response +
	// boot guard.
	return buildCriteria(c, schema, nil)
}

// QueryParser is the typed Mount-time-constructed parser for manual query
// handlers whose Request DTO opts into `?fields=` / `?sort=` AND that
// declare a typed Response. It closes the asymmetry [ParseCriteria] carries
// against the canonical [QueryWithParams] wrapper:
//
//   - The construction (in [NewQueryParser]) runs the exact same boot scan
//     the canonical wrapper runs at lines parallel to its sample-driven
//     reflection: [queryschema.ValidateFieldsResponse] panics on Responses
//     that violate the sparse-render contract (every field at every depth
//     must be *T or a slice/map with `,omitempty`);
//     [queryschema.ExtractProjectionSchema] builds the wire→doc path map; an
//     `slog.Warn` advisory enumerates the sortable wire paths when the
//     Request opts into `?sort=` so the operator can compare them against the
//     Mongo view's declared indexes during the same boot.
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
	schema     *queryschema.RequestSchema
	projSchema *queryschema.ProjectionSchema
}

// NewQueryParser builds a [QueryParser] at Mount time. Runs the boot scan
// detailed on the type doc: schema extraction, fields-side structural
// guard (panic on violation), projection-schema build, sortable-paths
// advisory. Panics on the same condition [QueryWithParams] panics:
// the Request DTO declares `query:"fields"` AND the Response shape
// violates the sparse-render contract.
func NewQueryParser[Req any, Resp any]() *QueryParser[Req, Resp] {
	reqType := reflect.TypeOf((*Req)(nil)).Elem()
	for reqType.Kind() == reflect.Pointer {
		reqType = reqType.Elem()
	}
	schema := queryschema.ExtractRequestSchema(reqType)

	var projSchema *queryschema.ProjectionSchema
	fieldsOptIn := schema.Reserved["fields"]
	sortOptIn := schema.Reserved["sort"]
	if fieldsOptIn || sortOptIn {
		respType := reflect.TypeOf((*Resp)(nil)).Elem()
		for respType.Kind() == reflect.Pointer {
			respType = respType.Elem()
		}
		if respType.Kind() == reflect.Struct {
			if fieldsOptIn {
				if errs := queryschema.ValidateFieldsResponse(respType); len(errs) > 0 {
					panic(queryschema.FormatFieldsResponseGuard(respType, errs))
				}
			}
			projSchema = queryschema.ExtractProjectionSchema(respType)
		}
	}
	if sortOptIn && projSchema != nil {
		paths := make([]string, 0, len(projSchema.Paths))
		for wirePath := range projSchema.Paths {
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
// QueryWithParams should use it to reject unknown query keys uniformly
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

// buildCriteria walks the query string, validates each key against the schema,
// and produces ReadCriteria. Returns (criteria, "", true) on success or
// (zero, badKey, false) on the first violation (unknown wire path OR operator
// outside the declared list for that path).
//
// projSchema is consulted only when the wire carries `?fields=`. When
// non-nil, each comma-separated token is validated against the Response
// DTO's declared wire paths and translated to the corresponding Go field
// path (the reader maps Go → column via the view's TableSchema). An unknown
// token surfaces the bad field on the canonical 400 envelope as `fields[<token>]`. Top-
// level `id` triggers the framework's auto-exclusion: when the consumer
// did NOT request `id`, the projection adds `_id: 0` so Mongo's default
// `_id` inclusion is dropped from the wire shape. When projSchema is nil,
// the legacy pass-through behavior applies: every token becomes an
// inclusion entry verbatim (no allowlist, no translation).
func buildCriteria(c fiber.Ctx, s *queryschema.RequestSchema, projSchema *queryschema.ProjectionSchema) (queries.ReadCriteria, string, bool) {
	crit := queries.ReadCriteria{Filter: map[string]any{}}
	// Pre-scan onlyTotal: VisitAll's iteration order is non-deterministic, so
	// we cannot rely on observing onlyTotal before a conflicting key. Reading
	// it explicitly up front lets the loop reject conflicts on first sight.
	// The flag is only honored when the Request DTO declared `query:"onlyTotal"`
	// (s.Reserved gates opt-in, same posture as `includeArchived` / `search` /
	// `fields` / `sort`).
	onlyTotalOn := s.Reserved["onlyTotal"] && c.Query("onlyTotal") == "true"
	if onlyTotalOn {
		crit.OnlyTotal = true
	}
	var badField string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, v []byte) { //nolint:staticcheck // SA1019: fasthttp VisitAll is deprecated; migrating this hot query-parse path to the All() range-over-func iterator is a mechanical follow-up, out of scope for a lint sweep.
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

		if s.Reserved[key] && reservedParamKeys[key] {
			if key == "onlyTotal" {
				// Already absorbed by the pre-scan; nothing else to apply.
				return
			}
			if key == "fields" {
				proj, wireSet, bad, projOk := queryschema.ParseProjection(val, projSchema)
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
				sortFields, bad, sortOk := queryschema.ParseSortWithSchema(val, projSchema)
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

		wirePath, op := queryschema.ParseKeyAgainstSchema(key, s)
		if wirePath == "" {
			badField = key
			ok = false
			return
		}
		spec := s.Filters[wirePath]

		effective := op
		if effective == "" {
			effective = OpEq
		}
		if !spec.Ops[effective] {
			badField = key
			ok = false
			return
		}
		queryschema.ApplyFilterParam(crit.Filter, spec, op, val)
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

// validateCursorAgainstCriteria decodes the cursor and asserts its STRUCTURE
// against the current wire criteria:
//
//   - decodability: the cursor must parse under the cursor schema.
//   - tuple length: len(K)-1 == len(Sort) (the trailing K element is always
//     _id). Protects against malformed cursors before the reader's keyset
//     builder indexes the tuple.
//
// Either case rejects with 400 SchemaViolationNotification on the cursor's
// wire key. The CONTEXT-HASH check (cursor.H vs the full listing context —
// filter + sort + search + includeArchived) deliberately does NOT run here:
// at this layer the criteria is the WIRE snapshot, BEFORE the Query's
// ToCriteria(ctx) layers identity overlays (tenant, owner, business gates)
// onto it — while the reader stamps outgoing cursors from the POST-ToCriteria
// criteria it received. Comparing the two snapshots rejects every legitimate
// cursor the moment a paged query carries an overlay. The authoritative hash
// check lives in the reader (mongo.MongoViewReader / the composed reader),
// which validates against the same post-ToCriteria context it stamps — a
// mid-navigation context change is still rejected with the same canonical
// 400, on every surface (REST and GraphQL alike), never silently honored.
func validateCursorAgainstCriteria(cursorStr string, crit queries.ReadCriteria, wireKey string) (string, bool) {
	cursor, err := queries.DecodeCursor(cursorStr)
	if err != nil {
		return wireKey, false
	}
	if len(cursor.K)-1 != len(crit.Sort) {
		return wireKey, false
	}
	return "", true
}

// validateByIDQuery enforces the by-id allowlist: only `includeArchived` is
// allowed. Returns ("", true) on a clean query string, (badKey, false) otherwise.
func validateByIDQuery(c fiber.Ctx) (string, bool) {
	var bad string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, _ []byte) { //nolint:staticcheck // SA1019: fasthttp VisitAll deprecated; All() migration deferred.
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
