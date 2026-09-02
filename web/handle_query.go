package web

import (
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

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
	validateResponseMapping(resultType, respType)
	if respType.Kind() == reflect.Struct {
		if errs := queryschema.ValidateComputedSources(resultType, respType); len(errs) > 0 {
			panic(queryschema.FormatComputedSourcesGuard(resultType, respType, errs))
		}
		if errs := queryschema.ValidateComputedFilters(schema, respType); len(errs) > 0 {
			panic(queryschema.FormatComputedFiltersGuard(reqType, respType, errs))
		}
	}
	// projSchema is the Response-side mapping (wire path → doc path) that
	// validates and translates `?fields=` values. It is a projection concern
	// only: `?fields=` names OUTPUT fields, so the Response is its authority.
	// Ordering speaks the Request's vocabulary and never consults it.
	var projSchema *queryschema.ProjectionSchema
	if schema.Reserved[queryschema.KeyFields] && respType.Kind() == reflect.Struct {
		if errs := queryschema.ValidateFieldsResponse(respType); len(errs) > 0 {
			panic(queryschema.FormatFieldsResponseGuard(respType, errs))
		}
		if errs := queryschema.ValidateFieldsResult(resultType); len(errs) > 0 {
			panic(queryschema.FormatFieldsResultGuard(resultType, errs))
		}
		projSchema = queryschema.ExtractProjectionSchema(respType)
	}
	// Boot-time advisory when the Request DTO declares an ordering vocabulary.
	// The framework has no way to verify, from the wrapper, that the read model
	// behind the endpoint has indexes covering those paths — it cannot even see
	// WHICH read model that is, since the declaration lives in a separate
	// construction site (ReadableFeature.Views() /
	// RelationalReadableFeature.RelationalViews()) — so the operator gets the
	// declared list to compare against the index declarations in the same boot.
	if schema.Reserved[queryschema.KeyOrderBy] {
		warnSortableOnce(reqType, schema.Sortable)
	}
	return schema, projSchema
}

// sortableWarned dedups the sortable-paths advisory. The same Request DTO is
// scanned once per endpoint that serves it (the paged wrapper AND the
// standalone QueryParser, across the REST/GraphQL/gRPC surfaces), so an
// undeduped warn repeated one line per registration — the same advice, the
// same paths, nothing new to act on after the first. Keyed by (Request type +
// the declared path set) so two endpoints that genuinely declare DIFFERENT
// vocabularies still each get their line. Mirrors the warn-once posture
// translation.Render already uses for a missing catalog key.
var sortableWarned = &sync.Map{} // map[string]struct{} keyed by "<reqType>\x1f<paths>"

// warnSortableOnce emits the boot-time advisory the first time a given
// (Request type, declared ordering vocabulary) pair is observed.
//
// It VERIFIES NOTHING and therefore names no store: the wrapper cannot see the
// read model behind the endpoint — the declaration lives in a separate
// construction site (ReadableFeature.Views() /
// RelationalReadableFeature.RelationalViews()) — so it can neither read the
// index declarations nor tell whether the endpoint is served from a projected
// view or straight from the SoR. The advisory is a list to check by hand, and
// the place to check depends on the backing: query.Indexes(...) on a projected
// view, the service's own migrations for a relational read model.
//
// The sort a read model actually receives carries its id as the trailing
// tiebreak, so a covering index is the COMPOUND of the declared path and that
// id — query.Compound("name","_id") on a projected view, an index on
// (name, <id column>) in a migration for a relational one, never a single-key
// index on the path alone, which does not satisfy a two-key sort and leaves a
// blocking sort in place.
func warnSortableOnce(reqType reflect.Type, sortable map[string]queryschema.SortSpec) {
	paths := make([]string, 0, len(sortable))
	for wirePath := range sortable {
		paths = append(paths, wirePath)
	}
	sort.Strings(paths)
	request := reqType.String()
	if _, loaded := sortableWarned.LoadOrStore(request+"\x1f"+strings.Join(paths, ","), struct{}{}); loaded {
		return
	}
	slog.Warn("query.sortable: endpoint declares an ordering vocabulary; verify the read model's indexes cover each path COMPOUNDED WITH its id tiebreak to avoid blocking sorts on large data sets",
		"request", request,
		"sortable_wire_paths", paths)
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
	queryschema.RecordSearchDeclaration(schema, reqType.String(), h)
	return func(c fiber.Ctx) error {
		crit, selectedWire, violation, ok := readCriteria(c, schema, projSchema)
		if !ok {
			return respondViolation[queries.PageOf[TResult]](c, pipe, violation)
		}
		// Sources read only to feed a selected computed field are blanked before
		// projection, so `?fields=` shapes the wire even when a source shares it.
		hidden := queryschema.UnrequestedComputedSources(projSchema, selectedWire)
		var req TReq
		if v := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); v != nil {
			return respondViolation[queries.PageOf[TResult]](c, pipe, v)
		}
		appCtx := AppContext(c)
		appCtx.SetParentIfAbsent(c)
		q := req.ToQuery(crit)
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		if result.IsSuccess() {
			return RespondPaged(c, fiber.StatusOK, result.Value(), func(r TResult) TResp {
				return responseProjection(queryschema.BlankResultPaths(r, hidden))
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
	validateResponseMapping(resultType, respType)
	schema := queryschema.ExtractRequestSchema(reqType)
	return func(c fiber.Ctx) error {
		if bad, ok := validateByIDQuery(c); !ok {
			return respondSchemaViolation[TResult](c, pipe, bad)
		}
		var req TReq
		if err := c.Bind().Query(&req); err != nil {
			return respondSchemaViolation[TResult](c, pipe, "includeArchived")
		}
		if v := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); v != nil {
			return respondViolation[TResult](c, pipe, v)
		}
		// The `:id` segment is refused HERE or nowhere: domain.ID is an opaque
		// wrapper, so a non-uuid segment travels intact to the reader and
		// surfaces as a driver error (a 500) on a relational backing, while a
		// Mongo-backed view of the same route answers 404 because the string
		// simply matches no _id. Refusing at the wire is what makes the two
		// postures answer one contract.
		if rawID := c.Params("id"); queryschema.IsMalformedPathID(rawID) {
			return respondViolation[TResult](c, pipe, queryschema.UnknownPathIDAddress(queryschema.KeyPathID, rawID))
		}
		// One reserved control is this route's whole wire vocabulary, but it
		// goes through the SAME assembler a listing does, so the opt-in gate
		// answers identically on both.
		//
		// The value is read off the WIRE, not off the bound DTO: the strict
		// spelling is the contract, and Fiber's binder would also take
		// "1"/"t"/"TRUE", which every other connector rejects. PRESENCE is the
		// key being on the query string, not the value being non-empty —
		// `?includeArchived=` is the control asked for with no answer, and
		// reading that as "absent" would rebuild, on the empty string, the
		// list-vs-by-id disagreement the strict parsing removed.
		var in queryschema.Read
		if args := c.Request().URI().QueryArgs(); args.Has(queryschema.KeyIncludeArchived) {
			archived, valid := queryschema.ParseControlBool(string(args.Peek(queryschema.KeyIncludeArchived)))
			if !valid {
				return respondSchemaViolation[TResult](c, pipe, queryschema.KeyIncludeArchived)
			}
			in = queryschema.ByIDRead(archived, true)
		}
		crit, _, violation, ok := queryschema.BuildCriteria(schema, nil, in)
		if !ok {
			return respondViolation[TResult](c, pipe, violation)
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
	if schema.Reserved[queryschema.KeyFields] {
		respType := reflect.TypeOf((*Resp)(nil)).Elem()
		for respType.Kind() == reflect.Pointer {
			respType = respType.Elem()
		}
		if respType.Kind() == reflect.Struct {
			if errs := queryschema.ValidateFieldsResponse(respType); len(errs) > 0 {
				panic(queryschema.FormatFieldsResponseGuard(respType, errs))
			}
			projSchema = queryschema.ExtractProjectionSchema(respType)
		}
	}
	if schema.Reserved[queryschema.KeyOrderBy] {
		warnSortableOnce(reqType, schema.Sortable)
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
	crit, _, violation, ok := readCriteria(c, p.schema, p.projSchema)
	return crit, violation, ok
}

// RespondViolation emits the canonical envelope for a typed rejection the
// caller assembled — from [QueryParser.Parse] on the read controls, or from
// [BindPath] on the path segments. The violation carries both the wire
// spelling of the offending field and the notification explaining it, so a
// manual handler renders the SAME message the auto wrapper does (ordering by a
// computed field, say, instead of a generic schema error).
//
// The STATUS comes from that notification, not from this call: a read-control
// rejection is the canonical 400, while a malformed identity segment answers
// 404 on a read and 400 on a write. A manual handler therefore never writes a
// status literal for a refusal.
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

// readCriteria is this surface's whole read path: decode the query string into
// the neutral request, then let the shared assembler decide what it means.
// The two steps are the seam every surface has — only the first one is REST's.
func readCriteria(c fiber.Ctx, s *queryschema.RequestSchema, proj *queryschema.ProjectionSchema) (queries.ReadCriteria, map[string]bool, *queryschema.Violation, bool) {
	in, violation, ok := decodeQuery(c, s, nil)
	if !ok {
		return queries.ReadCriteria{}, nil, violation, false
	}
	return queryschema.BuildCriteria(s, proj, in)
}

// decodeQuery turns a Fiber query string into the surface-neutral
// [queryschema.Read] — the ONE thing this surface owns: how a URL spells a
// value. A comma separates a list, an operator rides on the key after a dot,
// a boolean is exactly "true" or "false", a size is base-10.
//
// It decides nothing. Whether a key is accepted, whether an operator is
// declared for it, whether the endpoint takes the control at all — all of that
// is the Request DTO's answer, applied once in [queryschema.BuildCriteria].
// Presence is recorded for every control the wire carried, declared or not:
// the gate owns that verdict, and reporting it there is what makes the refusal
// identical on every surface.
//
// ignored names the control keys this route accepts and does nothing with —
// the export's pagination no-ops, documented as omitted OpenAPI parameters. A
// key in that set is not read at all, so its VALUE is not judged either: a
// documented no-op that refuses a malformed value would be a strange contract.
func decodeQuery(c fiber.Ctx, s *queryschema.RequestSchema, ignored map[string]bool) (queryschema.Read, *queryschema.Violation, bool) {
	var in queryschema.Read
	var violation *queryschema.Violation
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, v []byte) { //nolint:staticcheck // SA1019: fasthttp VisitAll is deprecated; migrating this hot query-parse path to the All() range-over-func iterator is a mechanical follow-up, out of scope for a lint sweep.
		if !ok {
			return
		}
		key, val := string(k), string(v)
		reject := func() {
			violation = queryschema.SchemaViolation(key)
			ok = false
		}

		// A reserved spelling the DTO declared as a FILTER leaf
		// (`query:"first" filter:"eq"`) keeps its filter meaning: the reserved
		// vocabulary never shadows an explicit declaration.
		if _, isFilterLeaf := s.Filters[key]; queryschema.ControlKeys[key] && !isFilterLeaf {
			if ignored[key] {
				return
			}
			switch key {
			case queryschema.KeyFirst, queryschema.KeyLast:
				n, err := strconv.ParseInt(val, 10, 64)
				if err != nil {
					reject()
					return
				}
				if key == queryschema.KeyFirst {
					in.Controls.First = &n
				} else {
					in.Controls.Last = &n
				}
			case queryschema.KeyAfter:
				in.Controls.After, in.After = true, val
			case queryschema.KeyBefore:
				in.Controls.Before, in.Before = true, val
			case queryschema.KeyOrderBy:
				in.Controls.OrderBy = true
				in.OrderBy = append(in.OrderBy, decodeOrderTokens(val)...)
			case queryschema.KeyFields:
				in.Controls.Fields = true
				in.Fields = append(in.Fields, splitList(val)...)
			case queryschema.KeySearch:
				in.Controls.Search, in.Search = true, val
			case queryschema.KeyIncludeArchived:
				archived, valid := queryschema.ParseControlBool(val)
				if !valid {
					reject()
					return
				}
				in.Controls.IncludeArchived, in.IncludeArchived = true, archived
			case queryschema.KeyOnlyTotal:
				// Presence gates like every control; only `true` ACTIVATES the
				// count short-circuit, so `?onlyTotal=false` on a declared
				// endpoint is a no-op, never a page-shaping conflict.
				active, valid := queryschema.ParseControlBool(val)
				if !valid {
					reject()
					return
				}
				in.Controls.OnlyTotal = &active
			}
			return
		}

		path, op := queryschema.ParseKeyAgainstSchema(key, s)
		if path == "" {
			reject()
			return
		}
		// A query string packs a list with commas — this wire's spelling, and
		// the only reason the decoder looks at the operator at all. A scalar
		// operand rides whole, empty included: `?name.contains=` is a present
		// operand, not an absent one.
		values := []string{val}
		if queryschema.OperatorTakesList(op) {
			values = splitList(val)
		}
		in.Filters = append(in.Filters, queryschema.FilterTerm{
			Path: path, Op: op, Values: values, Raw: key,
		})
	})
	return in, violation, ok
}

// decodeOrderTokens splits an `?orderBy=` value into terms. A `-` prefix is
// this wire's spelling for descending; the token rides along verbatim so a
// refusal names exactly what the consumer sent, prefix included.
func decodeOrderTokens(val string) []queryschema.OrderTerm {
	var out []queryschema.OrderTerm
	for _, token := range splitList(val) {
		term := queryschema.OrderTerm{Path: token, Raw: token}
		if strings.HasPrefix(token, "-") {
			term.Desc, term.Path = true, token[1:]
		}
		out = append(out, term)
	}
	return out
}

// splitList splits a comma-separated wire value, trimming each entry and
// dropping empties.
func splitList(val string) []string {
	if val == "" {
		return nil
	}
	parts := strings.Split(val, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validateByIDQuery enforces the by-id allowlist: `includeArchived` is the only
// key this route recognizes at all. WHETHER the endpoint accepts it is not
// decided here — that is the DTO opt-in gate's answer, applied by the shared
// assembler like it is on a listing. Returns ("", true) on a clean query
// string, (badKey, false) otherwise.
func validateByIDQuery(c fiber.Ctx) (string, bool) {
	var bad string
	ok := true
	c.Request().URI().QueryArgs().VisitAll(func(k, _ []byte) { //nolint:staticcheck // SA1019: fasthttp VisitAll deprecated; All() migration deferred.
		if !ok {
			return
		}
		if string(k) != queryschema.KeyIncludeArchived {
			bad = string(k)
			ok = false
		}
	})
	return bad, ok
}
