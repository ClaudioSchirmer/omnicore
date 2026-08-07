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
	// validate and translate `?fields=` AND `?orderBy=` values when the Request
	// DTO opts in to either parameter AND the Response is a struct. Built
	// once and shared between the two reserved keys.
	//
	// The fields-specific boot guard (every field *T + ,omitempty, recursive)
	// only fires when the Request DTO declared `query:"fields"` — orderBy has
	// no analogous structural requirement on the Response (it consumes only
	// the wire→doc path map). When the Response is map[string]any (RawDoc-
	// style projectors), projSchema stays nil and both keys fall back to
	// pass-through mode at the buildCriteria layer.
	var projSchema *queryschema.ProjectionSchema
	fieldsOptIn := schema.Reserved[queryschema.KeyFields]
	orderByOptIn := schema.Reserved[queryschema.KeyOrderBy]
	if fieldsOptIn || orderByOptIn {
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
	// Boot-time advisory when the Request DTO accepts ?orderBy=. The framework
	// has no way to verify, from the wrapper, that the Mongo view declares
	// indexes covering the sortable wire paths — the ViewDefinition lives in
	// a separate construction site (ReadableFeature.Views()). The warning
	// lists every sortable wire path discovered on the Response so the
	// operator can compare it against the view's .Indexes(...) declaration
	// during the same boot.
	if orderByOptIn && projSchema != nil {
		paths := make([]string, 0, len(projSchema.Paths))
		for wirePath := range projSchema.Paths {
			paths = append(paths, wirePath)
		}
		sort.Strings(paths)
		slog.Warn("query.orderBy.opt-in: endpoint accepts ?orderBy=; verify Mongo indexes cover the sortable wire paths to avoid performance degradation on large collections",
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
// only reserved query-string parameter is `?includeArchived=true` — and,
// like every reserved control, it obeys the DTO: the key is honored only
// when the Request DTO declares `query:"includeArchived"`, and rejected as
// the canonical NotDeclared 400 otherwise (never a silent ignore). Anything
// else on the query string produces 400. The path id is injected into the
// Query via SetPathID after ToQuery, mirroring CommandWithBodyID on the
// write side.
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
	includeArchivedOptIn := queryschema.ExtractRequestSchema(reqType).Reserved[queryschema.KeyIncludeArchived]
	return func(c fiber.Ctx) error {
		if bad, ok := validateByIDQuery(c, includeArchivedOptIn); !ok {
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
// `?fields=` / `?orderBy=`. Manual handlers that declare a typed Response AND
// opt into either reserved key should construct a [QueryParser] at Mount
// time instead — that path runs the same boot-time guards (sparse-render
// contract on the Response + sortable-paths advisory) the canonical
// QueryWithParams wrapper enforces, plus wire→doc translation at
// runtime. ParseCriteria stays as the un-typed escape hatch; it does not
// build a projection schema, so a stray `?fields=` token lands in
// pass-through mode (no allowlist, no `_id:0` auto-exclusion) and a stray
// `?orderBy=` token lands verbatim (no snake_case translation).
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
// handlers whose Request DTO opts into `?fields=` / `?orderBy=` AND that
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
//     Request opts into `?orderBy=` so the operator can compare them against the
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
	fieldsOptIn := schema.Reserved[queryschema.KeyFields]
	orderByOptIn := schema.Reserved[queryschema.KeyOrderBy]
	if fieldsOptIn || orderByOptIn {
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
	if orderByOptIn && projSchema != nil {
		paths := make([]string, 0, len(projSchema.Paths))
		for wirePath := range projSchema.Paths {
			paths = append(paths, wirePath)
		}
		sort.Strings(paths)
		slog.Warn("query.orderBy.opt-in: endpoint accepts ?orderBy=; verify Mongo indexes cover the sortable wire paths to avoid performance degradation on large collections",
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
		HasNextPage:     page.HasNextPage,
		HasPreviousPage: page.HasPreviousPage,
		StartCursor:     page.StartCursor,
		EndCursor:       page.EndCursor,
		TotalCount:      page.TotalCount,
	}
}

// RespondPaged emits the canonical paged success envelope: Data carries the
// projected items, Pagination carries the cursor envelope at the top level.
// Convenience wrapper around ProjectPage for the simple case where the
// handler returns a queries.Page directly and the consumer just wants the
// projection + envelope assembled.
//
// When page.OnlyTotal is true, the envelope flips to the only-total shape:
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
			Pagination:  &TotalOnlyPagination{TotalCount: page.TotalCount},
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
	// controls is the canonical snapshot handed to the control gateway after
	// the loop — presence + the values the gate needs. The loop itself owns
	// only WIRE-SHAPE parsing (numbers, cursor decodability, token
	// allowlists); the opt-in gate, the directional rule and the only-total
	// conflict matrix are the gateway's, shared verbatim with GraphQL and
	// gRPC. Recording presence regardless of the DTO's declaration is
	// deliberate: the gateway owns the opt-in verdict.
	var controls queryschema.Controls
	var badField string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, v []byte) { //nolint:staticcheck // SA1019: fasthttp VisitAll is deprecated; migrating this hot query-parse path to the All() range-over-func iterator is a mechanical follow-up, out of scope for a lint sweep.
		if !ok {
			return
		}
		key := string(k)
		val := string(v)

		// A reserved spelling that the DTO declared as a FILTER leaf
		// (`query:"first" filter:"eq"`) keeps its filter meaning — the
		// reserved vocabulary never shadows an explicit declaration. REST
		// speaks the canonical DTO keys verbatim, so the recognized wire
		// set IS queryschema.ControlKeys; controls never carry an operator
		// suffix.
		_, isFilterLeaf := s.Filters[key]
		if queryschema.ControlKeys[key] && !isFilterLeaf {
			switch key {
			case queryschema.KeyOnlyTotal:
				// Presence gates (any value on the wire needs the DTO opt-in,
				// exactly like includeArchived); only `true` ACTIVATES the
				// count short-circuit — `?onlyTotal=false` stays a no-op on a
				// declared endpoint, never a page-shaping conflict.
				active := val == "true"
				controls.OnlyTotal = &active
				crit.OnlyTotal = active
			case queryschema.KeyFirst, queryschema.KeyLast:
				n, err := strconv.ParseInt(val, 10, 64)
				if err != nil {
					badField = key
					ok = false
					return
				}
				if key == queryschema.KeyFirst {
					controls.First = &n
				} else {
					controls.Last = &n
				}
			case queryschema.KeyAfter:
				// Decodability is checked post-gateway (validateCursorAgainstCriteria)
				// so a gateway violation — the more informative rejection — wins over
				// a malformed-cursor one.
				controls.After = true
				crit.After = val
			case queryschema.KeyBefore:
				controls.Before = true
				crit.Before = val
			case queryschema.KeyOrderBy:
				controls.OrderBy = true
				orderBy, bad, obOk := queryschema.ParseOrderByWithSchema(val, projSchema)
				if !obOk {
					badField = "orderBy[" + bad + "]"
					ok = false
					return
				}
				crit.OrderBy = orderBy
			case queryschema.KeyFields:
				controls.Fields = true
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
			case queryschema.KeySearch:
				controls.Search = true
				crit.Search = val
			case queryschema.KeyIncludeArchived:
				controls.IncludeArchived = true
				crit.IncludeArchived = val == "true"
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
	// The canonical control gateway: the DTO opt-in gate, the directional rule
	// (forward first/after × backward last/before) and the only-total conflict
	// matrix — one implementation shared by every surface, run BEFORE the
	// handler. REST has no natural keys (every control has a wire spelling).
	if violations := queryschema.ValidateControls(s.Reserved, controls, nil); len(violations) > 0 {
		return crit, violations[0].Field(), false
	}
	// Materialize the Relay direction pair into the internal size+direction:
	// first=N → forward window of N; last=N → backward window of N (with no
	// cursor, the LAST N of the set).
	if controls.First != nil {
		crit.Limit = *controls.First
	}
	if controls.Last != nil {
		crit.Limit = *controls.Last
		crit.Backward = true
	}
	// Post-loop cursor structure checks — after the gateway, so a directional
	// conflict reports as such before a tuple-length mismatch.
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
//   - tuple length: len(K)-1 == len(OrderBy) (the trailing K element is always
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
	if len(cursor.K)-1 != len(crit.OrderBy) {
		return wireKey, false
	}
	return "", true
}

// validateByIDQuery enforces the by-id allowlist: only `includeArchived` is
// recognized, and only when the endpoint's Request DTO declared it
// (includeArchivedOptIn — the same DTO opt-in gate the list wrappers run
// through the canonical gateway; an undeclared control is a loud 400, never
// a silent ignore). Returns ("", true) on a clean query string,
// (badKey, false) otherwise.
func validateByIDQuery(c fiber.Ctx, includeArchivedOptIn bool) (string, bool) {
	var bad string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, _ []byte) { //nolint:staticcheck // SA1019: fasthttp VisitAll deprecated; All() migration deferred.
		if !ok {
			return
		}
		if string(k) != queryschema.KeyIncludeArchived || !includeArchivedOptIn {
			bad = string(k)
			ok = false
		}
	})
	return bad, ok
}

