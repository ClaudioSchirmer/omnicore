package query

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// FrameworkDefaultMaxLinkManyLimit is the per-parent ceiling a LinkMany segment
// applies when neither the link (MaxLinkManyLimit) nor the yaml
// (query.maxLinkManyLimit) declares one. It bounds the payload cost of one
// parent document fanning out into an unbounded 1:N segment.
const FrameworkDefaultMaxLinkManyLimit int64 = 100

// ComposedViewDefinition declares a READ-TIME composition over views that
// already exist — the fourth composition primitive and the only one that
// composes at read time. Unlike View / SharedBaseView / Embed it is NOT a view
// like the others: it is never materialized, never synced, never rebuilt;
// there is no Mongo collection, no Version(n), no schema-evolution entry, no
// recompose. A read against the composed name reads the PRIMARY view exactly
// as a direct read would, then enriches each returned item with data fetched
// by key, in batch, from the linked views ("legs"), joined in Go code.
//
// The core rule every knob derives from: pagination, sort, `search`, `total`
// and cursors belong exclusively to the primary view. Legs only enrich items
// already selected; they never add, remove or reorder primary rows. A filter
// addressed to a link segment filters what enters the segment, never which
// primary rows appear; the join is always LEFT (null sub-document / empty
// array when absent).
//
//	query.ComposedView("gadgets_full").
//	    Primary(GadgetView()).
//	    Link(query.JoinUpstream(GadgetUpstreamMirrorSchema(), "UpstreamMirror", "upstreamMirror")).
//	        On("id").
//	    LinkMany(query.JoinView(GadgetNotesView(), "Notes", "notes")).
//	        OrderBy("created_at").Desc().MaxLinkManyLimit(50).
//	        On("gadget_id")
//
// Join vocabulary: every relationship names one join column via .On(column); the
// ParentID always points at the other side's ID/_id; who holds the ParentID follows the
// multiplicity — 1:1 (Link) → the PRIMARY holds it; 1:N (LinkMany) → the LEG
// holds it. The leg carries only the two segment names (JoinView/JoinUpstream);
// the join column and the 1:N knobs live on the verb's binding.
//
// The primary is always an internal registered view. A leg is either an
// internal view (JoinView) or a locally materialized external collection
// (JoinUpstream over a core.NewExternalSchema whose collection an
// UpstreamSubscription declares). A leg addressing another service's live
// storage is forbidden — the request path never leaves the process.
//
// Composition is SINGLE-LEVEL — there is NO link-of-link. A leg is NEVER another
// ComposedView: Link/LinkMany take a *Leg that JoinView/JoinUpstream build only
// from a view or an external schema, so a composition-of-composition does not
// compile (depth is 1, enforced by the type system, exactly as embed-of-embed
// is). The only depth a leg carries is its own materialization (its children,
// siblings, roles and embeds), not a second join hop.
//
// Consumption is unchanged by design: the composed name goes wherever a view
// name goes (FindByParamsQueryHandler{Reader, View: "gadgets_full"}, auto or
// manual wiring, GraphQL, tabular export).
type ComposedViewDefinition struct {
	name    string
	primary *ViewDefinition
	links   []composedLinkDef
}

type composedLinkDef struct {
	docField  string // wire segment (the document field the leg lands under) = leg.externalName
	leg       *Leg
	many      bool
	joinCol   string // the ParentID column, named via .On(...)
	orderBy   string
	orderDesc bool
	// orderDescOnly marks ".Desc() without OrderBy" — a declaration mistake
	// surfaced at boot rather than silently reordering the _id default.
	orderDescOnly bool
	maxItems      int64
	// childSchema non-nil marks a LinkInChild: a 1:1 read-time enrichment landing
	// INSIDE each element of the primary's native child array (childSchema), keyed
	// by the element's own ParentID (joinCol) → the leg's _id. nil = a normal root
	// Link/LinkMany. A LinkInChild is always 1:1 (many == false).
	childSchema *core.TableSchema
}

// Leg is one side of a composed-view link OR a root-embed source: an internal
// view (JoinView) or a locally materialized external collection (JoinUpstream).
// It is a pure piece — what to pull plus the two segment names (goSegment for
// criteria/Response, externalName for the document field). The join column and
// the LinkMany-only knobs (OrderBy/Desc/MaxLinkManyLimit) are declared on the
// verb's binding, not here. An Embed accepts only a JoinUpstream (external) leg.
type Leg struct {
	view         *ViewDefinition   // internal leg (nil for external)
	schema       *core.TableSchema // external leg schema (nil for internal)
	goSegment    string
	externalName string
}

// IsMongo reports whether the leg resolves to a local external Mongo collection
// (a JoinUpstream leg). Used by the embed boot guards, which accept external legs
// only. A JoinView leg returns false.
func (l *Leg) IsMongo() bool { return l.schema != nil && l.schema.IsExternal() }

// Collection is the leg's local Mongo collection — the external schema's table or
// the internal leg view's name.
func (l *Leg) Collection() string { return legCollection(l) }

// Table is an alias of Collection, kept for the embed boot guards that inspect a
// leg as an embed source.
func (l *Leg) Table() string { return legCollection(l) }

// SchemaDef returns the external leg's core.TableSchema (nil for a JoinView leg).
func (l *Leg) SchemaDef() *core.TableSchema { return l.schema }

// ComposedView starts a composed-view declaration. The name is the read-side
// identity the consumer passes wherever a view name goes; it must not collide
// with any registered view or other composed view (boot-enforced).
func ComposedView(name string) *ComposedViewDefinition {
	return &ComposedViewDefinition{name: name}
}

// Primary declares the view that drives everything — filters that select rows,
// sort, search, pagination, total, cursors, MaxLimit ceiling. Exactly one, and
// it must be an internal registered view (regular or shared-base).
func (c *ComposedViewDefinition) Primary(v *ViewDefinition) *ComposedViewDefinition {
	if v == nil {
		panic(fmt.Sprintf("query.ComposedView(%q): .Primary(nil)", c.name))
	}
	if c.primary != nil {
		panic(fmt.Sprintf("query.ComposedView(%q): .Primary(...) declared twice — a composed view has exactly one primary", c.name))
	}
	c.primary = v
	return c
}

// Link declares a 1:1 leg: the PRIMARY holds the ParentID (named on the returned
// binding via .On(col), a primary column) pointing at the leg's _id. The matched
// document is attached under leg.externalName as a sub-document; an explicit null
// when absent (LEFT semantics). .On is mandatory — the binding it returns is the
// only route back to the ComposedViewDefinition, so a missing join key does not
// compile.
func (c *ComposedViewDefinition) Link(leg *Leg) *composedLink1Binding {
	if leg == nil {
		panic(fmt.Sprintf("query.ComposedView(%q): .Link(nil)", c.name))
	}
	return &composedLink1Binding{c: c, leg: leg}
}

// LinkMany declares a 1:N leg: the LEG holds the ParentID (named on the returned binding
// via .On(col), a leg column) pointing at the primary's _id. Matching documents
// are attached under leg.externalName as an array in the declared order (OrderBy,
// default _id ascending), capped per parent by the MaxLinkManyLimit cascade; an
// empty array when absent (LEFT semantics). The optional OrderBy/Desc/
// MaxLinkManyLimit precede the mandatory terminal .On(col).
func (c *ComposedViewDefinition) LinkMany(leg *Leg) *composedLinkManyBinding {
	if leg == nil {
		panic(fmt.Sprintf("query.ComposedView(%q): .LinkMany(nil)", c.name))
	}
	return &composedLinkManyBinding{c: c, leg: leg}
}

// LinkInChild declares a 1:1 read-time enrichment INSIDE a native child array of
// the PRIMARY — the non-materialized twin of a view's EmbedInChild. childSchema
// MUST be a native child of the primary's schema (boot-validated); for each
// primary row, every element of that child array gains a sub-document (under
// leg.externalName) looked up by the element's own ParentID — named on the returned
// binding via .On(col) — against the leg's _id. The leg is either an external
// JoinUpstream collection or an internal JoinView. 1:1 only (no
// LinkManyInChild — a 1:N inside a child element is the forbidden grandchild
// shape). Unlike EmbedInChild it is never materialized and needs no covering
// index (there is no recompose ripple). .On is the mandatory terminal.
func (c *ComposedViewDefinition) LinkInChild(childSchema *core.TableSchema, leg *Leg) *composedLinkInChildBinding {
	if leg == nil {
		panic(fmt.Sprintf("query.ComposedView(%q): .LinkInChild(nil leg)", c.name))
	}
	if childSchema == nil {
		panic(fmt.Sprintf("query.ComposedView(%q): .LinkInChild(nil childSchema)", c.name))
	}
	return &composedLinkInChildBinding{c: c, child: childSchema, leg: leg}
}

// composedLinkInChildBinding is what LinkInChild returns: On names the child
// element's ParentID column and completes the link.
type composedLinkInChildBinding struct {
	c     *ComposedViewDefinition
	child *core.TableSchema
	leg   *Leg
}

// On names the ParentID column each child element carries (→ the leg's _id) and
// completes the in-child link.
func (b *composedLinkInChildBinding) On(joinColumn string) *ComposedViewDefinition {
	b.c.links = append(b.c.links, composedLinkDef{
		docField:    b.leg.externalName,
		leg:         b.leg,
		many:        false,
		joinCol:     joinColumn,
		childSchema: b.child,
	})
	return b.c
}

// composedLink1Binding is what Link returns: On names the primary-side ParentID column
// and completes the link, returning the ComposedViewDefinition.
type composedLink1Binding struct {
	c   *ComposedViewDefinition
	leg *Leg
}

// On names the primary column holding the ParentID to the leg's _id and completes the
// 1:1 link.
func (b *composedLink1Binding) On(joinColumn string) *ComposedViewDefinition {
	b.c.links = append(b.c.links, composedLinkDef{docField: b.leg.externalName, leg: b.leg, many: false, joinCol: joinColumn})
	return b.c
}

// composedLinkManyBinding is what LinkMany returns: the optional 1:N knobs
// (OrderBy/Desc/MaxLinkManyLimit) chain on it, and the mandatory terminal On
// names the leg-side ParentID column and completes the link.
type composedLinkManyBinding struct {
	c         *ComposedViewDefinition
	leg       *Leg
	orderBy   string
	orderDesc bool
	maxItems  int64
}

// OrderBy declares the deterministic order of the LinkMany segment by a leg
// column (default: _id ascending). Every 1:N segment always has a guaranteed
// order — with or without truncation.
func (b *composedLinkManyBinding) OrderBy(column string) *composedLinkManyBinding {
	b.orderBy = column
	return b
}

// Desc inverts the OrderBy direction.
func (b *composedLinkManyBinding) Desc() *composedLinkManyBinding {
	b.orderDesc = true
	return b
}

// MaxLinkManyLimit declares the per-parent item ceiling of the LinkMany segment,
// mirroring the view-level MaxLimit pattern. Resolution at read time: this value
// (when > 0) wins; else the yaml default query.maxLinkManyLimit; else
// FrameworkDefaultMaxLinkManyLimit. When the ceiling is hit the segment is
// truncated deterministically in the declared order — silently, never an error:
// the ceiling is payload protection, not validation.
func (b *composedLinkManyBinding) MaxLinkManyLimit(n int64) *composedLinkManyBinding {
	b.maxItems = n
	return b
}

// On names the leg column holding the ParentID to the primary's _id and completes the
// 1:N link.
func (b *composedLinkManyBinding) On(joinColumn string) *ComposedViewDefinition {
	b.c.links = append(b.c.links, composedLinkDef{
		docField:      b.leg.externalName,
		leg:           b.leg,
		many:          true,
		joinCol:       joinColumn,
		orderBy:       b.orderBy,
		orderDesc:     b.orderDesc,
		orderDescOnly: b.orderDesc && b.orderBy == "",
		maxItems:      b.maxItems,
	})
	return b.c
}

// JoinView produces an internal leg over a registered view. The leg reads the
// view's own Mongo collection with the view's schema tree (children, embeds,
// roles translate exactly as a direct read of that view would). goName is the Go
// segment (criteria/Response), externalName the document field the leg lands
// under — both mandatory (declared like TableSchema.Field(go, external)).
func JoinView(v *ViewDefinition, goName, externalName string) *Leg {
	if v == nil {
		panic("query.JoinView(nil)")
	}
	if goName == "" || externalName == "" {
		panic(fmt.Sprintf("query.JoinView(%q): goName and externalName are both mandatory", v.Name()))
	}
	return &Leg{view: v, goSegment: goName, externalName: externalName}
}

// JoinUpstream produces an external leg over a locally materialized upstream
// collection — a core.NewExternalSchema describing the collection an
// UpstreamSubscription declares. It is also the ONLY source an Embed/EmbedMany/
// EmbedInChild accepts. Boot validates the subscription exists (a composed view
// never reads another service's live storage). goName / externalName are both
// mandatory (a type-less schema cannot derive them).
func JoinUpstream(ts *core.TableSchema, goName, externalName string) *Leg {
	if ts == nil {
		panic("query.JoinUpstream(nil)")
	}
	if !ts.IsExternal() {
		panic(fmt.Sprintf(
			"query.JoinUpstream(%q): the schema is write-anchored — an external leg takes a core.NewExternalSchema "+
				"describing a locally materialized upstream collection; an internal view joins via query.JoinView",
			ts.Table()))
	}
	if goName == "" || externalName == "" {
		panic(fmt.Sprintf("query.JoinUpstream(%q): goName and externalName are both mandatory", ts.Table()))
	}
	return &Leg{schema: ts, goSegment: goName, externalName: externalName}
}

// Name returns the composed view's read-side identity.
func (c *ComposedViewDefinition) Name() string { return c.name }

// ExternalLegs returns the EXTERNAL (JoinUpstream) legs this composition reads —
// the ones backed by a locally materialized mirror. Safe to call BEFORE
// ValidateComposedViews (unlike Links, it builds no translator node and resolves
// nothing): it exists for the boot guards that must cross-check a subscription's
// declaration against every consumer of its mirror, links included.
func (c *ComposedViewDefinition) ExternalLegs() []*Leg {
	var out []*Leg
	for _, ln := range c.links {
		if ln.leg != nil && ln.leg.schema != nil && ln.leg.schema.IsExternal() {
			out = append(out, ln.leg)
		}
	}
	return out
}

// PrimaryView returns the declared primary view (nil when not declared —
// rejected at boot by ValidateComposedViews).
func (c *ComposedViewDefinition) PrimaryView() *ViewDefinition { return c.primary }

// ExportPlan builds the tabular-export plan for the composed view: the
// primary's own plan with one child branch per leg, mirroring how Embed
// segments nest. An internal leg contributes its view's full export tree
// (children, embeds, roles included) re-rooted under the link's segment; an
// external leg contributes its flat external columns. Satisfies the same
// ExportView surface a ViewDefinition exposes, so QueryExport works on
// a composed name unchanged.
func (c *ComposedViewDefinition) ExportPlan() *queries.ExportPlan {
	plan := c.primary.ExportPlan()
	for _, ln := range c.links {
		seg := c.resolveLinkSegment(ln)
		var branch *queries.ExportNode
		if ln.leg.view != nil {
			branch = ln.leg.view.ExportPlan().Root
		} else {
			branch = buildExportNode(ln.leg.schema, nil, "", "")
		}
		branch.GoSegment = seg
		branch.WireSegment = ln.docField
		if ln.childSchema != nil {
			// LinkInChild nests INSIDE the primary child's export node (the primary
			// plan already produced it), mirroring EmbedInChild's tabular walk.
			childSeg := childDocSegment(ln.childSchema)
			for _, cn := range plan.Root.Children {
				if cn.GoSegment == childSeg {
					cn.Children = append(cn.Children, branch)
					break
				}
			}
			continue
		}
		plan.Root.Children = append(plan.Root.Children, branch)
	}
	return plan
}

// ResolveMaxExportRows delegates to the primary view — the export ceiling
// describes the cost of walking the primary's dataset (legs enrich the same
// rows). Satisfies the ExportView surface.
func (c *ComposedViewDefinition) ResolveMaxExportRows(yamlDefault int64) int64 {
	return c.primary.ResolveMaxExportRows(yamlDefault)
}

// schemaAndEmbeds returns the leg's schema tree: an internal leg exposes its
// view's root schema + declared embeds (the leg arrives exactly as a direct
// read of that view would); an external leg its external schema, no embeds.
func (l *Leg) schemaAndEmbeds() (*core.TableSchema, []embedDef) {
	if l.view != nil {
		return l.view.schema, l.view.embeds
	}
	return l.schema, nil
}

// resolveLinkSegment resolves a link's Go segment — the mandatory goName declared
// on the leg constructor (JoinView/JoinUpstream).
func (c *ComposedViewDefinition) resolveLinkSegment(ln composedLinkDef) string {
	return ln.leg.goSegment
}

// ComposedLink is the read-only, boot-validated projection of one link,
// consumed by the composed reader and the export planner. Built by Links()
// after validation succeeded — every field is resolved and consistent.
type ComposedLink struct {
	// GoSegment is the Go segment criteria/Response refer to (e.g. "Notes").
	GoSegment string
	// DocField is the wire segment the leg lands under (e.g. "notes").
	DocField string
	// Many distinguishes LinkMany (array segment) from Link (sub-document).
	Many bool
	// Collection is the leg's local Mongo collection.
	Collection string
	// External marks a JoinUpstream leg.
	External bool
	// ParentIDColumn is the declared join column — on the primary for a 1:1 link,
	// on the leg for a 1:N link.
	ParentIDColumn string
	// ParentKeyGoField is the Go-keyed field of the PRIMARY item carrying the
	// join value: "_id" on a 1:N link (leg.fk → primary._id) and on a 1:1 link
	// whose ParentID column is the primary's ID; otherwise the Go name of the
	// primary ParentID column.
	ParentKeyGoField string
	// OrderByColumn / OrderByDesc are the declared LinkMany order (column
	// resolved on the leg schema; empty column = _id ascending default).
	OrderByColumn string
	OrderByDesc   bool
	maxItems      int64
	node          *ViewNode
	// ChildSegment is non-empty for a LinkInChild: the Go segment (== doc segment)
	// of the primary's native child array the 1:1 enrichment lands inside. Empty
	// for a root Link/LinkMany.
	ChildSegment string
	// FKGoField is the Go field name each child element carries the join ParentID under
	// (the child schema's Go name for ParentIDColumn) — for a LinkInChild only.
	FKGoField string
}

// InChild reports whether this link is a LinkInChild (a 1:1 enrichment landing
// inside a primary child array) rather than a root Link/LinkMany.
func (l ComposedLink) InChild() bool { return l.ChildSegment != "" }

// ResolveMaxLinkManyLimit resolves the per-parent ceiling: the per-link value
// when declared, else the yaml default (query.maxLinkManyLimit) when positive,
// else FrameworkDefaultMaxLinkManyLimit.
func (l ComposedLink) ResolveMaxLinkManyLimit(yamlDefault int64) int64 {
	if l.maxItems > 0 {
		return l.maxItems
	}
	if yamlDefault > 0 {
		return yamlDefault
	}
	return FrameworkDefaultMaxLinkManyLimit
}

// Node returns the leg's translator tree (Go field paths ↔ physical columns,
// soft-delete gate, archived-children strip) — the leg view's full ViewNode
// for an internal leg, a flat schema node for an external one.
func (l ComposedLink) Node() *ViewNode { return l.node }

// Links materializes the validated read-only projection of every declared
// link, in declaration order. Call only after ValidateComposedViews accepted
// the definition — Links does not re-validate.
func (c *ComposedViewDefinition) Links() []ComposedLink {
	out := make([]ComposedLink, 0, len(c.links))
	for _, ln := range c.links {
		schema, _ := ln.leg.schemaAndEmbeds()
		var node *ViewNode
		if ln.leg.view != nil {
			node = ln.leg.view.BuildViewNode()
		} else {
			node = newViewNode(schema, nil)
		}
		parentKey := "_id"
		if !ln.many && ln.childSchema == nil && c.primary != nil && c.primary.schema != nil {
			if ln.joinCol != c.primary.schema.IDColumn() {
				if goName, ok := c.primary.schema.GoNameForRead(ln.joinCol); ok {
					parentKey = goName
				}
			}
		}
		childSeg, parentIDGoField := "", ""
		if ln.childSchema != nil {
			childSeg = childDocSegment(ln.childSchema)
			parentIDGoField = ln.joinCol
			if gn, ok := ln.childSchema.GoNameForRead(ln.joinCol); ok {
				parentIDGoField = gn
			}
		}
		out = append(out, ComposedLink{
			GoSegment:        c.resolveLinkSegment(ln),
			DocField:         ln.docField,
			Many:             ln.many,
			Collection:       legCollection(ln.leg),
			External:         ln.leg.view == nil,
			ParentIDColumn:   ln.joinCol,
			ParentKeyGoField: parentKey,
			OrderByColumn:    ln.orderBy,
			OrderByDesc:      ln.orderDesc,
			maxItems:         ln.maxItems,
			node:             node,
			ChildSegment:     childSeg,
			FKGoField:        parentIDGoField,
		})
	}
	return out
}

func legCollection(l *Leg) string {
	if l.view != nil {
		return l.view.Name()
	}
	return l.schema.Table()
}

// composedLegIndexCovers reports whether the leg view declares an index whose
// FIRST key is the given column — the same covering rule the §8.1 embed guard
// applies to join fields. The per-parent LinkMany subqueries filter on it, so
// a prefix match is what makes them index-driven.
func composedLegIndexCovers(v *ViewDefinition, col string) bool {
	for _, idx := range v.IndexSpecs() {
		keys := idx.KeyNames()
		if len(keys) > 0 && keys[0] == col {
			return true
		}
	}
	return false
}

// ValidateComposedViews enforces the composed-view boot contract (R11: any
// declaration exceeding the framework's limits is a fatal boot with an
// explanatory message, never a silent degradation). views are the registered
// ViewDefinitions; upstreamCollections the Mongo collections declared by the
// resolved UpstreamSubscriptions. Returns one error listing every offender so
// the operator sees them all in one boot attempt.
func ValidateComposedViews(composed []*ComposedViewDefinition, views []*ViewDefinition, upstreamCollections map[string]bool) error {
	viewNames := make(map[string]bool, len(views))
	for _, v := range views {
		viewNames[v.Name()] = true
	}
	seen := map[string]bool{}
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	for _, c := range composed {
		if c.name == "" {
			addf("composed view with empty name")
			continue
		}
		if viewNames[c.name] {
			addf("composed view %q: name collides with a registered view — the composed name is a read-side identity of its own", c.name)
		}
		if seen[c.name] {
			addf("composed view %q: declared more than once", c.name)
		}
		seen[c.name] = true
		if c.primary == nil {
			addf("composed view %q: no .Primary(...) declared", c.name)
			continue
		}
		if !viewNames[c.primary.Name()] {
			addf("composed view %q: primary view %q is not registered — the primary must be an internal view "+
				"contributed by a ReadableFeature (Views())", c.name, c.primary.Name())
		}
		if c.primary.SchemaDef() == nil {
			addf("composed view %q: primary view %q declares no .Schema(...) — a composed view has no schema of "+
				"its own; it derives its shape from the primary, so the primary must declare its root .Schema(...)",
				c.name, c.primary.Name())
		}
		if len(c.links) == 0 {
			addf("composed view %q: declares no .Link/.LinkMany — a composition without legs is the primary view itself; read it directly", c.name)
		}
		problems = validateComposedLinks(problems, c, viewNames, upstreamCollections)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(
		"composed view declaration(s) invalid — a ComposedView reads only registered internal views and locally "+
			"materialized upstream collections, every link declares its ParentID, and LinkMany-only knobs stay off 1:1 links:\n  - %s",
		strings.Join(problems, "\n  - "),
	)
}

func validateComposedLinks(problems []string, c *ComposedViewDefinition, viewNames, upstreamCollections map[string]bool) []string {
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	// Segment ownership at the composed root: every link segment plus every
	// segment the primary document already produces (embeds, derived children,
	// base-children, roles) and the primary's own flat Go fields — one
	// producer each, mirroring appendSegmentCollisions.
	owner := map[string]string{}
	if c.primary.schema != nil {
		for _, gf := range c.primary.schema.GoFields() {
			owner[gf] = "a primary root field"
		}
		primaryNode := c.primary.BuildViewNode()
		for seg := range primaryNode.embeds {
			owner[seg] = "a primary document segment (embed/child/role)"
		}
	}
	for _, ln := range c.links {
		kind := "Link"
		switch {
		case ln.childSchema != nil:
			kind = "LinkInChild"
		case ln.many:
			kind = "LinkMany"
		}
		seg := c.resolveLinkSegment(ln)
		if ln.childSchema != nil {
			// LinkInChild lands INSIDE each element of a primary child array, not at
			// the composed root — so it does NOT enter the root segment-ownership map.
			// It gets its own two checks: the child must be a native child of the
			// primary, and the leg's Go segment must not collide with a field the
			// child element already carries.
			nativeChild := false
			primaryTbl := ""
			if c.primary.schema != nil {
				primaryTbl = c.primary.schema.Table()
				for _, ch := range c.primary.schema.ChildSchemas() {
					if ch.Table() == ln.childSchema.Table() {
						nativeChild = true
						break
					}
				}
			}
			if !nativeChild {
				addf("composed view %q: LinkInChild %q — %q is NOT a native child of the primary schema %q; only a "+
					"child declared via root.Child(...) on the primary (for a shared-base primary, a child of the BASE) "+
					"can be enriched", c.name, ln.docField, ln.childSchema.Table(), primaryTbl)
			} else {
				for _, gf := range ln.childSchema.GoFields() {
					if gf == seg {
						addf("composed view %q: LinkInChild %q lands on Go field %q, which the child %q already carries — "+
							"rename the leg's Go segment", c.name, ln.docField, seg, ln.childSchema.Table())
						break
					}
				}
				for _, gc := range ln.childSchema.ChildSchemas() {
					if childDocSegment(gc) == seg {
						addf("composed view %q: LinkInChild %q lands on segment %q, already produced by a child of %q",
							c.name, ln.docField, seg, ln.childSchema.Table())
						break
					}
				}
			}
		} else if prev, dup := owner[seg]; dup {
			addf("composed view %q: %s %q would land on Go segment %q, already produced by %s — each segment has exactly one source",
				c.name, kind, ln.docField, seg, prev)
		} else {
			owner[seg] = fmt.Sprintf("%s %q", kind, ln.docField)
		}

		schema, _ := ln.leg.schemaAndEmbeds()
		if ln.leg.view != nil {
			if !viewNames[ln.leg.view.Name()] {
				addf("composed view %q: %s %q joins view %q, which is not registered — an internal leg must be "+
					"contributed by a ReadableFeature (Views())", c.name, kind, ln.docField, ln.leg.view.Name())
			}
		} else {
			if !upstreamCollections[schema.Table()] {
				addf("composed view %q: %s %q joins external collection %q, but no UpstreamSubscription materializes it — "+
					"a leg never reads another service's live storage; declare the subscription first (materialize, then compose)",
					c.name, kind, ln.docField, schema.Table())
			}
			if !schema.HasPKDeclared() {
				addf("composed view %q: %s %q external schema (collection %q) declares no primary key — declare .ID(column)",
					c.name, kind, ln.docField, schema.Table())
			}
		}

		if ln.joinCol == "" {
			holder := "the PRIMARY (primary.on → leg._id)"
			if ln.many {
				holder = "the LEG (leg.on → primary._id)"
			} else if ln.childSchema != nil {
				holder = "each CHILD ELEMENT (element.on → leg._id)"
			}
			addf("composed view %q: %s %q declares an empty join column — name it via .On(column); the ParentID column belongs to %s",
				c.name, kind, ln.docField, holder)
		} else if ln.childSchema != nil {
			if _, ok := ln.childSchema.GoNameForRead(ln.joinCol); !ok {
				addf("composed view %q: LinkInChild %q join column %q does not exist on the child schema (table %q) — "+
					"each element must carry the ParentID as a declared field", c.name, ln.docField, ln.joinCol, ln.childSchema.Table())
			}
		} else if ln.many {
			if schema != nil {
				if _, ok := schema.GoNameForRead(ln.joinCol); !ok {
					addf("composed view %q: LinkMany %q join column %q does not exist on the leg schema (collection %q)",
						c.name, ln.docField, ln.joinCol, schema.Table())
				}
			}
			// A LinkMany runs ONE find({fk: parent}) subquery PER PAGE PARENT;
			// without an index whose first key is the ParentID, each subquery is a
			// full collection scan — O(parents × leg docs) per request, the
			// classic silent monster. An internal leg declares its indexes on
			// the ViewDefinition, so this is verifiable at boot; reject it.
			// (An external leg has no index declaration to inspect — the
			// operator owns the upstream collection's indexes; the manual
			// carries the same rule as guidance.)
			if ln.leg.view != nil && !composedLegIndexCovers(ln.leg.view, ln.joinCol) {
				addf("composed view %q: LinkMany %q joins view %q on join column %q with NO covering index — "+
					"every page parent runs one find({%s: parent}) subquery, and without an index each one "+
					"is a full collection scan. Declare query.Index(%q) (or a compound index starting with it) "+
					"on the leg view",
					c.name, ln.docField, ln.leg.view.Name(), ln.joinCol, ln.joinCol, ln.joinCol)
			}
		} else if c.primary.schema != nil {
			if ln.joinCol != c.primary.schema.IDColumn() {
				if _, ok := c.primary.schema.GoNameForRead(ln.joinCol); !ok {
					addf("composed view %q: Link %q join column %q does not exist on the primary schema (table %q)",
						c.name, ln.docField, ln.joinCol, c.primary.schema.Table())
				}
			}
		}

		// OrderBy/Desc/MaxLinkManyLimit are 1:N-only knobs and live on the LinkMany
		// binding by construction — they cannot be declared on a 1:1 Link, so there
		// is no misplacement to reject. Only their value validity is checked here.
		if ln.many {
			if ln.orderBy != "" && schema != nil {
				if _, ok := schema.GoNameForRead(ln.orderBy); !ok {
					addf("composed view %q: LinkMany %q OrderBy column %q does not exist on the leg schema (collection %q)",
						c.name, ln.docField, ln.orderBy, schema.Table())
				}
			}
			if ln.orderDescOnly {
				addf("composed view %q: LinkMany %q declares .Desc() without .OrderBy(column) — name the order column",
					c.name, ln.docField)
			}
			if ln.maxItems < 0 {
				addf("composed view %q: LinkMany %q declares a negative MaxLinkManyLimit", c.name, ln.docField)
			}
		}
	}
	return problems
}
