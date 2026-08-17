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
type HasToParamsQuery[TQ any] interface {
	ToQuery(criteria queries.ReadCriteria) TQ
}

// HasToIDQuery is the contract for Request DTOs that produce a QueryByID —
// the same shape [HasToParamsQuery] has, over the same ReadCriteria. The
// difference is the wire vocabulary behind it, not the seat: a by-id read
// accepts exactly one reserved control (`includeArchived`), so the criteria
// the wrapper builds carries that and nothing else, where the paged wrapper
// fills the full set (filters + pagination). The rest of the wire state is
// the path id (injected by the wrapper post-ToQuery). The web→application
// boundary is dumb mapping; *AppContext is consumed by the Query's
// ToCriteria(ctx) downstream.
type HasToIDQuery[TQ any] interface {
	ToQuery(criteria queries.ReadCriteria) TQ
}

// queryBootScan runs the shared mount-time reflection for the read-side
// constructors: Request schema extraction, the Result↔Response alignment
// guard (the generic TResult→TResp mapper is name-based, so a Response
// field with no same-named Result backing is a boot panic), the sparse
// contract on BOTH shapes when the Request opts into `?fields=`, the
// projection schema build and the sortable-paths advisory.
func queryBootScan(reqType, resultType, respType reflect.Type) (*queryschema.RequestSchema, *queryschema.ProjectionSchema) {
	schema := queryschema.ExtractRequestSchema(reqType)
	for respType.Kind() == reflect.Pointer {
		respType = respType.Elem()
	}
	for resultType.Kind() == reflect.Pointer {
		resultType = resultType.Elem()
	}
	if respType.Kind() == reflect.Struct {
		if errs := queryschema.ValidateResultAlignment(resultType, respType); len(errs) > 0 {
			panic(queryschema.FormatResultAlignmentGuard(resultType, respType, errs))
		}
		if errs := queryschema.ValidateComputedSources(resultType, respType); len(errs) > 0 {
			panic(queryschema.FormatComputedSourcesGuard(resultType, respType, errs))
		}
		if errs := queryschema.ValidateComputedFilters(schema, respType); len(errs) > 0 {
			panic(queryschema.FormatComputedFiltersGuard(reqType, respType, errs))
		}
	}
	warnMappingFallback(reqType, resultType, respType)
	// projSchema is the Response-side mapping (wire path → doc path) used to
	// validate and translate `?fields=` AND `?orderBy=` values when the Request
	// DTO opts in to either parameter. Built once and shared between the two
	// reserved keys. The Response's wire tokens translate to Go field paths
	// that are — by the alignment guard above — the Result's field names,
	// which are the canonical document's keys.
	var projSchema *queryschema.ProjectionSchema
	fieldsOptIn := schema.Reserved[queryschema.KeyFields]
	orderByOptIn := schema.Reserved[queryschema.KeyOrderBy]
	if (fieldsOptIn || orderByOptIn) && respType.Kind() == reflect.Struct {
		if fieldsOptIn {
			if errs := queryschema.ValidateFieldsResponse(respType); len(errs) > 0 {
				panic(queryschema.FormatFieldsResponseGuard(respType, errs))
			}
			if errs := queryschema.ValidateFieldsResult(resultType); len(errs) > 0 {
				panic(queryschema.FormatFieldsResultGuard(resultType, errs))
			}
		}
		projSchema = queryschema.ExtractProjectionSchema(respType)
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
	return schema, projSchema
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
//  5. Dispatch via Pipeline. The handler returns a typed PageOf[TResult] —
//     the application filled each document into a Result and ran the
//     Query's FromQueryResult hook; the web layer never sees a raw document.
//  6. On Success — map each item via responseProjection (typically the
//     Response's FromResult method), emit Data=[]TResp + top-level
//     Pagination. On Failure — RespondFromResult standard path (translates
//     notifications).
//
// HTTP semantics:
//   - 200 → page rendered
//   - 400 → query-string key outside allowlist OR operator outside declared list
//   - other 4xx → custom semantics via Notification.Semantic()
//
// responseProjection is mandatory — the wire mapping seat, mirroring the
// command wrappers. The trivial implementation is the Response's FromResult
// delegating to the generic name-based mapper:
//
//	users.Get("/", fwweb.QueryWithParams(d.Pipeline,
//	    requests.FindUsersByParamsRequest{},
//	    requests.FindUsersByParamsResponse{}.FromResult,
//	    &handlers.FindByParamsQueryHandler[*queries.FindUserByParamsQuery, appqueries.FindUsersResult]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
func QueryWithParams[TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams[TResult], TResult any, TResp any](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TQ, queries.PageOf[TResult]],
) fiber.Handler {
	_ = sample
	reqType := reflect.TypeOf(sample)
	if reqType.Kind() == reflect.Pointer {
		reqType = reqType.Elem()
	}
	pathSchema := inspectPathTags(reqType)
	schema, projSchema := queryBootScan(
		reqType,
		reflect.TypeOf((*TResult)(nil)).Elem(),
		reflect.TypeOf((*TResp)(nil)).Elem(),
	)
	return func(c fiber.Ctx) error {
		crit, selectedWire, violation, ok := buildCriteria(c, schema, projSchema)
		if !ok {
			return respondViolation[queries.PageOf[TResult]](c, pipe, violation)
		}
		// Sources read only to feed a selected computed field are blanked before
		// projection, so `?fields=` shapes the wire even when a source shares it.
		hidden := queryschema.UnrequestedComputedSources(projSchema, selectedWire)
		var req TReq
		if bad, ok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !ok {
			return respondSchemaViolation[queries.PageOf[TResult]](c, pipe, bad)
		}
		appCtx := AppContext(c)
		appCtx.SetParentIfAbsent(c)
		q := req.ToQuery(crit)
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if result.IsSuccess() {
			return RespondPaged(c, fiber.StatusOK, result.Value(), func(r TResult) TResp {
				return responseProjection(blankResultPaths(r, hidden))
			})
		}
		return RespondFromResult(c, result, fiber.StatusOK)
	}
}

// QueryByID creates a fiber.Handler for read-by-id endpoints. The
// only reserved query-string parameter is `?includeArchived=true` — and,
// like every reserved control, it obeys the DTO: the key is honored only
// when the Request DTO declares `query:"includeArchived"`, and rejected as
// the canonical NotDeclared 400 otherwise (never a silent ignore). Anything
// else on the query string produces 400. The parsed control travels to the
// Query the same way the paged wrapper's full control set does — inside the
// ReadCriteria handed to ToQuery(criteria) — so both read shapes have one
// seat, and the by-id Query returns it from ToCriteria(ctx) after layering
// its identity-derived overlays. The path id is injected into the Query via
// SetPathID after ToQuery, mirroring CommandWithBodyID on the write side.
//
// responseProjection is mandatory — the wire mapping seat, mirroring the
// command wrappers:
//
//	users.Get("/:id", fwweb.QueryByID(d.Pipeline,
//	    requests.FindUserByIDRequest{},
//	    requests.FindUserByIDResponse{}.FromResult,
//	    &handlers.FindByIDQueryHandler[*queries.FindUserByIDQuery, appqueries.FindUserResult]{
//	        Reader: d.ViewReader, View: view.Name(),
//	    }))
func QueryByID[TReq HasToIDQuery[TQ], TQ queries.QueryByID[TResult], TResult any, TResp any](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TQ, TResult],
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
	resultType := reflect.TypeOf((*TResult)(nil)).Elem()
	respType := reflect.TypeOf((*TResp)(nil)).Elem()
	for respType.Kind() == reflect.Pointer {
		respType = respType.Elem()
	}
	for resultType.Kind() == reflect.Pointer {
		resultType = resultType.Elem()
	}
	if respType.Kind() == reflect.Struct {
		if errs := queryschema.ValidateResultAlignment(resultType, respType); len(errs) > 0 {
			panic(queryschema.FormatResultAlignmentGuard(resultType, respType, errs))
		}
	}
	warnMappingFallback(reqType, resultType, respType)
	includeArchivedOptIn := queryschema.ExtractRequestSchema(reqType).Reserved[queryschema.KeyIncludeArchived]
	return func(c fiber.Ctx) error {
		if bad, ok := validateByIDQuery(c, includeArchivedOptIn); !ok {
			return respondSchemaViolation[TResult](c, pipe, bad)
		}
		var req TReq
		if err := c.Bind().Query(&req); err != nil {
			return respondSchemaViolation[TResult](c, pipe, "includeArchived")
		}
		if bad, ok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !ok {
			return respondSchemaViolation[TResult](c, pipe, bad)
		}
		// The by-id twin of buildCriteria: one reserved control is the whole
		// wire vocabulary here, so the criteria is built inline and handed to
		// the same ToQuery(criteria) seat the paged wrapper feeds. Reading it
		// back off the bound DTO keeps the accepted spellings exactly Fiber's.
		crit := queries.ReadCriteria{Filter: map[string]any{}}
		if includeArchivedOptIn {
			crit.IncludeArchived = queryschema.ReadIncludeArchived(reflect.ValueOf(&req))
		}
		appCtx := AppContext(c)
		appCtx.SetParentIfAbsent(c)
		q := req.ToQuery(crit)
		q.SetPathID(c.Params("id"))
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if result.IsSuccess() {
			return RespondWithSuccess(c, fiber.StatusOK, responseProjection(result.Value()))
		}
		return RespondFromResult(c, result, fiber.StatusOK)
	}
}

// QueryParser is the typed Mount-time-constructed parser for manual query
// handlers. It runs the same boot scan the canonical wrappers run:
//
//   - The construction (in [NewQueryParser]) mirrors the canonical
//     wrapper's sample-driven reflection: [queryschema.ValidateFieldsResponse]
//     panics on Responses that violate the sparse-render contract (every
//     field at every depth must be *T or a slice/map with `,omitempty`);
//     [queryschema.ExtractProjectionSchema] builds the wire→doc path map; an
//     `slog.Warn` advisory enumerates the sortable wire paths when the
//     Request opts into `?orderBy=` so the operator can compare them against the
//     Mongo view's declared indexes during the same boot.
//   - The [QueryParser.Parse] call walks the request query string against
//     the cached schema + projection — runtime allowlist + wire→doc
//     translation are enabled when applicable, so a `?fields=addresses.zipCode`
//     token translates to the Go field path and an unknown
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
// captured at construction. Returns (criteria, "", true) on success or
// (zero, badKey, false) on the first violation. Forward badKey to
// [RespondSchemaViolation] to emit the canonical 400 envelope:
//
//	crit, badField, ok := parser.Parse(c)
//	if !ok {
//	    return fwweb.RespondSchemaViolation(c, pipe, badField)
//	}
func (p *QueryParser[Req, Resp]) Parse(c fiber.Ctx) (queries.ReadCriteria, *queryschema.Violation, bool) {
	crit, _, violation, ok := buildCriteria(c, p.schema, p.projSchema)
	return crit, violation, ok
}

// RespondViolation emits the canonical 400 envelope for a typed read-control
// rejection produced by [QueryParser.Parse] — the violation carries both the
// wire spelling of the offending field and the notification explaining it, so
// a manual query handler renders the SAME message the auto wrapper does
// (ordering by a computed field, say, instead of a generic schema error):
//
//	crit, violation, ok := parser.Parse(c)
//	if !ok {
//	    return fwweb.RespondViolation(c, pipe, violation)
//	}
func RespondViolation(c fiber.Ctx, pipe *pipeline.Pipeline, v *queryschema.Violation) error {
	return respondViolation[any](c, pipe, v)
}

// RespondSchemaViolation emits the canonical 400 envelope carrying
// SchemaViolationNotification (semantic Schema, context "Schema") for the
// given bad field. Manual query handlers that opt out of
// QueryWithParams should use it to reject unknown query keys uniformly
// with the wrapper:
//
//	crit, badField, ok := parser.Parse(c)
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

// ProjectPage walks page.Items applying the response projection per item
// and returns the projected items + a PaginationInfo ready to be placed on
// Response.Pagination. Used by RespondPaged and by manual paged handlers
// that want to assemble the envelope by hand.
//
// When page.OnlyTotal is true, ProjectPage is the wrong primitive — there
// are no items to project and the pagination shape is dedicated. Callers
// should branch on page.OnlyTotal themselves (or use RespondPaged, which
// branches internally).
func ProjectPage[TResult any, TResp any](page queries.PageOf[TResult], project func(TResult) TResp) ([]TResp, *PaginationInfo) {
	items := make([]TResp, len(page.Items))
	for i, r := range page.Items {
		items[i] = project(r)
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
// handler returns a queries.PageOf directly and the consumer just wants the
// projection + envelope assembled.
//
// When page.OnlyTotal is true, the envelope flips to the only-total shape:
// Data is omitted entirely and Pagination is a TotalOnlyPagination carrying
// solely Total. The listing-only fields (has_next/has_prev/cursors) are not
// emitted — they would carry zero-value noise that misleads consumers in
// count mode.
func RespondPaged[TResult any, TResp any](c fiber.Ctx, status int, page queries.PageOf[TResult], project func(TResult) TResp) error {
	if page.OnlyTotal {
		return Respond(c, Response{
			Success:     true,
			Status:      status,
			Description: http.StatusText(status),
			Pagination:  &TotalOnlyPagination{TotalCount: page.TotalCount},
		})
	}
	items, pagination := ProjectPage(page, project)
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
// the pass-through behavior applies: every token becomes an
// inclusion entry verbatim (no allowlist, no translation).
func buildCriteria(c fiber.Ctx, s *queryschema.RequestSchema, projSchema *queryschema.ProjectionSchema) (queries.ReadCriteria, map[string]bool, *queryschema.Violation, bool) {
	crit := queries.ReadCriteria{Filter: map[string]any{}}
	// controls is the canonical snapshot handed to the control gateway after
	// the loop — presence + the values the gate needs. The loop itself owns
	// only WIRE-SHAPE parsing (numbers, cursor decodability, token
	// allowlists); the opt-in gate, the directional rule and the only-total
	// conflict matrix are the gateway's, shared verbatim with GraphQL and
	// gRPC. Recording presence regardless of the DTO's declaration is
	// deliberate: the gateway owns the opt-in verdict.
	var controls queryschema.Controls
	var violation *queryschema.Violation
	// The wire paths the consumer selected via ?fields=, kept so the render can
	// blank sources read only to feed a selected computed field.
	var selectedWire map[string]bool
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
					violation = queryschema.SchemaViolation(key)
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
				orderBy, obViolation, obOk := queryschema.ParseOrderByWithSchema(val, projSchema)
				if !obOk {
					violation = obViolation
					ok = false
					return
				}
				crit.OrderBy = orderBy
			case queryschema.KeyFields:
				controls.Fields = true
				proj, wireSet, bad, projOk := queryschema.ParseProjection(val, projSchema)
				if !projOk {
					violation = queryschema.SchemaViolation(queryschema.FieldsField(bad))
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
				selectedWire = wireSet
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
			violation = queryschema.SchemaViolation(key)
			ok = false
			return
		}
		spec := s.Filters[wirePath]

		effective := op
		if effective == "" {
			effective = OpEq
		}
		if !spec.Ops[effective] {
			violation = queryschema.SchemaViolation(key)
			ok = false
			return
		}
		queryschema.ApplyFilterParam(crit.Filter, spec, op, val)
	})
	if !ok {
		return crit, nil, violation, ok
	}
	// The canonical control gateway: the DTO opt-in gate, the directional rule
	// (forward first/after × backward last/before) and the only-total conflict
	// matrix — one implementation shared by every surface, run BEFORE the
	// handler. REST has no natural keys (every control has a wire spelling).
	if violations := queryschema.ValidateControls(s.Reserved, controls, nil); len(violations) > 0 {
		return crit, nil, &queryschema.Violation{Field: violations[0].Field(), Notification: violations[0].Message().Notification}, false
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
			return crit, nil, queryschema.SchemaViolation(bad), false
		}
	}
	if crit.Before != "" {
		if bad, cursorOk := validateCursorAgainstCriteria(crit.Before, crit, "before"); !cursorOk {
			return crit, nil, queryschema.SchemaViolation(bad), false
		}
	}
	return crit, selectedWire, nil, true
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
