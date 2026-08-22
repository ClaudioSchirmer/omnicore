package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// idGoField is the Go field name the id resolves under in a criteria — the
// Entity contract fixes it, and criteria.ByID uses the same spelling. It is the
// deterministic ORDER BY tiebreak the offset pagination appends, mirroring the
// Mongo reader's `_id` tiebreak.
const idGoField = "ID"

// defaultPageLimit caps a page when the request carries no ?first/?last. The per-view
// MaxLimit ceiling is an operational refinement layered on top later; this is the
// floor so an unbounded relational read never happens.
const defaultPageLimit int64 = 100

// view is one relational-backed view's read state: the root schema (drives the
// criteria field resolution and the doc mapping), the ViewNode (column<->Go
// translation + the archived-child strip) and the aggregate loader the view was
// declared over. All three come from the declaration; the schema is the loader's
// own, so they cannot disagree.
type view struct {
	schema *core.TableSchema
	node   *query.ViewNode
	loader query.AggregateReader
}

// ViewReader implements queries.ViewReader by reading a relational-backed view
// from the source of record: it loads the aggregate through the view's loader,
// maps it into a column-keyed document (BuildDocument), then applies the view's
// ViewNode strip + Go translation, so the four web surfaces read it exactly as
// they read a projected view. Pagination is offset-in-cursor (skip/count), not
// keyset — the relational idiom — behind the unchanged after/before/limit surface.
type ViewReader struct {
	views map[string]view
	// maxLimitFn resolves the per-view page-size (`?first=`/`?last=`) ceiling, mirroring the Mongo
	// reader's cascade EXACTLY: the same resolver the bootstrap builds from the
	// view defs + the yaml default is wired into both readers, so a view's
	// MaxLimit and the global ceiling apply identically whichever backing serves
	// it. nil until SetMaxLimitResolver runs; resolveMaxLimit then falls back to
	// the framework floor.
	maxLimitFn func(string) int64
}

var _ queries.ViewReader = (*ViewReader)(nil)

// NewViewReader builds the reader from the declared relational views, indexed by
// name. The declarations are boot-validated (query.ValidateRelationalViews) before
// they get here, so a nil loader or a schemaless one has already aborted the boot
// — this constructor rejects nothing and cannot panic. A malformed entry that
// somehow arrives is skipped rather than serving a broken view.
func NewViewReader(views []*query.RelationalViewDefinition) *ViewReader {
	r := &ViewReader{views: make(map[string]view, len(views))}
	for _, v := range views {
		if v == nil || v.Loader() == nil || v.SchemaDef() == nil {
			continue
		}
		r.views[v.Name()] = view{
			schema: v.SchemaDef(),
			node:   v.BuildViewNode(),
			loader: v.Loader(),
		}
	}
	return r
}

// Empty reports whether any relational view is registered — the wiring installs
// this reader into the read seam only when there is one.
func (r *ViewReader) Empty() bool { return len(r.views) == 0 }

// SetMaxLimitResolver installs the per-view page-size ceiling resolver — the SAME
// closure the bootstrap wires into the Mongo reader — so the two backings enforce
// an identical MaxLimit cascade. Returns the receiver for chaining at the wiring
// site.
func (r *ViewReader) SetMaxLimitResolver(fn func(view string) int64) *ViewReader {
	r.maxLimitFn = fn
	return r
}

// resolveMaxLimit returns the effective per-page ceiling for view. Always > 0:
// a nil resolver or a non-positive answer falls back to the framework floor
// (defaultPageLimit), matching MongoViewReader.resolveMaxLimit.
func (r *ViewReader) resolveMaxLimit(view string) int64 {
	if r.maxLimitFn != nil {
		if n := r.maxLimitFn(view); n > 0 {
			return n
		}
	}
	return defaultPageLimit
}

// ViewNames is the per-view route the read seam consults: the set of view names
// this reader serves. The seam sends exactly these here and everything else to
// its fallback backing.
func (r *ViewReader) ViewNames() map[string]bool {
	out := make(map[string]bool, len(r.views))
	for name := range r.views {
		out[name] = true
	}
	return out
}

// ReadPage serves a paged read of a relational view: filter -> criteria, the
// offset window decoded from the cursor, load -> BuildDocument -> strip ->
// ToGoDoc, and the offset-encoded page/edge cursors.
func (r *ViewReader) ReadPage(ctx context.Context, name string, crit queries.ReadCriteria) (queries.Page, error) {
	v, ok := r.views[name]
	if !ok {
		return queries.Page{}, fmt.Errorf("relational view %q is not registered", name)
	}
	if crit.Search != "" {
		return queries.Page{}, unsupported("search")
	}
	where, err := toExpr(v.schema, crit.Filter)
	if err != nil {
		return queries.Page{}, err
	}

	if crit.OnlyTotal {
		n, err := v.loader.CountEntities(ctx, scopedQuery(where, crit.IncludeArchived))
		if err != nil {
			return queries.Page{}, err
		}
		return queries.Page{OnlyTotal: true, TotalCount: n}, nil
	}

	// MaxLimit cascade, identical to the Mongo reader: a consumer `?first=`/`?last=` over
	// the per-view ceiling is the canonical 400; an absent/zero limit defers to
	// the ceiling so every relational page is bounded. The trusted export wrapper
	// sets BypassMaxLimit to run its own (larger) maxExportRows ceiling verbatim.
	maxLimit := r.resolveMaxLimit(name)
	if !crit.BypassMaxLimit && crit.Limit > maxLimit {
		return queries.Page{}, core.LimitExceededError(maxLimit, crit.Backward || crit.Before != "")
	}
	limit := crit.Limit
	if limit <= 0 {
		limit = maxLimit
	}
	hashCtx := queries.HashContext(crit.Filter, crit.OrderBy, crit.Search, crit.IncludeArchived)

	// The listing total, counted under the SAME scoped criteria the OnlyTotal
	// branch above uses — so `?first=N` and `?onlyTotal=true` report the same
	// number by construction, exactly as they do on the Mongo reader.
	//
	// The count and the page fetch are independent queries, so they run
	// CONCURRENTLY instead of serializing one round trip after the other —
	// with ONE exception: the bare-backward window (`last=N`, no cursor)
	// anchors its offset ON the total, so that path counts first, exactly as
	// before. resolveWindow consumes the total only on that branch.
	type countResult struct {
		total int64
		err   error
	}
	countCh := make(chan countResult, 1)
	bareBackward := crit.Backward && crit.After == "" && crit.Before == ""
	var total int64
	if bareBackward {
		n, err := v.loader.CountEntities(ctx, scopedQuery(where, crit.IncludeArchived))
		if err != nil {
			return queries.Page{}, err
		}
		total = n
		countCh <- countResult{total: n}
	} else {
		go func() {
			n, err := v.loader.CountEntities(ctx, scopedQuery(where, crit.IncludeArchived))
			countCh <- countResult{total: n, err: err}
		}()
	}

	win, err := r.resolveWindow(crit, limit, total, hashCtx)
	if err != nil {
		return queries.Page{}, err
	}

	joinCount := func() (int64, error) {
		res := <-countCh
		return res.total, res.err
	}

	if win.fetchLimit <= 0 {
		// Zero-width window — paging `before` the very first row (offset 0), or a
		// backward page over an empty result. Return the empty page WITHOUT
		// issuing the query: q.Limit(0) renders as "no LIMIT clause" in
		// applyWindow, which would load the ENTIRE table into memory and bypass
		// MaxLimit. The has-more flags come straight from the resolved window.
		if total, err = joinCount(); err != nil {
			return queries.Page{}, err
		}
		return queries.Page{TotalCount: total, HasNextPage: win.hasNext, HasPreviousPage: win.hasPrev}, nil
	}

	q := scopedQuery(where, crit.IncludeArchived)
	if err := applySort(v.schema, q, crit.OrderBy); err != nil {
		// The buffered channel lets the in-flight count goroutine finish and
		// be collected without blocking anyone.
		return queries.Page{}, err
	}
	q.OrderBy(idGoField) // deterministic tiebreak — offset pages must be stable
	q.Offset(win.offset).Limit(win.fetchLimit)

	ents, err := v.loader.FindAllEntities(ctx, q)
	if err != nil {
		return queries.Page{}, err
	}
	if total, err = joinCount(); err != nil {
		return queries.Page{}, err
	}

	// Over-fetch: a fetched limit+1 row proves a further page exists in the fetch
	// direction; drop the probe row from the returned page.
	hasMore := win.overFetch && int64(len(ents)) > limit
	if hasMore {
		ents = ents[:limit]
	}

	page := queries.Page{
		Items:       make([]map[string]any, 0, len(ents)),
		ItemCursors: make([]string, 0, len(ents)),
		TotalCount:  total,
	}
	for j, ent := range ents {
		doc := BuildDocument(v.schema, ent)
		if !crit.IncludeArchived {
			v.node.StripArchivedChildren(doc)
		}
		goDoc := v.node.ToGoDoc(doc)
		applyProjection(goDoc, crit.Projection)
		page.Items = append(page.Items, goDoc)
		cur, err := queries.EncodeCursor(offsetTuple(win.offset+int64(j), len(crit.OrderBy)), hashCtx)
		if err != nil {
			return queries.Page{}, err
		}
		page.ItemCursors = append(page.ItemCursors, cur)
	}
	page.HasNextPage = win.hasNext
	if win.overFetch {
		page.HasNextPage = hasMore
	}
	page.HasPreviousPage = win.hasPrev
	// An EDGE cursor is emitted only where a neighbouring page actually exists —
	// EndCursor with HasNextPage, StartCursor with HasPreviousPage — which is the
	// rule the Mongo reader applies. Flipping the backing of a view must not
	// change the envelope a consumer parses: emitting an EndCursor on the last
	// page would contradict the HasNextPage=false sitting beside it, and a client
	// treating "a cursor is present" as "there is more" would spend it for an
	// empty page. ItemCursors stays fully populated either way — it addresses
	// ROWS, not page boundaries, so the GraphQL edges[].cursor of the final row
	// is still served.
	if len(page.ItemCursors) > 0 {
		if page.HasPreviousPage {
			page.StartCursor = page.ItemCursors[0]
		}
		if page.HasNextPage {
			page.EndCursor = page.ItemCursors[len(page.ItemCursors)-1]
		}
	}
	page.Projection = crit.Projection // echo the effective projection (export plan pruning)
	return page, nil
}

// applyProjection prunes a served Go-keyed document to the requested selection,
// in memory — the counterpart of the Mongo reader's server-side projection, and
// identical in outcome. ProjectOnly keeps ONLY the selected paths, so the root id
// is absent unless the consumer named it; a dotted path prunes INTO the child
// sub-document or array (`Parts.Label` keeps each `Parts` element with only
// `Label`). ProjectExcept deletes the listed leaves, nested ones included.
// ProjectAll is a no-op — the whole document.
func applyProjection(doc map[string]any, proj queries.Projection) {
	if len(doc) == 0 || !proj.Narrows() {
		return
	}
	if proj.IsInclusion() {
		keep := newProjTree()
		for path := range proj.Paths {
			keep.add(path)
		}
		pruneInclude(doc, keep)
		return
	}
	for path := range proj.Paths {
		excludePath(doc, strings.Split(path, "."))
	}
}

// projTree is a prefix tree of the included ?fields= paths. A node reached by a
// key that terminates there is "full" (keep the value whole); a node with
// children is a partial include (recurse, keeping only the listed sub-paths).
type projTree struct {
	full     bool
	children map[string]*projTree
}

func newProjTree() *projTree { return &projTree{children: map[string]*projTree{}} }

func (t *projTree) add(path string) {
	n := t
	for _, seg := range strings.Split(path, ".") {
		c, ok := n.children[seg]
		if !ok {
			c = newProjTree()
			n.children[seg] = c
		}
		n = c
	}
	n.full = true
}

// pruneInclude keeps only the doc keys present in the tree. A full (or childless)
// node keeps its value as-is; a partial node recurses into a child map or an
// array of maps, so a nested path prunes each element to the asked sub-fields. A
// sub-path asked on a scalar has nothing to descend into and is dropped, matching
// Mongo.
func pruneInclude(doc map[string]any, t *projTree) {
	for k := range doc {
		child, ok := t.children[k]
		if !ok {
			delete(doc, k)
			continue
		}
		if child.full || len(child.children) == 0 {
			continue
		}
		switch v := doc[k].(type) {
		case map[string]any:
			pruneInclude(v, child)
		case []any:
			for _, el := range v {
				if m, ok := el.(map[string]any); ok {
					pruneInclude(m, child)
				}
			}
		default:
			delete(doc, k)
		}
	}
}

// excludePath deletes the leaf at the dotted path, descending through child maps
// AND arrays of maps, so an exclusion `parts.label` drops `label` from every
// `parts` element.
func excludePath(node any, parts []string) {
	if len(parts) == 0 {
		return
	}
	switch v := node.(type) {
	case map[string]any:
		if len(parts) == 1 {
			delete(v, parts[0])
			return
		}
		if next, ok := v[parts[0]]; ok {
			excludePath(next, parts[1:])
		}
	case []any:
		for _, el := range v {
			excludePath(el, parts)
		}
	}
}

// ReadByID serves a by-id read: criteria.ByID (merged with any root-level
// security-overlay filter) + Limit(1), then the same doc mapping. Not-found is
// the empty slice, surfaced as (nil, false, nil) — never an error.
func (r *ViewReader) ReadByID(ctx context.Context, name, id string, crit queries.ReadCriteria) (map[string]any, bool, error) {
	v, ok := r.views[name]
	if !ok {
		return nil, false, fmt.Errorf("relational view %q is not registered", name)
	}
	where := criteria.Eq(idGoField, domain.NewID(id))
	overlay, err := toExpr(v.schema, crit.Filter)
	if err != nil {
		return nil, false, err
	}
	if overlay != nil {
		where = criteria.And(where, overlay)
	}
	q := scopedQuery(where, crit.IncludeArchived).Limit(1)
	ents, err := v.loader.FindAllEntities(ctx, q)
	if err != nil {
		return nil, false, err
	}
	if len(ents) == 0 {
		return nil, false, nil
	}
	doc := BuildDocument(v.schema, ents[0])
	if !crit.IncludeArchived {
		v.node.StripArchivedChildren(doc)
	}
	goDoc := v.node.ToGoDoc(doc)
	applyProjection(goDoc, crit.Projection)
	return goDoc, true, nil
}

// window is the resolved offset page: the SoR offset, the fetch limit (limit+1
// when over-fetching to probe hasNext forward), and the has-more flags.
type window struct {
	offset     int64
	fetchLimit int64
	overFetch  bool // forward: fetched limit+1, hasNext derived from the probe row
	hasNext    bool // used when !overFetch (the backward cases)
	hasPrev    bool
}

// resolveWindow turns the request's cursor/direction into an offset window. after
// walks forward from a cursor; before walks back from one; a bare backward page
// (GraphQL last:N with no before) anchors at the end on total, the row count the
// caller already holds; the default is the forward first page. Offsets are
// absolute row indexes, encoded in the cursor as a single int — the relational
// read carries no keyset tuple.
func (r *ViewReader) resolveWindow(crit queries.ReadCriteria, limit, total int64, hashCtx string) (window, error) {
	switch {
	case crit.After != "":
		pos, err := decodeOffset(crit.After, hashCtx)
		if err != nil {
			return window{}, err
		}
		off := pos + 1
		return window{offset: off, fetchLimit: limit + 1, overFetch: true, hasPrev: off > 0}, nil
	case crit.Before != "":
		pos, err := decodeOffset(crit.Before, hashCtx)
		if err != nil {
			return window{}, err
		}
		off := max(int64(0), pos-limit)
		return window{offset: off, fetchLimit: pos - off, hasNext: true, hasPrev: off > 0}, nil
	case crit.Backward:
		off := max(int64(0), total-limit)
		return window{offset: off, fetchLimit: total - off, hasNext: false, hasPrev: off > 0}, nil
	default:
		return window{offset: 0, fetchLimit: limit + 1, overFetch: true, hasPrev: false}, nil
	}
}

// offsetTuple encodes the absolute row offset a relational cursor carries in a
// tuple shaped like the Mongo keyset cursor the WIRE layer validates: its
// length must be len(sort)+1 (queryschema.BuildCriteria asserts
// len(K)-1 == len(sort), the trailing slot being the keyset _id). The offset
// lives in K[0]; the remaining slots are inert padding, so an incoming cursor
// passes the structural check on every surface while the reader only ever reads
// K[0]. sortLen is len(crit.OrderBy) at encode time.
func offsetTuple(offset int64, sortLen int) []any {
	k := make([]any, sortLen+1)
	k[0] = offset
	for i := 1; i <= sortLen; i++ {
		k[i] = 0
	}
	return k
}

// decodeOffset reads the absolute-index int a relational cursor carries (K[0]),
// after checking its context hash matches the current filter/sort — a mismatch
// means the listing context changed mid-navigation, so the cursor is rejected
// exactly as the Mongo keyset path rejects a stale one. The tuple length is not
// re-checked here: the wire layer already asserted len(K)-1 == len(sort).
func decodeOffset(cur, hashCtx string) (int64, error) {
	c, err := queries.DecodeCursor(cur)
	if err != nil {
		return 0, core.InvalidCursorError(fmt.Errorf("invalid cursor: %w", err))
	}
	if c.H != hashCtx {
		return 0, core.InvalidCursorError(errors.New("cursor context hash does not match current criteria"))
	}
	if len(c.K) == 0 {
		return 0, core.InvalidCursorError(errors.New("cursor tuple is empty"))
	}
	f, ok := c.K[0].(float64) // the cursor tuple round-trips through JSON
	if !ok || f < 0 {
		return 0, core.InvalidCursorError(errors.New("cursor does not carry a valid row offset"))
	}
	return int64(f), nil
}

// scopedQuery builds a criteria.Query with the WHERE and the archived scope — the
// shared prefix of every relational read (page window, by-id, count).
func scopedQuery(where criteria.Expr, includeArchived bool) *criteria.Query {
	q := criteria.Where(where)
	if includeArchived {
		q.IncludeArchived()
	}
	return q
}
