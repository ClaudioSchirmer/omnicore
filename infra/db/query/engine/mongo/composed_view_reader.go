package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// legFetchConcurrency bounds the parallel per-parent fetches of a LinkMany
// leg. A page of N parents issues N small, indexed, limit-capped finds; this
// cap keeps the burst bounded regardless of the page size.
const legFetchConcurrency = 8

// ComposedViewReader decorates MongoViewReader with the read-time composition
// the ComposedViewDefinition declares. It satisfies the same
// queries.ViewReader port: a read against a composed name reads the PRIMARY
// view through the wrapped reader — the full existing pipeline (allowlist
// already validated at the web layer, keyset pagination, context hash,
// MaxLimit cascade, archived gate) runs untouched — then enriches each
// returned item with the declared legs, fetched by key in batch and joined in
// Go code. A non-composed name passes through verbatim, so the decorator is
// transparent to every regular view.
//
// Semantics (the composed-view contract):
//   - Pagination, sort, search, total and cursors are the primary's,
//     byte-for-byte. Legs never add, remove or reorder primary rows.
//   - A filter addressed to a leg segment (Go path "Notes.Text") filters what
//     enters the segment, never which primary rows appear.
//   - LEFT join always: a 1:1 segment is an explicit null when absent, a 1:N
//     segment an empty array.
//   - A sort path into a leg segment is rejected with the canonical Schema
//     violation (400) — segment order is declared on the link, not wire-set.
//   - `?includeArchived` propagates to the primary and every leg; a leg whose
//     schema declares no soft-delete has no gate (the knob is a no-op there).
//   - onlyTotal short-circuits before any leg is fetched.
//
// Cursor context: the composed listing context includes the segment filters
// (they shape the page content), so the decorator validates incoming cursors
// against the FULL criteria hash and stamps outgoing cursors with it, while
// the wrapped reader keeps operating on the primary-only criteria hash
// internally — the decorator translates between the two at its boundary.
type ComposedViewReader struct {
	inner    *MongoViewReader
	composed map[string]*composedRuntime
}

type composedRuntime struct {
	primary string
	legs    []*legRuntime
	bySeg   map[string]*legRuntime
}

type legRuntime struct {
	link     query.ComposedLink
	node     *query.ViewNode
	maxItems int64 // resolved LinkMany ceiling (1:1 legs: unused)
}

// NewComposedViewReader wraps inner with the given composed-view definitions.
// yamlMaxLinkManyLimit is the yaml default (query.maxLinkManyLimit) consulted
// by the per-link ceiling cascade; the framework constant fallback lives in
// query.FrameworkDefaultMaxLinkManyLimit. Call only with boot-validated
// definitions (query.ValidateComposedViews).
func NewComposedViewReader(inner *MongoViewReader, defs []*query.ComposedViewDefinition, yamlMaxLinkManyLimit int64) *ComposedViewReader {
	r := &ComposedViewReader{inner: inner, composed: make(map[string]*composedRuntime, len(defs))}
	for _, def := range defs {
		rt := &composedRuntime{
			primary: def.PrimaryView().Name(),
			bySeg:   map[string]*legRuntime{},
		}
		for _, link := range def.Links() {
			leg := &legRuntime{
				link:     link,
				node:     link.Node(),
				maxItems: link.ResolveMaxLinkManyLimit(yamlMaxLinkManyLimit),
			}
			rt.legs = append(rt.legs, leg)
			rt.bySeg[link.GoSegment] = leg
		}
		r.composed[def.Name()] = rt
	}
	return r
}

// IsComposed reports whether the given read-side name is a registered
// composed view. The wrapped MongoViewReader consults it to delegate composed
// names while keeping every regular read on its own path (the delegation is
// loop-free: a composed read's primary fetch re-enters under the primary
// view's non-composed name).
func (r *ComposedViewReader) IsComposed(view string) bool {
	_, ok := r.composed[view]
	return ok
}

func (r *ComposedViewReader) ReadPage(ctx context.Context, view string, c queries.ReadCriteria) (queries.Page, error) {
	rt, ok := r.composed[view]
	if !ok {
		return r.inner.ReadPage(ctx, view, c)
	}

	// R3 — sort belongs to the primary. A path into a leg segment is the
	// canonical Schema rejection (400), same notification the wire allowlist
	// emits; surfaces that do not pre-validate (GraphQL, manual criteria)
	// reach it here.
	for _, sf := range c.Sort {
		if head, _, _ := strings.Cut(sf.Field, "."); rt.bySeg[head] != nil {
			return queries.Page{}, core.SingleNotificationError("Schema", "sort", domain.SchemaViolationNotification{})
		}
	}

	split := splitComposedCriteria(rt, c)

	// The composed listing context (segment filters included) is what incoming
	// cursors were stamped with; the wrapped reader speaks the primary-only
	// context. Validate against the full hash, then translate for the inner.
	fullHash := queries.HashContext(c.Filter, c.Sort, c.Search, c.IncludeArchived)
	primaryHash := queries.HashContext(split.primary.Filter, c.Sort, c.Search, c.IncludeArchived)
	if err := rewriteCursorIn(&split.primary.After, fullHash, primaryHash); err != nil {
		return queries.Page{}, err
	}
	if err := rewriteCursorIn(&split.primary.Before, fullHash, primaryHash); err != nil {
		return queries.Page{}, err
	}

	page, err := r.inner.ReadPage(ctx, rt.primary, split.primary)
	if err != nil {
		return queries.Page{}, err
	}
	if page.OnlyTotal {
		return page, nil
	}

	if err := r.attachLegs(ctx, rt, split, page.Items, c.IncludeArchived); err != nil {
		return queries.Page{}, err
	}
	stripHelperFields(page.Items, split)

	if fullHash != primaryHash {
		if err := rewriteCursorsOut(&page, fullHash); err != nil {
			return queries.Page{}, err
		}
	}
	page.Projection = c.Projection // echo the composed projection for export plan pruning
	return page, nil
}

func (r *ComposedViewReader) ReadByID(ctx context.Context, view, id string, c queries.ReadCriteria) (map[string]any, bool, error) {
	rt, ok := r.composed[view]
	if !ok {
		return r.inner.ReadByID(ctx, view, id, c)
	}
	split := splitComposedCriteria(rt, c)
	doc, found, err := r.inner.ReadByID(ctx, rt.primary, id, split.primary)
	if err != nil || !found {
		return nil, false, err
	}
	items := []map[string]any{doc}
	if err := r.attachLegs(ctx, rt, split, items, c.IncludeArchived); err != nil {
		return nil, false, err
	}
	stripHelperFields(items, split)
	return items[0], true, nil
}

// composedSplit is the outcome of routing one ReadCriteria between the primary
// and the legs: the primary-only criteria, the per-leg segment filters and
// projections, which legs the projection keeps, and the helper fields the
// primary projection had to carry for the joins (stripped post-attach).
type composedSplit struct {
	primary    queries.ReadCriteria
	legFilters map[string]map[string]any
	legProj    map[string]map[string]int
	fetchLeg   map[string]bool
	stripID    bool
	stripKeys  []string
}

// splitComposedCriteria routes filters and projection entries by their first
// Go path segment: entries addressing a leg segment go to that leg; everything
// else stays on the primary. It also guarantees the primary projection carries
// the join keys the attach step needs ("_id" and each 1:1 parent FK field),
// remembering what it added so the wire shape is restored afterwards.
func splitComposedCriteria(rt *composedRuntime, c queries.ReadCriteria) *composedSplit {
	s := &composedSplit{
		primary:    c,
		legFilters: map[string]map[string]any{},
		legProj:    map[string]map[string]int{},
		fetchLeg:   map[string]bool{},
	}

	if len(c.Filter) > 0 {
		primaryFilter := make(map[string]any, len(c.Filter))
		for k, v := range c.Filter {
			if head, rest, dotted := strings.Cut(k, "."); dotted && rt.bySeg[head] != nil {
				lf := s.legFilters[head]
				if lf == nil {
					lf = map[string]any{}
					s.legFilters[head] = lf
				}
				lf[rest] = v
				continue
			}
			primaryFilter[k] = v
		}
		s.primary.Filter = primaryFilter
	}

	// Projection routing. buildCriteria produces inclusion entries (+ the
	// `_id:0` auto-exclusion); ToCriteria may add exclusions (Restrict).
	//   - whole-doc (nil/empty)      → every leg attaches, untouched primary.
	//   - inclusion mode             → a leg attaches when its segment (or a
	//     sub-path) is included; the others are omitted from the wire shape.
	//   - exclusion-only mode        → every leg attaches except an explicitly
	//     excluded segment.
	legTouched := map[string]bool{}
	legIncluded := map[string]bool{}
	legExcluded := map[string]bool{}
	inclusionMode := false
	if len(c.Projection) > 0 {
		primaryProj := make(map[string]int, len(c.Projection))
		for k, v := range c.Projection {
			if leg := rt.bySeg[k]; leg != nil {
				legTouched[k] = true
				if v == 1 {
					legIncluded[k] = true
					inclusionMode = true
				} else {
					legExcluded[k] = true
				}
				continue
			}
			if head, rest, dotted := strings.Cut(k, "."); dotted && rt.bySeg[head] != nil {
				lp := s.legProj[head]
				if lp == nil {
					lp = map[string]int{}
					s.legProj[head] = lp
				}
				lp[rest] = v
				legTouched[head] = true
				if v == 1 {
					legIncluded[head] = true
					inclusionMode = true
				}
				continue
			}
			if v == 1 && k != "_id" {
				inclusionMode = true
			}
			primaryProj[k] = v
		}
		s.primary.Projection = primaryProj
	}

	for _, leg := range rt.legs {
		seg := leg.link.GoSegment
		switch {
		case len(c.Projection) == 0:
			s.fetchLeg[seg] = true
		case legExcluded[seg]:
			// explicitly dropped
		case inclusionMode:
			s.fetchLeg[seg] = legIncluded[seg]
		default:
			// exclusion-only projection: untouched legs attach
			s.fetchLeg[seg] = !legTouched[seg]
		}
	}

	ensureJoinKeys(rt, s)
	return s
}

// ensureJoinKeys guarantees the primary projection still carries every field
// the attach step joins on: "_id" (1:N parents and PK-joined 1:1 legs) and the
// Go field of each 1:1 parent FK. Added or un-excluded entries are recorded so
// stripHelperFields restores the consumer's exact wire shape afterwards.
func ensureJoinKeys(rt *composedRuntime, s *composedSplit) {
	proj := s.primary.Projection
	if len(proj) == 0 {
		return
	}
	needsID := false
	needKeys := map[string]bool{}
	for _, leg := range rt.legs {
		if !s.fetchLeg[leg.link.GoSegment] {
			continue
		}
		if leg.link.ParentKeyGoField == "_id" {
			needsID = true
		} else {
			needKeys[leg.link.ParentKeyGoField] = true
		}
	}
	if !needsID && len(needKeys) == 0 {
		return
	}
	inclusion := false
	for k, v := range proj {
		if v == 1 && k != "_id" {
			inclusion = true
			break
		}
	}
	if needsID {
		if v, present := proj["_id"]; present && v == 0 {
			delete(proj, "_id")
			s.stripID = true
		}
	}
	for k := range needKeys {
		v, present := proj[k]
		switch {
		case inclusion && (!present || v == 0):
			proj[k] = 1
			s.stripKeys = append(s.stripKeys, k)
		case !inclusion && present && v == 0:
			delete(proj, k)
			s.stripKeys = append(s.stripKeys, k)
		}
	}
}

// stripHelperFields removes the join-key fields ensureJoinKeys added so the
// items match the consumer's requested projection exactly.
func stripHelperFields(items []map[string]any, s *composedSplit) {
	if !s.stripID && len(s.stripKeys) == 0 {
		return
	}
	for _, item := range items {
		if s.stripID {
			delete(item, "_id")
		}
		for _, k := range s.stripKeys {
			delete(item, k)
		}
	}
}

// attachLegs fetches every kept leg for the given items and attaches the
// segments in place. 1:1 legs resolve in ONE $in find per leg; 1:N legs run
// one small indexed, capped, ordered find per parent, concurrency-bounded.
func (r *ComposedViewReader) attachLegs(ctx context.Context, rt *composedRuntime, s *composedSplit, items []map[string]any, includeArchived bool) error {
	if len(items) == 0 {
		return nil
	}
	for _, leg := range rt.legs {
		if !s.fetchLeg[leg.link.GoSegment] {
			continue
		}
		var err error
		if leg.link.Many {
			err = r.attachMany(ctx, leg, s, items, includeArchived)
		} else {
			err = r.attachOne(ctx, leg, s, items, includeArchived)
		}
		if err != nil {
			return fmt.Errorf("composed view: leg %q: %w", leg.link.GoSegment, err)
		}
	}
	return nil
}

// legBaseFilter assembles the leg's Mongo filter shared by every fetch of one
// request: the translated segment filters plus the leg's own soft-delete gate
// (a leg without soft-delete has no gate — the includeArchived knob is a
// no-op there, never an error).
func (r *ComposedViewReader) legBaseFilter(leg *legRuntime, s *composedSplit, includeArchived bool) bson.M {
	filter := bson.M{}
	if lf := s.legFilters[leg.link.GoSegment]; len(lf) > 0 {
		applyFilter(filter, translateFilterKeys(leg.node, lf))
	}
	if sdCol, sdOn := leg.node.SoftDeleteColumn(); sdOn && !includeArchived {
		filter[sdCol] = nil
	}
	return filter
}

// legProjection translates the segment's sparse projection. A partial
// inclusion hides the leg doc's `_id` from the wire (the consumer asked for
// specific leaves) — but the 1:1 attach step GROUPS by `_id`, so the column
// stays in the Mongo projection and is stripped from the translated doc after
// grouping (stripID reports that). Whole-segment attaches keep the full doc,
// `_id` included, exactly like a direct read of the leg view.
func (r *ComposedViewReader) legProjection(leg *legRuntime, s *composedSplit) (proj bson.D, stripID bool) {
	lp := s.legProj[leg.link.GoSegment]
	if len(lp) == 0 {
		return nil, false
	}
	colProj := translateProjectionKeys(leg.node, lp)
	inclusion := false
	for _, v := range colProj {
		if v == 1 {
			inclusion = true
			break
		}
	}
	if inclusion {
		// Keep _id queryable for the join; hide it from the wire post-attach.
		colProj["_id"] = 1
		stripID = true
	}
	return buildProjection(colProj, nil), stripID
}

// attachOne resolves a 1:1 leg: one find({_id: {$in: keys}}) carrying the
// segment filters, the leg's archived gate and the segment projection; the
// matches group by _id and attach as sub-documents — an explicit null when
// absent (LEFT semantics, and the shape a filtered-out match leaves behind).
func (r *ComposedViewReader) attachOne(ctx context.Context, leg *legRuntime, s *composedSplit, items []map[string]any, includeArchived bool) error {
	keyField := leg.link.ParentKeyGoField
	var keys []any
	seen := map[string]bool{}
	for _, item := range items {
		v, present := item[keyField]
		if !present || v == nil {
			continue
		}
		k := fmt.Sprintf("%v", v)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, v)
		}
	}

	byKey := map[string]map[string]any{}
	if len(keys) > 0 {
		filter := r.legBaseFilter(leg, s, includeArchived)
		filter["_id"] = bson.M{"$in": keys}
		findOpts := options.Find().SetLimit(int64(len(keys)))
		proj, stripLegID := r.legProjection(leg, s)
		if proj != nil {
			findOpts.SetProjection(proj)
		}
		col := r.inner.mongo.collFn(r.inner.resolver.Active(leg.link.Collection).String())
		cur, err := col.Find(ctx, filter, findOpts)
		if err != nil {
			return err
		}
		var docs []bson.M
		if err := cur.All(ctx, &docs); err != nil {
			cur.Close(ctx)
			return err
		}
		cur.Close(ctx)
		for _, d := range docs {
			m := map[string]any(d)
			key := fmt.Sprintf("%v", m["_id"])
			goDoc := r.toGoLegDoc(leg, m, includeArchived)
			if stripLegID {
				delete(goDoc, "_id")
			}
			byKey[key] = goDoc
		}
	}

	for _, item := range items {
		var match any // explicit null when absent
		if v, present := item[keyField]; present && v != nil {
			if doc, found := byKey[fmt.Sprintf("%v", v)]; found {
				match = doc
			}
		}
		item[leg.link.GoSegment] = match
	}
	return nil
}

// attachMany resolves a 1:N leg: one small find per parent — indexed on the
// leg FK, sorted by the declared order (+ _id tiebreaker, the same stable-sort
// rule every reader query follows), capped at the resolved per-parent ceiling
// (deterministic silent truncation: "the first N in the declared order") —
// with the fetches concurrency-bounded. Empty array when nothing matches.
func (r *ComposedViewReader) attachMany(ctx context.Context, leg *legRuntime, s *composedSplit, items []map[string]any, includeArchived bool) error {
	base := r.legBaseFilter(leg, s, includeArchived)
	proj, stripLegID := r.legProjection(leg, s)

	var legSort []queries.SortField
	if leg.link.OrderByColumn != "" {
		legSort = []queries.SortField{{Field: leg.link.OrderByColumn, Desc: leg.link.OrderByDesc}}
	}
	sortDoc := buildStableSortDoc(legSort, false)

	col := r.inner.mongo.collFn(r.inner.resolver.Active(leg.link.Collection).String())
	sem := make(chan struct{}, legFetchConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, item := range items {
		item := item
		parentKey, present := item["_id"]
		if !present || parentKey == nil {
			item[leg.link.GoSegment] = []any{}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			filter := bson.M{leg.link.FKColumn: parentKey}
			for k, v := range base {
				filter[k] = v
			}
			findOpts := options.Find().SetLimit(leg.maxItems).SetSort(sortDoc)
			if proj != nil {
				findOpts.SetProjection(proj)
			}
			cur, err := col.Find(ctx, filter, findOpts)
			if err == nil {
				var docs []bson.M
				err = cur.All(ctx, &docs)
				cur.Close(ctx)
				if err == nil {
					out := make([]any, 0, len(docs))
					for _, d := range docs {
						goDoc := r.toGoLegDoc(leg, map[string]any(d), includeArchived)
						if stripLegID {
							delete(goDoc, "_id")
						}
						out = append(out, goDoc)
					}
					mu.Lock()
					item[leg.link.GoSegment] = out
					mu.Unlock()
					return
				}
			}
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return firstErr
}

// toGoLegDoc applies to one leg document the same membrane a direct read of
// the leg view applies: BSON value normalization, the default-read strip of
// archived nested children, and the column→Go translation.
func (r *ComposedViewReader) toGoLegDoc(leg *legRuntime, m map[string]any, includeArchived bool) map[string]any {
	normalizeBSONValues(m)
	if !includeArchived {
		leg.node.StripArchivedChildren(m)
	}
	return leg.node.ToGoDoc(m)
}

// rewriteCursorIn validates an incoming cursor against the composed (full)
// context hash and re-stamps it with the primary-only hash the wrapped reader
// validates internally. Surfaces that pre-validate (the REST wrapper) never
// fail here; the ones that do not (GraphQL, hand-rolled criteria) get the
// same protection the wrapper gives — never silently navigating a keyset
// against a different result set.
func rewriteCursorIn(cursorStr *string, fullHash, primaryHash string) error {
	if *cursorStr == "" || fullHash == primaryHash {
		return nil
	}
	cur, err := queries.DecodeCursor(*cursorStr)
	if err != nil {
		return core.InvalidCursorError(fmt.Errorf("invalid cursor: %w", err))
	}
	if cur.H != fullHash {
		return core.InvalidCursorError(errors.New("cursor context hash does not match current criteria"))
	}
	rewritten, err := queries.EncodeCursor(cur.K, primaryHash)
	if err != nil {
		return core.InvalidCursorError(err)
	}
	*cursorStr = rewritten
	return nil
}

// rewriteCursorsOut re-stamps every cursor the primary read issued with the
// composed (full) context hash, so the cursors round-trip through the wire
// pre-validation against the criteria the consumer actually sent.
func rewriteCursorsOut(page *queries.Page, fullHash string) error {
	restamp := func(s string) (string, error) {
		if s == "" {
			return s, nil
		}
		cur, err := queries.DecodeCursor(s)
		if err != nil {
			return "", err
		}
		return queries.EncodeCursor(cur.K, fullHash)
	}
	var err error
	if page.NextCursor, err = restamp(page.NextCursor); err != nil {
		return err
	}
	if page.PrevCursor, err = restamp(page.PrevCursor); err != nil {
		return err
	}
	for i, s := range page.ItemCursors {
		if page.ItemCursors[i], err = restamp(s); err != nil {
			return err
		}
	}
	return nil
}

var _ queries.ViewReader = (*ComposedViewReader)(nil)
