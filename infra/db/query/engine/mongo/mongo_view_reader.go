package mongo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// FrameworkDefaultMaxReadLimit is the per-page ceiling the reader applies
// when no override is configured. Matches bootstrap.FrameworkDefaultMaxLimit
// (kept duplicated here so the infra package does not import bootstrap —
// dependency rule keeps infra → application/persistence only). The reader's
// resolver consults: per-view (query.ViewDefinition.MaxLimit) > yaml-supplied
// resolver value > this constant.
const FrameworkDefaultMaxReadLimit int64 = 100

// MongoViewReader adapts MongoDB to the application's queries.ViewReader port.
// It is the only place in the codebase that translates plain ReadCriteria
// into bson and converts bson.M back into map[string]any.
//
// Per-view max-limit resolution is hooked via SetMaxLimitResolver — bootstrap
// builds the closure from the collected ViewDefinitions plus the yaml-supplied
// default. When the resolver is nil (manual construction in tests / custom
// adapters), every view falls back to FrameworkDefaultMaxReadLimit.
type MongoViewReader struct {
	mongo *MongoDB
	// resolver maps a logical view name to the physical collection currently
	// serving its reads (its active slot). Shared process-wide; the read path
	// resolves through it so a rebuild flip is observed here without changing
	// the ViewReader interface (which speaks logical names, per the layer rules).
	resolver   *query.ViewResolver
	maxLimitFn func(view string) int64
	viewNodes  map[string]*query.ViewNode
	// composed handles the read-time composed views (SetComposedViews). A read
	// against a composed name is delegated to it; every other name follows the
	// regular path. Installed by MUTATION (like SetViews) so every handler that
	// captured this reader before bootstrap wiring finished — GraphQL fields
	// registered inside the consumer's Wire(), for instance — still resolves
	// composed names.
	composed *ComposedViewReader
}

func NewMongoViewReader(m *MongoDB, resolver *query.ViewResolver) *MongoViewReader {
	return &MongoViewReader{mongo: m, resolver: resolver}
}

// SetViews registers the collected ViewDefinitions so the reader can translate
// criteria/documents between Go field paths and physical columns via each view's
// TableSchema tree. Bootstrap calls this with the views aggregated from every
// ReadableFeature. A view absent from the map (or a view declared without a
// schema) falls back to identity translation (keys are treated as physical doc
// columns) so schema-less views and tests keep working.
func (r *MongoViewReader) SetViews(views []*query.ViewDefinition) *MongoViewReader {
	r.viewNodes = make(map[string]*query.ViewNode, len(views))
	for _, v := range views {
		r.viewNodes[v.Name()] = v.BuildViewNode()
	}
	return r
}

// resolveViewSchema returns the translator for view, or an empty (identity)
// node when none is registered.
func (r *MongoViewReader) resolveViewSchema(view string) *query.ViewNode {
	if r.viewNodes != nil {
		if n := r.viewNodes[view]; n != nil {
			return n
		}
	}
	return &query.ViewNode{}
}

// translateFilterKeys rewrites a Go-field-path-keyed filter into a
// physical-column-keyed filter via the view node. A key the view cannot resolve
// is a schema mismatch, not something to guess at — it aborts the read with the
// canonical 400 (see translateDotted).
func translateFilterKeys(node *query.ViewNode, src map[string]any) (map[string]any, error) {
	if len(src) == 0 {
		return src, nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		col, err := translateDotted(node, k)
		if err != nil {
			return nil, err
		}
		out[col] = v
	}
	return out, nil
}

// translateSortFields rewrites Go-field-path sort terms into physical columns.
func translateSortFields(node *query.ViewNode, src []queries.OrderByField) ([]queries.OrderByField, error) {
	if len(src) == 0 {
		return src, nil
	}
	out := make([]queries.OrderByField, len(src))
	for i, s := range src {
		col, err := translateDotted(node, s.Field)
		if err != nil {
			return nil, err
		}
		out[i] = queries.OrderByField{Field: col, Desc: s.Desc}
	}
	return out, nil
}

// translateProjectionKeys rewrites a Go-field-path projection into physical
// columns. The reserved "_id" key passes through untranslated.
// normalizeIdentity settles the store's own identity key before the document
// leaves this reader. `_id` is Mongo's spelling and it stops here: a document
// whose schema maps a physical id column already carries "ID" (ToGoDoc
// translated it), but a mirror of an upstream collection is stored under `_id`
// alone, and no schema column maps it. Lifting it onto the Go field "ID" here is
// what lets every layer above speak one identity vocabulary — the application
// layer used to carry this fallback, which meant it knew a store's key name.
//
// `kept` is whether the consumer's selection keeps the identity at all, and it
// GATES both halves. The promotion used to be unconditional, and that is how
// ReadCriteria.Restrict("ID") — an AUTHORITY, whose contract is that the field
// reaches neither the store nor the wire — was silently defeated on every
// Mongo-backed view: the exclusion did remove the schema's id COLUMN from the
// projection, but Mongo returns `_id` on every document regardless, and this
// function then lifted it straight back onto "ID". The restricted field was
// served, spelled exactly as the DTO fills it. The store key leaves with it:
// nothing above this line asked for an identity, under either spelling.
func normalizeIdentity(doc map[string]any, kept bool) map[string]any {
	if doc == nil {
		return doc
	}
	if !kept {
		delete(doc, "_id")
		delete(doc, idGoField)
		return doc
	}
	if _, has := doc[idGoField]; has {
		return doc
	}
	if v, ok := doc["_id"].(string); ok {
		doc[idGoField] = v
	}
	return doc
}

// idGoField is the Go field name the root identity resolves under — fixed by the
// Entity contract, and the path a selection names when it wants the id.
const idGoField = "ID"

func translateProjectionKeys(node *query.ViewNode, src queries.Projection) (map[string]int, error) {
	if !src.Narrows() {
		return nil, nil
	}
	flag := 0
	if src.IsInclusion() {
		flag = 1
	}
	out := make(map[string]int, len(src.Paths)+1)
	for path := range src.Paths {
		col, err := translateDotted(node, path)
		if err != nil {
			return nil, err
		}
		out[col] = flag
	}
	// `_id` is Mongo's, and it stops here. The driver returns it on every
	// inclusion projection unless it is explicitly excluded, so a selection that
	// did not name the identity has to say so — in the store's own vocabulary,
	// inside the store's own reader. Above this line the identity is just the Go
	// path "ID", selected or not like any other.
	if src.IsInclusion() && !src.Selects(idGoField) {
		out["_id"] = 0
	}
	return out, nil
}

// translateDotted translates a dotted Go field path into the dotted physical
// column path.
//
// A node with NO schema is an unregistered view name — it translates nothing
// and every path passes through unchanged, exactly as before (ColumnPath's own
// contract). A node WITH a schema that cannot resolve the path is a different
// animal: the caller named a field this view does not have. Passing that
// through to Mongo verbatim is what made a mistyped filter return an empty page
// with a 200 and a mistyped sort do nothing at all — so it now fails with the
// canonical Schema 400 naming the offending path.
func translateDotted(node *query.ViewNode, dotted string) (string, error) {
	parts := strings.Split(dotted, ".")
	col, ok := node.ColumnPath(parts)
	if !ok {
		return "", core.UnresolvedFieldPathError(dotted)
	}
	return strings.Join(col, "."), nil
}

// SetComposedViews registers the boot-validated composed-view definitions so
// reads against a composed name orchestrate the read-time composition
// (primary read + keyed leg fetches). yamlMaxLinkManyLimit is the yaml default
// of the per-parent LinkMany ceiling cascade. Call with an empty slice (or
// nil) to reset. Mirrors SetViews: bootstrap mutates the ONE reader instance,
// so the composition is visible to every consumer regardless of when it
// captured the reader.
func (r *MongoViewReader) SetComposedViews(defs []*query.ComposedViewDefinition, yamlMaxLinkManyLimit int64) *MongoViewReader {
	if len(defs) == 0 {
		r.composed = nil
		return r
	}
	r.composed = NewComposedViewReader(r, defs, yamlMaxLinkManyLimit)
	return r
}

// SetMaxLimitResolver installs the per-view max-limit lookup the reader
// consults at read time. The closure must return:
//   - the per-view override (query.ViewDefinition.MaxLimit) when declared (> 0);
//   - else the yaml-supplied service-wide default (cfg.Query.MaxLimit > 0);
//   - else 0 to delegate to FrameworkDefaultMaxReadLimit.
//
// Returns the reader for fluent chaining. Calling with nil resets the reader
// to the framework-default-everywhere posture (the constructor's initial
// state).
func (r *MongoViewReader) SetMaxLimitResolver(fn func(view string) int64) *MongoViewReader {
	r.maxLimitFn = fn
	return r
}

// resolveMaxLimit returns the effective ceiling for view. Always > 0.
func (r *MongoViewReader) resolveMaxLimit(view string) int64 {
	if r.maxLimitFn != nil {
		if n := r.maxLimitFn(view); n > 0 {
			return n
		}
	}
	return FrameworkDefaultMaxReadLimit
}

func (r *MongoViewReader) ReadPage(ctx context.Context, view string, c queries.ReadCriteria) (queries.Page, error) {
	// Composed names delegate to the read-time composition; its primary read
	// re-enters here under the primary view's (non-composed) name.
	if r.composed != nil && r.composed.IsComposed(view) {
		return r.composed.ReadPage(ctx, view, c)
	}

	maxLimit := r.resolveMaxLimit(view)

	// Limit cascade — the resolved max is always > 0 (framework fallback).
	// Consumer-supplied limit greater than the ceiling is rejected with the
	// canonical 400 envelope; absent or zero limit defers to the ceiling so
	// every Mongo Find always carries a bounded SetLimit. A trusted internal
	// caller (the tabular-export wrapper) may set BypassMaxLimit to use its own
	// operator-set ceiling (maxExportRows) verbatim instead of the page ceiling.
	if !c.BypassMaxLimit && c.Limit > maxLimit {
		return queries.Page{}, core.LimitExceededError(maxLimit, c.Backward || c.Before != "")
	}
	limit := c.Limit
	if limit <= 0 {
		limit = maxLimit
	}

	// Cursor mutual exclusion — defense in depth. The wrapper already rejects
	// `?after=` + `?before=` together at the wire layer; if both reach here it
	// is a programming bug at the caller (manual handler hand-rolling
	// criteria) and we surface it explicitly rather than silently honoring
	// only one.
	if c.After != "" && c.Before != "" {
		return queries.Page{}, fmt.Errorf("read criteria: after and before are mutually exclusive")
	}

	node := r.resolveViewSchema(view)
	colFilter, err := translateFilterKeys(node, c.Filter)
	if err != nil {
		return queries.Page{}, err
	}
	colSort, err := translateSortFields(node, c.OrderBy)
	if err != nil {
		return queries.Page{}, err
	}
	colProj, err := translateProjectionKeys(node, c.Projection)
	if err != nil {
		return queries.Page{}, err
	}
	sdCol, sdOn := node.DeletedAtColumn()

	filter := bson.M{}
	applyFilter(filter, colFilter)
	if !c.IncludeArchived && sdOn {
		filter[sdCol] = nil
	}
	if c.Search != "" {
		filter["$text"] = bson.M{"$search": c.Search}
	}

	col := r.mongo.collFn(r.resolver.Active(view).String())

	// Only-total short-circuit: skip Find + projection + cursor walk. The
	// dedicated wire envelope (TotalOnlyPagination) consumes solely TotalCount.
	if c.OnlyTotal {
		total, err := col.CountDocuments(ctx, filter)
		if err != nil {
			return queries.Page{}, err
		}
		return queries.Page{OnlyTotal: true, TotalCount: total}, nil
	}

	// The listing total counts the whole filtered set — the filter BEFORE the
	// keyset clause — and is independent of the page fetch, so it runs
	// CONCURRENTLY with the Find below instead of serializing one round trip
	// after the other. The count goroutine reads `filter` and nothing ever
	// mutates it again: the keyset clause lands on `findFilter`, a top-level
	// copy taken before the goroutine starts (appendKeysetClause rewrites only
	// top-level entries, and the shared $and slice is cloned on write there).
	type countResult struct {
		total int64
		err   error
	}
	countCh := make(chan countResult, 1)
	go func() {
		total, err := col.CountDocuments(ctx, filter)
		countCh <- countResult{total: total, err: err}
	}()
	findFilter := make(bson.M, len(filter)+1)
	for k, v := range filter {
		findFilter[k] = v
	}

	// Direction: an explicit Backward request (`last` — every surface sets the
	// flag) OR a non-empty Before cursor, which always means backward. The
	// explicit flag is what lets `last=N` with no cursor walk back from the end
	// of the set; inferring from Before stays as defense in depth for criteria
	// assembled by hand.
	backward := c.Backward || c.Before != ""

	// Stable sort: every Mongo Find consulting this view adds `_id` (asc) as
	// the last tiebreaker so the result set is deterministic regardless of
	// natural storage order. Custom sort fields layer in front of it; the
	// backward path inverts every direction (including _id) so Mongo returns
	// the N docs immediately preceding the cursor — we reverse the slice in
	// Go before returning so the caller always sees results in the canonical
	// requested order.
	sortDoc := buildStableSortDoc(colSort, backward)
	findOpts := options.Find().SetLimit(limit + 1).SetSort(sortDoc)

	// Cursor → keyset $or cascade. The cursor's tuple aligns positionally
	// with c.OrderBy + trailing _id; the wrapper validated the tuple length
	// matches before dispatch, so a mismatch reaching here is a defense-in-
	// depth signal of a corrupt cursor.
	if c.After != "" || c.Before != "" {
		cursorStr := c.After
		if backward {
			cursorStr = c.Before
		}
		cursor, decErr := queries.DecodeCursor(cursorStr)
		if decErr != nil {
			return queries.Page{}, core.InvalidCursorError(fmt.Errorf("invalid cursor: %w", decErr))
		}
		if len(cursor.K)-1 != len(c.OrderBy) {
			return queries.Page{}, core.InvalidCursorError(fmt.Errorf("cursor tuple length %d does not match sort field count %d",
				len(cursor.K)-1, len(c.OrderBy)))
		}
		// Context alignment — THE authoritative check, on every surface. The
		// hash covers the full listing context (filter + sort + search +
		// includeArchived) AS THE READER SEES IT: post-ToCriteria, identity
		// overlays included — the same context outgoing cursors are stamped
		// with, so overlay-bearing paged queries round-trip. ANY mismatch means
		// the cursor was issued against a different result set (consumer
		// changed the query mid-navigation, or carried a cursor from another
		// listing) → the canonical Schema notification (400-equivalent), never
		// a silently wrong page. The REST wrapper deliberately does NOT
		// pre-compare the hash (it only sees the pre-overlay wire snapshot);
		// every surface defers to this check.
		if cursor.H != queries.HashContext(c.Filter, c.OrderBy, c.Search, c.IncludeArchived) {
			return queries.Page{}, core.InvalidCursorError(errors.New("cursor context hash does not match current criteria"))
		}
		direction := 1
		if backward {
			direction = -1
		}
		keyset := buildKeysetFilter(cursor.K, colSort, direction)
		appendKeysetClause(findFilter, keyset)
	}

	// Projection — if the consumer requested ?fields= AND the active sort
	// would otherwise be stripped from the doc, transparently re-include the
	// sort field paths so the doc carries the values we need for NextCursor /
	// PrevCursor. Strip them from the returned doc after the cursor build so
	// the wire shape stays exactly as the consumer asked.
	inclusion := c.Projection.IsInclusion()
	autoIncluded := projectionAutoIncluded(colProj, colSort, inclusion)
	// A consumer projection that narrows a SEGMENT's subfields
	// (?fields=dependents.name, ?fields=product.code, a GraphQL selection set)
	// would drop that segment's DeletedAt column from the returned entries —
	// and the default-read archived strip below can only hide what the entries
	// still carry. Auto-include the DeletedAt column of every segment that
	// declares one (child collections, roles, materialized embeds, EmbedInChild
	// enrichments alike), and remember it for post-strip removal so the wire
	// shape still matches the consumer's request exactly.
	childSDCleanup := map[string]string{}
	if len(colProj) > 0 && !c.IncludeArchived {
		var extra []string
		extra, childSDCleanup = childDeletedAtAutoIncludes(colProj, node.ChildDeletedAtPaths(), inclusion)
		autoIncluded = append(autoIncluded, extra...)
	}
	// The store's identity is the cursor's ABSOLUTE tiebreaker — buildStableSortDoc
	// appends `_id` to every sort and encodeTupleCursor reads it off the doc — so it
	// obeys the same auto-include / post-strip contract the two blocks above apply
	// to sort fields and segment DeletedAt columns. It was the one cursor input
	// outside that mechanism: a selection that did not name the identity had `_id`
	// EXCLUDED (translateProjectionKeys' `_id: 0`), which left the tiebreaker slot
	// reading a value the doc no longer carried. Re-include it for the query and
	// strip it after the cursors are built, so the wire shape still matches the
	// selection exactly.
	// A selection that does not name the identity had `_id` excluded, which would
	// leave the cursor's tiebreaker unreadable. Force it back into the projection;
	// normalizeIdentity takes it out of every document again below, on the broader
	// question of whether the selection keeps the identity at all.
	identityAutoIncluded(colProj, inclusion)
	keepIdentity := c.Projection.Keeps(idGoField)
	// An exclusion projection whose every entry was un-excluded above has nothing
	// left to say: `{}` is not a projection Mongo accepts as "the whole document",
	// so the read simply goes unprojected, and the strip below still shapes the
	// wire.
	if len(colProj) > 0 {
		findOpts.SetProjection(buildProjection(colProj))
	}

	cur, err := col.Find(ctx, findFilter, findOpts)
	if err != nil {
		return queries.Page{}, err
	}
	defer cur.Close(ctx)

	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return queries.Page{}, err
	}

	// Join the concurrent count — the page carries TotalCount on every read,
	// so a count failure fails the read exactly as it did when it ran first.
	counted := <-countCh
	if counted.err != nil {
		return queries.Page{}, counted.err
	}
	total := counted.total

	// The +1 trick: a returned slice strictly bigger than `limit` proves
	// there is at least one more doc in the queried direction.
	localHasMore := int64(len(docs)) > limit
	if localHasMore {
		docs = docs[:limit]
	}

	// Backward path queried Mongo in inverted sort order; flip the slice so
	// the caller observes the canonical order.
	if backward {
		for i, j := 0, len(docs)-1; i < j; i, j = i+1, j-1 {
			docs[i], docs[j] = docs[j], docs[i]
		}
	}

	// Build cursors from the doc BEFORE stripping auto-included sort fields.
	// The context hash binds each cursor to its issuing call's full listing
	// context (filter + sort + search + includeArchived) so a later request
	// that changes ANY axis is detected and rejected upstream instead of
	// silently navigating against the old keyset boundary on a different
	// result set.
	contextHash := queries.HashContext(c.Filter, c.OrderBy, c.Search, c.IncludeArchived)
	var nextCursorStr, prevCursorStr string
	// Per-item cursors, positionally aligned with the docs (and thus the
	// Items built below) in canonical order. Built from the SAME physical doc
	// the edge cursors use — before the sort-field auto-includes are stripped,
	// since the Go-field-keyed Items no longer carry them. Transports that need
	// a cursor per element (the GraphQL Relay connection) read Page.ItemCursors;
	// the edge NextCursor / PrevCursor stay the first / last of this list.
	var itemCursors []string
	if len(docs) > 0 {
		itemCursors = make([]string, len(docs))
		for i, d := range docs {
			cur, cerr := encodeTupleCursor(d, colSort, contextHash)
			if cerr != nil {
				return queries.Page{}, cerr
			}
			itemCursors[i] = cur
		}
		prevCursorStr = itemCursors[0]
		nextCursorStr = itemCursors[len(itemCursors)-1]
	}

	// Strip the sort-field auto-includes so the wire shape matches the
	// consumer's `?fields=` request exactly.
	for _, path := range autoIncluded {
		for _, d := range docs {
			deleteDocPath(d, path)
		}
	}

	items := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		m := map[string]any(d)
		normalizeBSONValues(m)
		// The root-level DeletedAt gate ran in the Mongo filter; the same
		// default-read contract applies to EVERY segment below it — child
		// collections, roles, materialized embed segments and EmbedInChild
		// enrichments — each filtered only where its own source schema declares
		// a DeletedAt column. Skipped wholesale when the caller asked for
		// archived data, which is what makes one flag reveal every level.
		if !c.IncludeArchived {
			node.StripArchivedChildren(m)
		}
		// Remove the auto-included child DeletedAt columns from the kept
		// entries — the strip has already consumed them, and the consumer's
		// projection did not ask for them. Paths cover three shapes: a child
		// collection at the root ("Dependents"), a SharedBaseView role segment
		// (a single map, "User"), and a role's own child collection (dotted,
		// "User.Dependents").
		for docField, sdCol := range childSDCleanup {
			removeChildSDColumn(m, strings.Split(docField, "."), sdCol)
		}
		items = append(items, normalizeIdentity(node.ToGoDoc(m), keepIdentity))
	}

	isFirstForward := c.After == "" && c.Before == ""

	var hasNext, hasPrev bool
	if backward {
		// Backward came from one of two places. With a Before cursor we walked
		// BACK from a forward page, so the page we left still sits ahead →
		// HasNext is true. With `last:N` and no cursor we are AT the end of the
		// set, so there is nothing ahead → HasNext is false. Either way HasPrev
		// reflects whether more docs remain further behind.
		hasNext = c.Before != ""
		hasPrev = localHasMore
	} else {
		hasNext = localHasMore
		hasPrev = !isFirstForward
	}

	page := queries.Page{
		Items:           items,
		HasNextPage:     hasNext,
		HasPreviousPage: hasPrev,
		TotalCount:      total,
		ItemCursors:     itemCursors,
		Projection:      c.Projection, // echo the effective projection for export plan pruning
	}
	if hasNext {
		page.EndCursor = nextCursorStr
	}
	if hasPrev {
		page.StartCursor = prevCursorStr
	}
	return page, nil
}

func (r *MongoViewReader) ReadByID(ctx context.Context, view, id string, c queries.ReadCriteria) (map[string]any, bool, error) {
	if r.composed != nil && r.composed.IsComposed(view) {
		return r.composed.ReadByID(ctx, view, id, c)
	}
	node := r.resolveViewSchema(view)
	sdCol, sdOn := node.DeletedAtColumn()
	col := r.mongo.collFn(r.resolver.Active(view).String())
	filter := bson.M{}
	colFilter, err := translateFilterKeys(node, c.Filter)
	if err != nil {
		return nil, false, err
	}
	applyFilter(filter, colFilter)
	// The path id is the read's SUBJECT; a criteria filter that also constrains
	// the identity is an overlay scoping what this caller may read. Both apply,
	// so they AND. Seeding the filter with the path id and letting applyFilter
	// write over it would have let the overlay REPLACE the subject — the caller
	// would be served the row its own scope named instead of the one the route
	// addressed. (Before the identity vocabulary was settled the two landed on
	// different keys and never met, which is why this could not be observed.)
	if scoped, clash := filter["_id"]; clash {
		delete(filter, "_id")
		and, _ := filter["$and"].(bson.A)
		filter["$and"] = append(and, bson.M{"_id": scoped}, bson.M{"_id": id})
	} else {
		filter["_id"] = id
	}
	if !c.IncludeArchived && sdOn {
		filter[sdCol] = nil
	}
	// Projection. A by-id read has no wire `?fields=`, so what arrives here
	// came from the Query's ToCriteria — most often ReadCriteria.Restrict,
	// the field-level access-control seam, which implements the removal by
	// writing an exclusion into the projection. Ignoring it would mean the
	// restriction silently does not apply on this route.
	findOpts := options.FindOne()
	if c.Projection.Narrows() {
		colProj, perr := translateProjectionKeys(node, c.Projection)
		if perr != nil {
			return nil, false, perr
		}
		findOpts.SetProjection(buildProjection(colProj))
	}
	var doc bson.M
	err = col.FindOne(ctx, filter, findOpts).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m := map[string]any(doc)
	normalizeBSONValues(m)
	// Same default-read contract as ReadPage: archived entries in the nested
	// aggregate-child collections are hidden unless archived data was asked for.
	if !c.IncludeArchived {
		node.StripArchivedChildren(m)
	}
	// The by-id route answers to the same authority as the listing: a projection
	// that does not keep the identity does not get one, under either spelling.
	return normalizeIdentity(node.ToGoDoc(m), c.Projection.Keeps(idGoField)), true, nil
}

// normalizeBSONValues rewrites driver-specific BSON scalars into their plain
// Go equivalents, recursively through nested maps and slices (the child
// collections), so EVERY consumer downstream of the reader sees Go-typed
// values: a BSON datetime becomes time.Time (UTC). The reader is the membrane
// that promises Go vocabulary (it already translates column names); values
// follow the same rule, so the Result fill and every consumer above it see
// Go types rather than driver types.
func normalizeBSONValues(m map[string]any) {
	for k, v := range m {
		m[k] = normalizeBSONValue(v)
	}
}

func normalizeBSONValue(v any) any {
	switch t := v.(type) {
	case bson.DateTime:
		return t.Time().UTC()
	case bson.M:
		normalizeBSONValues(t)
		return map[string]any(t)
	case map[string]any:
		normalizeBSONValues(t)
		return t
	case bson.A:
		for i, item := range t {
			t[i] = normalizeBSONValue(item)
		}
		return []any(t)
	case []any:
		for i, item := range t {
			t[i] = normalizeBSONValue(item)
		}
		return t
	default:
		return v
	}
}

// buildStableSortDoc produces the canonical sort document. `_id` is always
// appended as the last element so the underlying Mongo query is stable across
// docs that share a sort_value. `reverse` inverts every direction (used by the
// backward / ?before= path); the caller restores canonical order in Go by
// reversing the returned slice.
func buildStableSortDoc(sortFields []queries.OrderByField, reverse bool) bson.D {
	doc := bson.D{}
	sortsOnIdentity := false
	for _, s := range sortFields {
		v := 1
		if s.Desc {
			v = -1
		}
		if reverse {
			v = -v
		}
		if s.Field == "_id" {
			sortsOnIdentity = true
		}
		doc = append(doc, bson.E{Key: s.Field, Value: v})
	}
	// A consumer that sorts BY the identity already named the tiebreaker, and it
	// is unique — nothing after it can order anything. Appending the automatic
	// `_id` again would put the key in the sort document twice, in contradictory
	// directions when that sort was DESC. Its own term stands as the tiebreaker.
	if sortsOnIdentity {
		return doc
	}
	idDir := 1
	if reverse {
		idDir = -1
	}
	doc = append(doc, bson.E{Key: "_id", Value: idDir})
	return doc
}

// buildKeysetFilter produces the $or cascade that page-keysets past the
// cursor tuple. For a tuple of length n (sort fields + _id), it emits n
// $or arms: arm p (1-indexed) carries equalities on tuple[0..p-2] AND a
// strict inequality on tuple[p-1]. The inequality direction is
// `globalDirection × fieldDirection` — globalDirection=+1 for ?after=,
// -1 for ?before=; fieldDirection=+1 for ASC sort fields, -1 for DESC.
// The _id slot is treated as ASC throughout (its direction always +1).
func buildKeysetFilter(tuple []any, sortFields []queries.OrderByField, globalDirection int) bson.M {
	arms := bson.A{}
	n := len(tuple)
	// The cascade ends at the identity: it is unique, so an arm past it can only
	// restate the same boundary. When the CONSUMER sorted by the identity, that
	// slot — not the appended trailing one — is where the cascade stops. Emitting
	// the trailing arm anyway wrote an equality and an inequality on `_id` into
	// the same bson.M, where the second silently overwrote the first and turned
	// the arm into "everything on the other side", which un-bounded the page.
	for i, sf := range sortFields {
		if sf.Field == "_id" {
			n = i + 1
			break
		}
	}
	for p := 1; p <= n; p++ {
		arm := bson.M{}
		for i := 0; i < p-1; i++ {
			field := sortFieldAt(sortFields, i)
			arm[field] = tuple[i]
		}
		fieldDir := 1
		if p-1 < len(sortFields) && sortFields[p-1].Desc {
			fieldDir = -1
		}
		op := "$gt"
		if globalDirection*fieldDir == -1 {
			op = "$lt"
		}
		field := sortFieldAt(sortFields, p-1)
		arm[field] = bson.M{op: tuple[p-1]}
		arms = append(arms, arm)
	}
	return bson.M{"$or": arms}
}

// sortFieldAt returns the doc-path of the sort field at position i, or "_id"
// when i is past the user-declared sort list (the trailing tiebreaker slot).
func sortFieldAt(sortFields []queries.OrderByField, i int) string {
	if i < len(sortFields) {
		return sortFields[i].Field
	}
	return "_id"
}

// appendKeysetClause merges the keyset $or into the existing filter without
// clobbering pre-existing $and / $or sentinels. MultiClause filters land as
// $and arrays via applyFilter; in that case we append the keyset clause as a
// new $and entry. When no $and exists we can lift the keyset $or to the top
// level (Mongo treats top-level field entries as AND).
//
// The $and slice is CLONED, never appended in place: the caller hands in a
// top-level copy of the base filter whose nested values are shared with the
// concurrently-running count query, so writing into the shared backing array
// is off the table.
func appendKeysetClause(filter bson.M, keyset bson.M) {
	if existing, ok := filter["$and"]; ok {
		if arr, isArr := existing.(bson.A); isArr {
			merged := make(bson.A, 0, len(arr)+1)
			merged = append(merged, arr...)
			filter["$and"] = append(merged, keyset)
			return
		}
	}
	if existing, ok := filter["$or"]; ok {
		// Coalesce both $or as AND.
		filter["$and"] = bson.A{
			bson.M{"$or": existing},
			keyset,
		}
		delete(filter, "$or")
		return
	}
	for k, v := range keyset {
		filter[k] = v
	}
}

// projectionAutoIncluded makes the projection carry the sort values the cursor
// builder reads off each doc, and returns the paths to remove from the doc once
// the cursors are encoded — so the wire shape still matches the consumer's
// request exactly. The returned list is therefore "what the doc must carry that
// the consumer did NOT ask for", which is precisely what gets stripped again.
//
// The repair is mode-dependent, for the same reason identityAutoIncluded's is:
// a Mongo projection is either an include-list or an exclude-list, and mixing
// them fails the whole read (Location31253 "Cannot do inclusion on field X in
// exclusion projection"). Only `_id` is exempt.
//
//   - INCLUSION (`?fields=email` + ?orderBy=name): a sort field the selection
//     did not name is absent from the doc, so it is added as an inclusion.
//   - EXCLUSION (what ReadCriteria.Restrict produces): every field is served
//     unless the projection names it. A sort field it does NOT name is already
//     there and needs nothing — adding `name: 1` beside `phone: 0` was what made
//     a field-restricted listing with an ?orderBy= fail the read outright. One
//     the projection DOES name is un-excluded instead, which is the only repair
//     the mode allows. Restrict never produces that combination (it scrubs the
//     field from OrderBy as it drops it from the projection), so it is reachable
//     only from criteria assembled by hand.
func projectionAutoIncluded(userProj map[string]int, sortFields []queries.OrderByField, inclusion bool) []string {
	if len(userProj) == 0 || len(sortFields) == 0 {
		return nil
	}
	out := make([]string, 0, len(sortFields))
	for _, sf := range sortFields {
		if sf.Field == "_id" {
			continue
		}
		_, named := userProj[sf.Field]
		if inclusion {
			if named {
				continue
			}
			userProj[sf.Field] = 1
		} else {
			if !named {
				continue
			}
			delete(userProj, sf.Field)
		}
		out = append(out, sf.Field)
	}
	return out
}

// identityAutoIncluded re-includes the store's identity column in a projection
// that would have dropped it, so the page's cursors can read the tiebreaker off
// the doc. Getting it back OUT is not its business: normalizeIdentity drops the
// identity from every document the selection does not keep, which is the broader
// condition — a projection can drop the identity without ever naming `_id`, and
// an exclusion of the schema's own id column does exactly that.
//
// Both narrowing modes can drop the identity, and they need OPPOSITE repairs
// because `_id` is the one field Mongo lets a projection flag against its mode:
//   - inclusion (`?fields=name` → `{name: 1, _id: 0}`): flip the wrapper's
//     auto-exclusion to `{_id: 1}`. Legal beside the inclusions, and the only way
//     to get the column back.
//   - exclusion (ReadCriteria.Restrict dropping "ID" → `{_id: 0}`): DELETE the
//     entry instead. An exclusion projection returns `_id` by default, so
//     removing the exclusion is enough — and writing `{_id: 1}` there would be
//     read as an inclusion projection of the identity ALONE when `_id` is the
//     only entry, collapsing the whole document to its key.
//
// Anything else — a projection that keeps the identity, or one that never
// narrowed — is left untouched and reported false.
func identityAutoIncluded(userProj map[string]int, inclusion bool) {
	if flag, declared := userProj["_id"]; !declared || flag != 0 {
		return
	}
	if inclusion {
		userProj["_id"] = 1
	} else {
		delete(userProj, "_id")
	}
}

// buildProjection assembles the bson.D projection from the resolved column map:
// keys sorted so the emitted document is stable across runs, values verbatim.
//
// It renders, it does not decide. Every auto-include — sort fields, segment
// DeletedAt columns, the identity — has already been folded INTO the map by the
// helper that owns that decision, each one writing the flag its projection mode
// allows. Rendering the map is what keeps a single mode in the emitted document;
// appending auto-included paths as a separate `: 1` tail (what this did before)
// could not know the mode and put an inclusion into an exclusion projection.
func buildProjection(userProj map[string]int) bson.D {
	keys := make([]string, 0, len(userProj))
	for k := range userProj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	proj := bson.D{}
	for _, k := range keys {
		proj = append(proj, bson.E{Key: k, Value: userProj[k]})
	}
	return proj
}

// encodeTupleCursor walks the doc reading each sort field's value, then
// appends the stringified _id as the absolute tiebreaker. The encoded form
// is opaque to the consumer — base64(URLEncoding) of the JSON payload —
// matched by queries.DecodeCursor at the reader's next call. contextHash
// binds the cursor to the issuing call's full listing context (filter +
// sort + search + includeArchived) so a mid-navigation context change is
// detected and rejected; the empty string is the canonical hash for the
// default context (no filter, no sort, no search, archived excluded).
func encodeTupleCursor(doc map[string]any, sortFields []queries.OrderByField, contextHash string) (string, error) {
	tuple := make([]any, 0, len(sortFields)+1)
	for _, sf := range sortFields {
		tuple = append(tuple, lookupDocPath(doc, sf.Field))
	}
	// A doc with no identity has no valid keyset cursor: the tiebreaker is what
	// makes the next page start strictly AFTER this row, and there is nothing to
	// compare against. Stringifying the absent value produced the literal
	// "<nil>" — a cursor that decodes, matches its context hash and then compares
	// `_id > "<nil>"`, which lands mid-alphabet and silently re-serves the same
	// row for every id that sorts below it. Refuse instead: the read path
	// guarantees `_id` is projected (identityAutoIncluded), so reaching here
	// means the doc itself is malformed, and a loud failure is the honest answer.
	id, ok := doc["_id"]
	if !ok || id == nil {
		return "", fmt.Errorf("cannot build a keyset cursor: document carries no _id")
	}
	tuple = append(tuple, fmt.Sprintf("%v", id))
	return queries.EncodeCursor(tuple, contextHash)
}

// lookupDocPath walks a dotted doc path returning the leaf value, or nil if
// any intermediate node is absent / not a map. Nested array entries (e.g.
// "addresses.city" where addresses is []map) return nil — sort over array
// elements has no defined semantic, and the reader does not pretend to
// support it.
func lookupDocPath(doc map[string]any, path string) any {
	parts := strings.Split(path, ".")
	var current any = doc
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}

// deleteDocPath removes the leaf at the dotted path from doc. No-op if any
// intermediate node is absent or not a map. Used to strip cursor-only sort
// fields the reader auto-included into the Mongo projection.
func deleteDocPath(doc map[string]any, path string) {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		delete(doc, parts[0])
		return
	}
	var current any = doc
	for i := 0; i < len(parts)-1; i++ {
		m, ok := current.(map[string]any)
		if !ok {
			return
		}
		current = m[parts[i]]
	}
	if m, ok := current.(map[string]any); ok {
		delete(m, parts[len(parts)-1])
	}
}

// applyFilter materializes c.Filter onto dst, translating port-level
// sentinel values into MongoDB-native shapes and expanding MultiClause
// entries into a top-level `$and` array.
//
// MultiClause carries more than one operator clause for the same logical
// field (e.g. `?age.gte=18&age.lte=65` or
// `?name.startswith=Bob&name.icontains=ob`). Each clause becomes a
// `{field: translatedValue}` entry inside a single top-level `$and` array
// so every declared operator is honored simultaneously instead of having
// only one survive the map collapse. Multiple MultiClause entries from
// different fields contribute to the same `$and` — AND is commutative, so
// `$and: [A, B, C]` matches the intended `(A & B) & (C)` semantic
// regardless of map iteration order.
//
// Plain (single-clause) field entries land directly on dst under the
// field name, preserving the canonical `{field: value}` shape Mongo
// indexes can use without an outer `$and`.
func applyFilter(dst bson.M, src map[string]any) {
	var andClauses bson.A
	for k, v := range src {
		if mc, ok := v.(queries.MultiClause); ok {
			for _, clause := range mc.Clauses {
				andClauses = append(andClauses, bson.M{k: translateFilterValue(clause)})
			}
			continue
		}
		dst[k] = translateFilterValue(v)
	}
	if len(andClauses) > 0 {
		dst["$and"] = andClauses
	}
}

// translateFilterValue converts the port-level sentinel types defined in
// application/queries into MongoDB-native values. The web layer emits neutral
// sentinels one operator at a time: Clause for the ordinal/set operators
// (ne/gt/gte/lt/lte, in/nin) → the {$op: ...} sub-document; TextMatch for the
// text-match operators (startswith/contains and their case-folding variants)
// → a QuoteMeta'd, kind-anchored bson.Regex (negated under $not); TextMatchList
// for the case-insensitive list operators (iin/inin) → native bson.Regex
// elements inside $in / $nin.
// A bare `eq` scalar carries no sentinel and passes through unchanged (Mongo
// reads {field: scalar} as equality).
//
// The translation is shallow on purpose — the wire wrappers in web/
// produce values one operator at a time at field level; there is no
// recursive sentinel inside arbitrary sub-documents to walk. MultiClause
// is consumed one level up by applyFilter rather than here, because the
// expansion needs to know the field name to assemble the $and entries.
func translateFilterValue(v any) any {
	switch x := v.(type) {
	case queries.Clause:
		if len(x.Values) == 0 {
			return bson.M{}
		}
		switch x.Op {
		case queries.FilterIn:
			return bson.M{"$in": x.Values}
		case queries.FilterNin:
			return bson.M{"$nin": x.Values}
		case queries.FilterNe:
			return bson.M{"$ne": x.Values[0]}
		case queries.FilterGt:
			return bson.M{"$gt": x.Values[0]}
		case queries.FilterGte:
			return bson.M{"$gte": x.Values[0]}
		case queries.FilterLt:
			return bson.M{"$lt": x.Values[0]}
		case queries.FilterLte:
			return bson.M{"$lte": x.Values[0]}
		default:
			return bson.M{}
		}
	case queries.TextMatch:
		opts := ""
		if x.CaseInsensitive {
			opts = "i"
		}
		re := bson.Regex{Pattern: textPattern(x.Value, x.Kind), Options: opts}
		if x.Negate {
			return bson.M{"$not": re}
		}
		return re
	case queries.TextMatchList:
		opts := ""
		if x.CaseInsensitive {
			opts = "i"
		}
		elements := make(bson.A, 0, len(x.Values))
		for _, val := range x.Values {
			elements = append(elements, bson.Regex{Pattern: "^" + regexp.QuoteMeta(val) + "$", Options: opts})
		}
		key := "$in"
		if x.Negate {
			key = "$nin"
		}
		return bson.M{key: elements}
	default:
		return v
	}
}

// textPattern renders a store-neutral TextMatch value + kind into a MongoDB
// regular expression: regexp.QuoteMeta escapes the raw value, then the kind
// anchors it — prefix "^v", whole "^v$", substring "v". Escaping and anchoring
// live here (not in the wire layer) so the port stays store-neutral.
func textPattern(value string, kind queries.TextMatchKind) string {
	q := regexp.QuoteMeta(value)
	switch kind {
	case queries.TextPrefix:
		return "^" + q
	case queries.TextExact:
		return "^" + q + "$"
	default: // queries.TextContains
		return q
	}
}

var _ queries.ViewReader = (*MongoViewReader)(nil)

// removeChildSDColumn removes the auto-included DeletedAt column at the
// given doc-field path — a child collection ([]any of maps), a SharedBaseView
// role segment (a single map) or, dotted, a role's own child collection.
// Intermediate segments are always maps (dotted paths only descend through
// role segments); anything absent or differently shaped is a no-op.
func removeChildSDColumn(container any, segs []string, sdCol string) {
	if len(segs) == 0 {
		return
	}
	// An intermediate segment may be an ARRAY of sub-documents: a 1:N embed of a
	// local view (query.JoinView) whose own child collections carry lifecycles,
	// e.g. "sales.SaleItems" where `sales` is an array. Descend into every
	// element. (Role segments and 1:1 embeds are maps, handled below.)
	if items, ok := container.([]any); ok {
		for _, item := range items {
			removeChildSDColumn(item, segs, sdCol)
		}
		return
	}
	m, ok := container.(map[string]any)
	if !ok {
		return
	}
	if len(segs) > 1 {
		removeChildSDColumn(m[segs[0]], segs[1:], sdCol)
		return
	}
	switch t := m[segs[0]].(type) {
	case []any:
		for _, e := range t {
			if em, ok := e.(map[string]any); ok {
				delete(em, sdCol)
			}
		}
	case map[string]any:
		delete(t, sdCol)
	}
}

// projectionTouchesField reports whether the (physical) projection references
// the given doc field path — either whole ("Dependents", "User",
// "User.Dependents") or any subfield ("Dependents.name").
func projectionTouchesField(colProj map[string]int, docField string) bool {
	for key := range colProj {
		if key == docField || strings.HasPrefix(key, docField+".") {
			return true
		}
	}
	return false
}

// childDeletedAtAutoIncludes decides which segment DeletedAt columns a
// consumer projection must transparently re-include so the default-read
// archived strip can still see (and hide) archived content, plus the per-field
// cleanup map (docField -> sdCol) used to remove those columns from the
// returned docs afterwards so the wire shape matches the request. "child" in
// the name is historical: the paths cover every segment kind that declares a
// DeletedAt column, not only child collections.
//
// It fires ONLY when the projection narrows to a STRICT SUBFIELD of the child
// (dependents.name): that projection would otherwise drop the child's
// DeletedAt column, blinding the strip. When the WHOLE child field is
// projected (?fields=dependents) the returned object ALREADY carries its
// DeletedAt column, so re-including "dependents.deleted_at" is both
// unnecessary and an ILLEGAL projection — Mongo rejects an inclusion that lists
// a field and a subpath of it together (Location31249 "Path collision at
// <field>.<sub>"). The whole-field case is skipped, so it behaves exactly like
// a no-?fields read for that segment: the strip sees the column, and
// ToGoDoc/the Response DTO drop it on the wire.
// The mode split is the same one projectionAutoIncluded and identityAutoIncluded
// answer: an INCLUSION that narrows into the segment has to add the column back,
// while an EXCLUSION already serves it and must only un-exclude the case where
// the projection named the column itself. Writing `dependents.deleted_at: 1`
// into an exclusion projection would fail the read the way the sort-field
// auto-include did (Location31253), and it was never needed there.
func childDeletedAtAutoIncludes(colProj map[string]int, childSDPaths map[string]string, inclusion bool) ([]string, map[string]string) {
	var autoIncluded []string
	cleanup := map[string]string{}
	for docField, sdCol := range childSDPaths {
		if _, whole := colProj[docField]; whole {
			continue
		}
		if !projectionTouchesField(colProj, docField) {
			continue
		}
		path := docField + "." + sdCol
		if inclusion {
			colProj[path] = 1
		} else {
			if _, named := colProj[path]; !named {
				continue
			}
			delete(colProj, path)
		}
		autoIncluded = append(autoIncluded, path)
		cleanup[docField] = sdCol
	}
	return autoIncluded, cleanup
}
