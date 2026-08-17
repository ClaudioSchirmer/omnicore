package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

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
//     schema declares no DeletedAt has no gate (the knob is a no-op there).
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
	// segKey is the Go path a filter/projection/sort addresses this leg by: the
	// leg's GoSegment for a root Link/LinkMany, or "<ChildSegment>.<GoSegment>"
	// for a LinkInChild (two levels deep — inside a primary child array).
	segKey string
}

// segMatch finds the leg a Go path addresses: the longest dotted-prefix of path
// that is a registered leg segKey. Returns the leg, the remaining sub-path, and
// whether a leg matched. "Notes.Text" → (Notes leg, "Text"); "Lines.Item.Label"
// → (LinkInChild leg keyed "Lines.Item", "Label"); a primary-only path → no match.
func (rt *composedRuntime) segMatch(path string) (*legRuntime, string, bool) {
	parts := strings.Split(path, ".")
	for n := len(parts); n >= 1; n-- {
		key := strings.Join(parts[:n], ".")
		if leg := rt.bySeg[key]; leg != nil {
			return leg, strings.Join(parts[n:], "."), true
		}
	}
	return nil, "", false
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
			segKey := link.GoSegment
			if link.InChild() {
				segKey = link.ChildSegment + "." + link.GoSegment
			}
			leg := &legRuntime{
				link:     link,
				node:     link.Node(),
				maxItems: link.ResolveMaxLinkManyLimit(yamlMaxLinkManyLimit),
				segKey:   segKey,
			}
			rt.legs = append(rt.legs, leg)
			rt.bySeg[segKey] = leg
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
	for _, sf := range c.OrderBy {
		if _, _, ok := rt.segMatch(sf.Field); ok {
			return queries.Page{}, core.SingleNotificationError("Schema", "sort", domain.SchemaViolationNotification{})
		}
	}

	split := splitComposedCriteria(rt, c)

	// The composed listing context (segment filters included) is what incoming
	// cursors were stamped with; the wrapped reader speaks the primary-only
	// context. Validate against the full hash, then translate for the inner.
	fullHash := queries.HashContext(c.Filter, c.OrderBy, c.Search, c.IncludeArchived)
	primaryHash := queries.HashContext(split.primary.Filter, c.OrderBy, c.Search, c.IncludeArchived)
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
	// stripChildFK names the (childSegment, elementFKGoField) pairs ensureJoinKeys
	// force-included on the primary so a LinkInChild could join a sparse
	// projection; stripHelperFields removes them from each child element after.
	stripChildFK []childFKStrip
}

type childFKStrip struct{ seg, field string }

// splitComposedCriteria routes filters and projection entries by their first
// Go path segment: entries addressing a leg segment go to that leg; everything
// else stays on the primary. It also guarantees the primary projection carries
// the join keys the attach step needs ("_id" and each 1:1 parent ParentID field),
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
			if leg, rest, ok := rt.segMatch(k); ok && rest != "" {
				lf := s.legFilters[leg.segKey]
				if lf == nil {
					lf = map[string]any{}
					s.legFilters[leg.segKey] = lf
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
			if leg, rest, ok := rt.segMatch(k); ok {
				sk := leg.segKey
				if rest == "" { // whole segment included/excluded
					legTouched[sk] = true
					if v == 1 {
						legIncluded[sk] = true
						inclusionMode = true
					} else {
						legExcluded[sk] = true
					}
					continue
				}
				lp := s.legProj[sk] // a sub-path into the segment
				if lp == nil {
					lp = map[string]int{}
					s.legProj[sk] = lp
				}
				lp[rest] = v
				legTouched[sk] = true
				if v == 1 {
					legIncluded[sk] = true
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
		seg := leg.segKey
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
// the attach step joins on: "_id" (1:N parents and ID-joined 1:1 legs) and the
// Go field of each 1:1 parent ParentID. Added or un-excluded entries are recorded so
// stripHelperFields restores the consumer's exact wire shape afterwards.
func ensureJoinKeys(rt *composedRuntime, s *composedSplit) {
	proj := s.primary.Projection
	if len(proj) == 0 {
		return
	}
	needsID := false
	needKeys := map[string]bool{}
	needChild := map[string]bool{} // "childSeg.parentIDGoField" nested join paths (LinkInChild)
	for _, leg := range rt.legs {
		if !s.fetchLeg[leg.segKey] {
			continue
		}
		if leg.link.InChild() {
			needChild[leg.link.ChildSegment+"."+leg.link.FKGoField] = true
			continue
		}
		if leg.link.ParentKeyGoField == "_id" {
			needsID = true
		} else {
			needKeys[leg.link.ParentKeyGoField] = true
		}
	}
	if !needsID && len(needKeys) == 0 && len(needChild) == 0 {
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
	// LinkInChild joins per child element: the child array + the element's ParentID Go
	// field must survive a sparse projection so attachInChild can look the leg up.
	for path := range needChild {
		seg, field, _ := strings.Cut(path, ".")
		v, present := proj[path]
		switch {
		case inclusion && (!present || v == 0):
			proj[path] = 1
			s.stripChildFK = append(s.stripChildFK, childFKStrip{seg, field})
		case !inclusion && present && v == 0:
			delete(proj, path)
			s.stripChildFK = append(s.stripChildFK, childFKStrip{seg, field})
		}
	}
}

// stripHelperFields removes the join-key fields ensureJoinKeys added so the
// items match the consumer's requested projection exactly.
func stripHelperFields(items []map[string]any, s *composedSplit) {
	if !s.stripID && len(s.stripKeys) == 0 && len(s.stripChildFK) == 0 {
		return
	}
	for _, item := range items {
		if s.stripID {
			delete(item, "_id")
		}
		for _, k := range s.stripKeys {
			delete(item, k)
		}
		for _, cs := range s.stripChildFK {
			for _, el := range childElems(item[cs.seg]) {
				delete(el, cs.field)
			}
		}
	}
}

// childElems normalizes a child-array value (as ToGoDoc produces it) into the
// map elements the composer/attach step edits in place.
func childElems(v any) []map[string]any {
	switch arr := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(arr))
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return arr
	default:
		return nil
	}
}

// attachLegs fetches every kept leg for the given items and attaches the
// segments in place. 1:1 legs resolve in ONE $in find per leg; 1:N legs in
// ONE $in-matched aggregation per leg, grouped per parent with the ordered,
// capped $topN accumulator.
func (r *ComposedViewReader) attachLegs(ctx context.Context, rt *composedRuntime, s *composedSplit, items []map[string]any, includeArchived bool) error {
	if len(items) == 0 {
		return nil
	}
	for _, leg := range rt.legs {
		if !s.fetchLeg[leg.segKey] {
			continue
		}
		var err error
		switch {
		case leg.link.InChild():
			err = r.attachInChild(ctx, leg, s, items, includeArchived)
		case leg.link.Many:
			err = r.attachMany(ctx, leg, s, items, includeArchived)
		default:
			err = r.attachOne(ctx, leg, s, items, includeArchived)
		}
		if err != nil {
			return fmt.Errorf("composed view: leg %q: %w", leg.segKey, err)
		}
	}
	return nil
}

// legBaseFilter assembles the leg's Mongo filter shared by every fetch of one
// request: the translated segment filters plus the leg's own DeletedAt gate
// (a leg without DeletedAt has no gate — the includeArchived knob is a
// no-op there, never an error).
func (r *ComposedViewReader) legBaseFilter(leg *legRuntime, s *composedSplit, includeArchived bool) (bson.M, error) {
	filter := bson.M{}
	if lf := s.legFilters[leg.segKey]; len(lf) > 0 {
		colFilter, err := translateFilterKeys(leg.node, lf)
		if err != nil {
			return nil, err
		}
		applyFilter(filter, colFilter)
	}
	if sdCol, sdOn := leg.node.DeletedAtColumn(); sdOn && !includeArchived {
		filter[sdCol] = nil
	}
	return filter, nil
}

// legProjection translates the segment's sparse projection. A partial
// inclusion hides the leg doc's `_id` from the wire (the consumer asked for
// specific leaves) — but the 1:1 attach step GROUPS by `_id`, so the column
// stays in the Mongo projection and is stripped from the translated doc after
// grouping (stripID reports that). Whole-segment attaches keep the full doc,
// `_id` included, exactly like a direct read of the leg view.
func (r *ComposedViewReader) legProjection(leg *legRuntime, s *composedSplit) (proj bson.D, stripID bool, err error) {
	lp := s.legProj[leg.segKey]
	if len(lp) == 0 {
		return nil, false, nil
	}
	colProj, err := translateProjectionKeys(leg.node, lp)
	if err != nil {
		return nil, false, err
	}
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
	return buildProjection(colProj, nil), stripID, nil
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
		filter, err := r.legBaseFilter(leg, s, includeArchived)
		if err != nil {
			return err
		}
		filter["_id"] = bson.M{"$in": keys}
		findOpts := options.Find().SetLimit(int64(len(keys)))
		proj, stripLegID, err := r.legProjection(leg, s)
		if err != nil {
			return err
		}
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
				leg.node.StripJoinKeyID(goDoc)
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

// attachInChild resolves a LinkInChild (the read-time twin of a view's
// EmbedInChild): every element of the primary's native child array gains a 1:1
// sub-document looked up by the element's own ParentID. It is attachOne one level down —
// ONE find({_id:{$in:keys}}) across every element on the page (same segment
// filter, archived gate and projection), grouped by _id, stitched per element,
// with an explicit null when the ParentID is absent or the segment filter drops it.
func (r *ComposedViewReader) attachInChild(ctx context.Context, leg *legRuntime, s *composedSplit, items []map[string]any, includeArchived bool) error {
	childSeg := leg.link.ChildSegment
	fkField := leg.link.FKGoField
	field := leg.link.GoSegment

	var keys []any
	seen := map[string]bool{}
	for _, item := range items {
		for _, el := range childElems(item[childSeg]) {
			v, present := el[fkField]
			if !present || v == nil {
				continue
			}
			k := fmt.Sprintf("%v", v)
			if !seen[k] {
				seen[k] = true
				keys = append(keys, v)
			}
		}
	}

	byKey := map[string]map[string]any{}
	if len(keys) > 0 {
		filter, err := r.legBaseFilter(leg, s, includeArchived)
		if err != nil {
			return err
		}
		filter["_id"] = bson.M{"$in": keys}
		findOpts := options.Find().SetLimit(int64(len(keys)))
		proj, stripLegID, err := r.legProjection(leg, s)
		if err != nil {
			return err
		}
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
				leg.node.StripJoinKeyID(goDoc)
			}
			byKey[key] = goDoc
		}
	}

	for _, item := range items {
		for _, el := range childElems(item[childSeg]) {
			var match any // explicit null when absent or filtered out
			if v, present := el[fkField]; present && v != nil {
				if doc, found := byKey[fmt.Sprintf("%v", v)]; found {
					match = doc
				}
			}
			el[field] = match
		}
	}
	return nil
}

// attachMany resolves a 1:N leg in ONE aggregation round trip: a $match on
// the page's parent keys (indexed on the leg ParentID, carrying the segment
// filters and the leg's archived gate), an optional $project mirroring the
// segment's sparse projection, and a $group whose $topN accumulator applies
// the declared order (+ _id tiebreaker, the same stable-sort rule every
// reader query follows) and the resolved per-parent ceiling server-side —
// deterministic silent truncation: "the first N in the declared order",
// per parent, exactly as the retired one-find-per-parent walk produced.
// $topN needs MongoDB 5.2, which is already the framework's floor (the
// materialized-embed ordering rides $sortArray from the same release).
// Empty array when nothing matches a parent.
func (r *ComposedViewReader) attachMany(ctx context.Context, leg *legRuntime, s *composedSplit, items []map[string]any, includeArchived bool) error {
	base, err := r.legBaseFilter(leg, s, includeArchived)
	if err != nil {
		return err
	}
	proj, stripLegID, err := r.legProjection(leg, s)
	if err != nil {
		return err
	}

	var keys []any
	seen := map[string]bool{}
	for _, item := range items {
		v, present := item["_id"]
		if !present || v == nil {
			continue
		}
		k := fmt.Sprintf("%v", v)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, v)
		}
	}

	byParent := map[string][]any{}
	if len(keys) > 0 {
		match := bson.M{leg.link.ParentIDColumn: bson.M{"$in": keys}}
		for k, v := range base {
			match[k] = v
		}

		// The $group below reads the parent key off each document, so a sparse
		// segment projection must keep that column queryable — force it in and
		// strip it from the raw docs afterwards, the same transparent-helper
		// treatment the projection already gives `_id`.
		proj, stripParentCol := ensureParentColumn(proj, leg.link.ParentIDColumn)

		var legSort []queries.OrderByField
		if leg.link.OrderByColumn != "" {
			legSort = []queries.OrderByField{{Field: leg.link.OrderByColumn, Desc: leg.link.OrderByDesc}}
		}
		sortDoc := buildStableSortDoc(legSort, false)

		pipeline := bson.A{bson.M{"$match": match}}
		if len(proj) > 0 {
			pipeline = append(pipeline, bson.M{"$project": proj})
		}
		pipeline = append(pipeline, bson.M{"$group": bson.M{
			"_id": "$" + leg.link.ParentIDColumn,
			"docs": bson.M{"$topN": bson.M{
				"n":      leg.maxItems,
				"sortBy": sortDoc,
				"output": "$$ROOT",
			}},
		}})

		col := r.inner.mongo.collFn(r.inner.resolver.Active(leg.link.Collection).String())
		cur, err := col.Aggregate(ctx, pipeline)
		if err != nil {
			return err
		}
		var groups []struct {
			Key  any      `bson:"_id"`
			Docs []bson.M `bson:"docs"`
		}
		if err := cur.All(ctx, &groups); err != nil {
			cur.Close(ctx)
			return err
		}
		cur.Close(ctx)

		for _, g := range groups {
			out := make([]any, 0, len(g.Docs))
			for _, d := range g.Docs {
				m := map[string]any(d)
				if stripParentCol {
					delete(m, leg.link.ParentIDColumn)
				}
				goDoc := r.toGoLegDoc(leg, m, includeArchived)
				if stripLegID {
					leg.node.StripJoinKeyID(goDoc)
				}
				out = append(out, goDoc)
			}
			byParent[fmt.Sprintf("%v", g.Key)] = out
		}
	}

	for _, item := range items {
		segment := []any{}
		if v, present := item["_id"]; present && v != nil {
			if docs, found := byParent[fmt.Sprintf("%v", v)]; found {
				segment = docs
			}
		}
		item[leg.link.GoSegment] = segment
	}
	return nil
}

// ensureParentColumn guarantees a non-empty segment projection keeps the leg's
// parent-key column queryable for the $group stage. An inclusion projection
// gains {parentCol: 1} when the consumer did not ask for it; an exclusion
// projection drops an explicit {parentCol: 0}. Either adjustment reports
// strip=true so the attach step removes the column from the raw docs before
// translation, restoring the consumer's exact segment shape. An empty
// projection (whole doc) needs nothing.
func ensureParentColumn(proj bson.D, parentCol string) (bson.D, bool) {
	if len(proj) == 0 {
		return proj, false
	}
	inclusion := false
	for _, e := range proj {
		if v, ok := e.Value.(int); ok && v == 1 && e.Key != "_id" {
			inclusion = true
			break
		}
	}
	out := make(bson.D, 0, len(proj)+1)
	present := false
	explicitZero := false
	for _, e := range proj {
		if e.Key == parentCol {
			present = true
			if v, ok := e.Value.(int); ok && v == 0 {
				explicitZero = true
				continue // exclusion of the join key: lift it, strip after
			}
		}
		out = append(out, e)
	}
	switch {
	case inclusion && !present:
		out = append(out, bson.E{Key: parentCol, Value: 1})
		return out, true
	case explicitZero:
		return out, true
	default:
		return out, false
	}
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
	if page.EndCursor, err = restamp(page.EndCursor); err != nil {
		return err
	}
	if page.StartCursor, err = restamp(page.StartCursor); err != nil {
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
