package relational

import (
	"context"
	"fmt"

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

// defaultPageLimit caps a page when the request carries no ?limit. The per-view
// MaxLimit ceiling is an operational refinement layered on top later; this is the
// floor so an unbounded relational read never happens.
const defaultPageLimit int64 = 100

// view is one relational-backed view's read state: the root schema (drives the
// criteria field resolution + the doc mapping), the ViewNode (column<->Go
// translation + the archived-child strip) and the aggregate loader the view
// carries (repo.Loader, handed in at declaration via RelationalSource).
type view struct {
	schema *core.TableSchema
	node   *query.ViewNode
	loader query.RelationalReader
}

// RelationalViewReader implements queries.ViewReader by reading a marked view
// directly from the relational backend (SoR) instead of the Mongo projection: it
// loads the aggregate through the view's loader, maps it to the same column-keyed
// document a Mongo-backed view stores (BuildDocument), then applies the same
// ViewNode strip + Go translation, so the four web surfaces read it identically.
// Pagination is offset-in-cursor (skip/count), not keyset — the relational
// idiom — behind the unchanged after/before/limit surface.
type RelationalViewReader struct {
	views map[string]view
}

var _ queries.ViewReader = (*RelationalViewReader)(nil)

// NewRelationalViewReader builds the reader from the collected views, indexing
// only the RelationalSource() ones by name. Views without the marker are served
// by the Mongo reader and never reach here.
func NewRelationalViewReader(views []*query.ViewDefinition) *RelationalViewReader {
	r := &RelationalViewReader{views: make(map[string]view)}
	for _, v := range views {
		if !v.IsRelational() {
			continue
		}
		r.views[v.Name()] = view{
			schema: v.SchemaDef(),
			node:   v.BuildViewNode(),
			loader: v.RelationalReader(),
		}
	}
	return r
}

// Views reports whether any relational view is registered — the wiring installs
// this reader into the dispatch seam only when there is one.
func (r *RelationalViewReader) Empty() bool { return len(r.views) == 0 }

// ReadPage serves a paged read of a relational view: filter -> criteria, the
// offset window decoded from the cursor, load -> BuildDocument -> strip ->
// ToGoDoc, and the offset-encoded page/edge cursors.
func (r *RelationalViewReader) ReadPage(ctx context.Context, name string, crit queries.ReadCriteria) (queries.Page, error) {
	v, ok := r.views[name]
	if !ok {
		return queries.Page{}, fmt.Errorf("relational view %q is not registered", name)
	}
	if crit.Search != "" {
		return queries.Page{}, unsupported("search")
	}
	where, err := toExpr(crit.Filter)
	if err != nil {
		return queries.Page{}, err
	}

	if crit.OnlyTotal {
		n, err := v.loader.CountEntities(ctx, scopedQuery(where, crit.IncludeArchived))
		if err != nil {
			return queries.Page{}, err
		}
		return queries.Page{OnlyTotal: true, Total: n}, nil
	}

	limit := crit.Limit
	if limit <= 0 {
		limit = defaultPageLimit
	}
	hashCtx := queries.HashContext(crit.Filter, crit.Sort, crit.Search, crit.IncludeArchived)

	win, err := r.resolveWindow(ctx, v, crit, where, limit, hashCtx)
	if err != nil {
		return queries.Page{}, err
	}

	q := scopedQuery(where, crit.IncludeArchived)
	if err := applySort(q, crit.Sort); err != nil {
		return queries.Page{}, err
	}
	q.OrderBy(idGoField) // deterministic tiebreak — offset pages must be stable
	q.Offset(win.offset).Limit(win.fetchLimit)

	ents, err := v.loader.FindAllEntities(ctx, q)
	if err != nil {
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
	}
	for j, ent := range ents {
		doc := BuildDocument(v.schema, ent)
		promoteID(doc, v.schema)
		if !crit.IncludeArchived {
			v.node.StripArchivedChildren(doc)
		}
		page.Items = append(page.Items, v.node.ToGoDoc(doc))
		cur, err := queries.EncodeCursor([]any{win.offset + int64(j)}, hashCtx)
		if err != nil {
			return queries.Page{}, err
		}
		page.ItemCursors = append(page.ItemCursors, cur)
	}
	if len(page.ItemCursors) > 0 {
		page.PrevCursor = page.ItemCursors[0]
		page.NextCursor = page.ItemCursors[len(page.ItemCursors)-1]
	}
	page.HasNext = win.hasNext
	if win.overFetch {
		page.HasNext = hasMore
	}
	page.HasPrev = win.hasPrev
	return page, nil
}

// ReadByID serves a by-id read: criteria.ByID (merged with any root-level
// security-overlay filter) + Limit(1), then the same doc mapping. Not-found is
// the empty slice, surfaced as (nil, false, nil) — never an error.
func (r *RelationalViewReader) ReadByID(ctx context.Context, name, id string, crit queries.ReadCriteria) (map[string]any, bool, error) {
	v, ok := r.views[name]
	if !ok {
		return nil, false, fmt.Errorf("relational view %q is not registered", name)
	}
	where := criteria.Eq(idGoField, domain.NewID(id))
	overlay, err := toExpr(crit.Filter)
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
	promoteID(doc, v.schema)
	if !crit.IncludeArchived {
		v.node.StripArchivedChildren(doc)
	}
	return v.node.ToGoDoc(doc), true, nil
}

// promoteID mirrors the Mongo storage transform onto the freshly built
// column-keyed document: ApplyProjection stores the aggregate under {_id: id}
// while KEEPING the physical id column, so a Mongo-read document carries both.
// BuildDocument matches the composer's column-keyed shape (physical id only, so
// the parity test holds), so the reader adds `_id` here — after the build, before
// ToGoDoc — to make a relational-served document identical to a Mongo-served one
// (ToGoDoc passes `_id` through and maps the physical id column to "ID"). Only
// the root gets an `_id`; children stay sub-documents keyed by their physical id.
func promoteID(doc query.Document, schema *core.TableSchema) {
	if idCol := schema.IDColumn(); idCol != "" {
		if v, ok := doc[idCol]; ok {
			doc["_id"] = v
		}
	}
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
// (GraphQL last:N with no before) anchors at the end via one COUNT; the default
// is the forward first page. Offsets are absolute row indexes, encoded in the
// cursor as a single int — the relational read carries no keyset tuple.
func (r *RelationalViewReader) resolveWindow(ctx context.Context, v view, crit queries.ReadCriteria, where criteria.Expr, limit int64, hashCtx string) (window, error) {
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
		total, err := v.loader.CountEntities(ctx, scopedQuery(where, crit.IncludeArchived))
		if err != nil {
			return window{}, err
		}
		off := max(int64(0), total-limit)
		return window{offset: off, fetchLimit: total - off, hasNext: false, hasPrev: off > 0}, nil
	default:
		return window{offset: 0, fetchLimit: limit + 1, overFetch: true, hasPrev: false}, nil
	}
}

// decodeOffset reads the absolute-index int a relational cursor carries, after
// checking its context hash matches the current filter/sort — a mismatch means
// the listing context changed mid-navigation, so the cursor is rejected exactly
// as the Mongo keyset path rejects a stale one.
func decodeOffset(cur, hashCtx string) (int64, error) {
	c, err := queries.DecodeCursor(cur)
	if err != nil {
		return 0, err
	}
	if c.H != hashCtx {
		return 0, queries.ErrCursorInvalid
	}
	if len(c.K) != 1 {
		return 0, queries.ErrCursorInvalid
	}
	f, ok := c.K[0].(float64) // the cursor tuple round-trips through JSON
	if !ok || f < 0 {
		return 0, queries.ErrCursorInvalid
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
