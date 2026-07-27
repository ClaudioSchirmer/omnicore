package mongo

import (
	"context"
	"errors"
	"fmt"
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
// physical-column-keyed filter via the view node. Keys that do not resolve are
// passed through unchanged (defensive; the web allowlist validates first).
func translateFilterKeys(node *query.ViewNode, src map[string]any) map[string]any {
	if len(src) == 0 {
		return src
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[translateDotted(node, k)] = v
	}
	return out
}

// translateSortFields rewrites Go-field-path sort terms into physical columns.
func translateSortFields(node *query.ViewNode, src []queries.SortField) []queries.SortField {
	if len(src) == 0 {
		return src
	}
	out := make([]queries.SortField, len(src))
	for i, s := range src {
		out[i] = queries.SortField{Field: translateDotted(node, s.Field), Desc: s.Desc}
	}
	return out
}

// translateProjectionKeys rewrites a Go-field-path projection into physical
// columns. The reserved "_id" key passes through untranslated.
func translateProjectionKeys(node *query.ViewNode, src map[string]int) map[string]int {
	if len(src) == 0 {
		return src
	}
	out := make(map[string]int, len(src))
	for k, v := range src {
		if k == "_id" {
			out[k] = v
			continue
		}
		out[translateDotted(node, k)] = v
	}
	return out
}

// translateDotted translates a dotted Go field path into the dotted physical
// column path, leaving it unchanged when the node cannot resolve it.
func translateDotted(node *query.ViewNode, dotted string) string {
	parts := strings.Split(dotted, ".")
	col, ok := node.ColumnPath(parts)
	if !ok {
		return dotted
	}
	return strings.Join(col, ".")
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
		return queries.Page{}, core.LimitExceededError(maxLimit)
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
	colFilter := translateFilterKeys(node, c.Filter)
	colSort := translateSortFields(node, c.Sort)
	colProj := translateProjectionKeys(node, c.Projection)
	sdCol, sdOn := node.SoftDeleteColumn()

	filter := bson.M{}
	applyFilter(filter, colFilter)
	if !c.IncludeArchived && sdOn {
		filter[sdCol] = nil
	}
	if c.Search != "" {
		filter["$text"] = bson.M{"$search": c.Search}
	}

	col := r.mongo.collFn(r.resolver.Active(view).String())

	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		return queries.Page{}, err
	}

	// Count-only short-circuit: skip Find + projection + cursor walk. The
	// dedicated wire envelope (TotalOnlyPagination) consumes solely Total.
	if c.OnlyTotal {
		return queries.Page{OnlyTotal: true, Total: total}, nil
	}

	// Direction: an explicit Backward request (GraphQL Relay `last`) OR a
	// non-empty Before cursor (REST `?before=`, which always means backward).
	// REST never sets Backward, so its behavior is unchanged; the explicit flag
	// is what lets `last:N` with no cursor walk back from the end of the set.
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
	// with c.Sort + trailing _id; the wrapper validated the tuple length
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
		if len(cursor.K)-1 != len(c.Sort) {
			return queries.Page{}, core.InvalidCursorError(fmt.Errorf("cursor tuple length %d does not match sort field count %d",
				len(cursor.K)-1, len(c.Sort)))
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
		if cursor.H != queries.HashContext(c.Filter, c.Sort, c.Search, c.IncludeArchived) {
			return queries.Page{}, core.InvalidCursorError(errors.New("cursor context hash does not match current criteria"))
		}
		direction := 1
		if backward {
			direction = -1
		}
		keyset := buildKeysetFilter(cursor.K, colSort, direction)
		appendKeysetClause(filter, keyset)
	}

	// Projection — if the consumer requested ?fields= AND the active sort
	// would otherwise be stripped from the doc, transparently re-include the
	// sort field paths so the doc carries the values we need for NextCursor /
	// PrevCursor. Strip them from the returned doc after the cursor build so
	// the wire shape stays exactly as the consumer asked.
	autoIncluded := projectionAutoIncluded(colProj, colSort)
	// A consumer projection that narrows a derived child collection's
	// subfields (?fields=dependents.name / a GraphQL selection set) would
	// strip the child's soft-delete column from the returned entries — and
	// the default-read archived-entry strip below can only hide what the
	// entries still carry. Auto-include each projected child's soft-delete
	// column, and remember it for post-strip removal so the wire shape still
	// matches the consumer's request exactly.
	childSDCleanup := map[string]string{}
	if len(colProj) > 0 && !c.IncludeArchived {
		var extra []string
		extra, childSDCleanup = childSoftDeleteAutoIncludes(colProj, node.ChildSoftDeletePaths())
		autoIncluded = append(autoIncluded, extra...)
	}
	if len(colProj) > 0 {
		findOpts.SetProjection(buildProjection(colProj, autoIncluded))
	}

	cur, err := col.Find(ctx, filter, findOpts)
	if err != nil {
		return queries.Page{}, err
	}
	defer cur.Close(ctx)

	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return queries.Page{}, err
	}

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
	contextHash := queries.HashContext(c.Filter, c.Sort, c.Search, c.IncludeArchived)
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
			itemCursors[i] = encodeTupleCursor(d, colSort, contextHash)
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
		// The root-level soft-delete gate ran in the Mongo filter; the same
		// default-read contract applies one level down — archived entries in
		// the nested aggregate-child collections are stripped unless the
		// caller asked for archived data.
		if !c.IncludeArchived {
			node.StripArchivedChildren(m)
		}
		// Remove the auto-included child soft-delete columns from the kept
		// entries — the strip has already consumed them, and the consumer's
		// projection did not ask for them. Paths cover three shapes: a child
		// collection at the root ("Dependents"), a SharedBaseView role segment
		// (a single map, "User"), and a role's own child collection (dotted,
		// "User.Dependents").
		for docField, sdCol := range childSDCleanup {
			removeChildSDColumn(m, strings.Split(docField, "."), sdCol)
		}
		items = append(items, node.ToGoDoc(m))
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
		Items:       items,
		HasNext:     hasNext,
		HasPrev:     hasPrev,
		Total:       total,
		ItemCursors: itemCursors,
		Projection:  c.Projection, // echo the effective projection for export plan pruning
	}
	if hasNext {
		page.NextCursor = nextCursorStr
	}
	if hasPrev {
		page.PrevCursor = prevCursorStr
	}
	return page, nil
}

func (r *MongoViewReader) ReadByID(ctx context.Context, view, id string, c queries.ReadCriteria) (map[string]any, bool, error) {
	if r.composed != nil && r.composed.IsComposed(view) {
		return r.composed.ReadByID(ctx, view, id, c)
	}
	node := r.resolveViewSchema(view)
	sdCol, sdOn := node.SoftDeleteColumn()
	col := r.mongo.collFn(r.resolver.Active(view).String())
	filter := bson.M{"_id": id}
	applyFilter(filter, translateFilterKeys(node, c.Filter))
	if !c.IncludeArchived && sdOn {
		filter[sdCol] = nil
	}
	var doc bson.M
	err := col.FindOne(ctx, filter).Decode(&doc)
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
	return node.ToGoDoc(m), true, nil
}

// normalizeBSONValues rewrites driver-specific BSON scalars into their plain
// Go equivalents, recursively through nested maps and slices (the child
// collections), so EVERY consumer downstream of the reader sees Go-typed
// values: a BSON datetime becomes time.Time (UTC). The typed-Response JSON
// path already tolerated the driver types via its unmarshal round-trip, but
// consumers of the raw document — the tabular export, RawDoc handlers —
// received bson.DateTime and rendered epoch milliseconds. The reader is the
// membrane that promises Go vocabulary (it already translates column names);
// values follow the same rule.
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
func buildStableSortDoc(sortFields []queries.SortField, reverse bool) bson.D {
	doc := bson.D{}
	for _, s := range sortFields {
		v := 1
		if s.Desc {
			v = -1
		}
		if reverse {
			v = -v
		}
		doc = append(doc, bson.E{Key: s.Field, Value: v})
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
func buildKeysetFilter(tuple []any, sortFields []queries.SortField, globalDirection int) bson.M {
	arms := bson.A{}
	n := len(tuple)
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
func sortFieldAt(sortFields []queries.SortField, i int) string {
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
func appendKeysetClause(filter bson.M, keyset bson.M) {
	if existing, ok := filter["$and"]; ok {
		if arr, isArr := existing.(bson.A); isArr {
			filter["$and"] = append(arr, keyset)
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

// projectionAutoIncluded returns the doc paths to add to the projection so
// the doc carries the values needed to build cursors when ?fields= would
// otherwise strip a sort field. Returned paths are stripped from the
// returned doc after the cursor is encoded so the wire shape matches the
// consumer's ?fields= request exactly.
func projectionAutoIncluded(userProj map[string]int, sortFields []queries.SortField) []string {
	if len(userProj) == 0 || len(sortFields) == 0 {
		return nil
	}
	out := make([]string, 0, len(sortFields))
	for _, sf := range sortFields {
		if sf.Field == "_id" {
			continue
		}
		if _, included := userProj[sf.Field]; included {
			continue
		}
		out = append(out, sf.Field)
	}
	return out
}

// buildProjection assembles the bson.D projection. User-declared keys come
// in deterministic order (sorted) so the emitted projection is stable across
// runs; auto-included paths land after, also sorted. _id auto-exclusion
// declared by the wrapper (userProj["_id"] = 0) is preserved verbatim.
func buildProjection(userProj map[string]int, autoIncluded []string) bson.D {
	keys := make([]string, 0, len(userProj))
	for k := range userProj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	proj := bson.D{}
	for _, k := range keys {
		proj = append(proj, bson.E{Key: k, Value: userProj[k]})
	}
	if len(autoIncluded) > 0 {
		extra := append([]string(nil), autoIncluded...)
		sort.Strings(extra)
		for _, k := range extra {
			proj = append(proj, bson.E{Key: k, Value: 1})
		}
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
func encodeTupleCursor(doc map[string]any, sortFields []queries.SortField, contextHash string) string {
	tuple := make([]any, 0, len(sortFields)+1)
	for _, sf := range sortFields {
		tuple = append(tuple, lookupDocPath(doc, sf.Field))
	}
	tuple = append(tuple, fmt.Sprintf("%v", doc["_id"]))
	s, _ := queries.EncodeCursor(tuple, contextHash)
	return s
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
// application/queries into MongoDB-native values. The web layer emits these
// sentinels for case-insensitive list operators (iin / inin), where Mongo
// requires native bson.Regex elements inside $in / $nin rather than the
// {$regex: pattern, $options: "i"} sub-document shape that works fine at
// field level. All other values pass through unchanged.
//
// The translation is shallow on purpose — the wire wrappers in web/
// produce values one operator at a time at field level; there is no
// recursive sentinel inside arbitrary sub-documents to walk. MultiClause
// is consumed one level up by applyFilter rather than here, because the
// expansion needs to know the field name to assemble the $and entries.
func translateFilterValue(v any) any {
	switch x := v.(type) {
	case queries.RegexMatch:
		opts := ""
		if x.CaseInsensitive {
			opts = "i"
		}
		re := bson.Regex{Pattern: x.Pattern, Options: opts}
		if x.Negate {
			return bson.M{"$not": re}
		}
		return re
	case queries.RegexMatchList:
		opts := ""
		if x.CaseInsensitive {
			opts = "i"
		}
		elements := make(bson.A, 0, len(x.Patterns))
		for _, p := range x.Patterns {
			elements = append(elements, bson.Regex{Pattern: p, Options: opts})
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

var _ queries.ViewReader = (*MongoViewReader)(nil)

// removeChildSDColumn removes the auto-included soft-delete column at the
// given doc-field path — a child collection ([]any of maps), a SharedBaseView
// role segment (a single map) or, dotted, a role's own child collection.
// Intermediate segments are always maps (dotted paths only descend through
// role segments); anything absent or differently shaped is a no-op.
func removeChildSDColumn(container any, segs []string, sdCol string) {
	m, ok := container.(map[string]any)
	if !ok || len(segs) == 0 {
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

// childSoftDeleteAutoIncludes decides which child soft-delete columns a
// consumer projection must transparently re-include so the default-read
// archived-entry strip can still see (and hide) archived nested entries, plus
// the per-field cleanup map (docField -> sdCol) used to remove those columns
// from the returned docs afterwards so the wire shape matches the request.
//
// It fires ONLY when the projection narrows to a STRICT SUBFIELD of the child
// (dependents.name): that projection would otherwise drop the child's
// soft-delete column, blinding the strip. When the WHOLE child field is
// projected (?fields=dependents) the returned object ALREADY carries its
// soft-delete column, so re-including "dependents.deleted_at" is both
// unnecessary and an ILLEGAL projection — Mongo rejects an inclusion that lists
// a field and a subpath of it together (Location31249 "Path collision at
// <field>.<sub>"). The whole-field case is skipped, so it behaves exactly like
// a no-?fields read for that segment: the strip sees the column, and
// ToGoDoc/the Response DTO drop it on the wire.
func childSoftDeleteAutoIncludes(colProj map[string]int, childSDPaths map[string]string) ([]string, map[string]string) {
	var autoIncluded []string
	cleanup := map[string]string{}
	for docField, sdCol := range childSDPaths {
		if _, whole := colProj[docField]; whole {
			continue
		}
		if projectionTouchesField(colProj, docField) {
			autoIncluded = append(autoIncluded, docField+"."+sdCol)
			cleanup[docField] = sdCol
		}
	}
	return autoIncluded, cleanup
}
