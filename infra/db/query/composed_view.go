package query

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
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
//	    Link("upstreamMirror", query.JoinUpstream(GadgetUpstreamMirrorSchema()).
//	        FK("id").
//	        As("UpstreamMirror")).
//	    LinkMany("notes", query.JoinView(GadgetNotesView()).
//	        FK("gadget_id").
//	        OrderBy("created_at").Desc().
//	        MaxLinkManyLimit(50))
//
// Join vocabulary: every relationship declares one FK(column); the FK always
// points at the other side's PK/_id; who holds the FK follows the multiplicity
// — 1:1 (Link) → the PRIMARY holds it; 1:N (LinkMany) → the LEG holds it.
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
	docField string // wire segment (the document field the leg lands under)
	leg      *Leg
	many     bool
}

// Leg is one side of a composed-view link: an internal view (JoinView) or a
// locally materialized external collection (JoinUpstream). Configure it with
// FK (mandatory), As (mandatory for external legs), and — on a LinkMany only —
// OrderBy/Desc and MaxLinkManyLimit.
type Leg struct {
	view      *ViewDefinition   // internal leg (nil for external)
	schema    *core.TableSchema // external leg schema (nil for internal)
	fk        string
	goSegment string
	orderBy   string
	orderDesc bool
	// orderDescOnly marks ".Desc() without OrderBy" — a declaration mistake
	// surfaced at boot rather than silently reordering the _id default.
	orderDescOnly bool
	maxItems      int64
}

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

// Link declares a 1:1 leg: the PRIMARY holds the FK (declared via leg.FK,
// naming a primary column) pointing at the leg's _id. The matched document is
// attached under docField as a sub-document; an explicit null when absent
// (LEFT semantics).
func (c *ComposedViewDefinition) Link(docField string, leg *Leg) *ComposedViewDefinition {
	return c.link(docField, leg, false)
}

// LinkMany declares a 1:N leg: the LEG holds the FK (declared via leg.FK,
// naming a leg column) pointing at the primary's _id. Matching documents are
// attached under docField as an array in the leg's declared order (OrderBy,
// default _id ascending), capped per parent by the MaxLinkManyLimit cascade;
// an empty array when absent (LEFT semantics).
func (c *ComposedViewDefinition) LinkMany(docField string, leg *Leg) *ComposedViewDefinition {
	return c.link(docField, leg, true)
}

func (c *ComposedViewDefinition) link(docField string, leg *Leg, many bool) *ComposedViewDefinition {
	if leg == nil {
		panic(fmt.Sprintf("query.ComposedView(%q): link %q declares a nil leg", c.name, docField))
	}
	if docField == "" {
		panic(fmt.Sprintf("query.ComposedView(%q): a link declares an empty document field", c.name))
	}
	c.links = append(c.links, composedLinkDef{docField: docField, leg: leg, many: many})
	return c
}

// JoinView produces an internal leg over a registered view. The leg reads the
// view's own Mongo collection with the view's schema tree (children, embeds,
// roles translate exactly as a direct read of that view would).
func JoinView(v *ViewDefinition) *Leg {
	if v == nil {
		panic("query.JoinView(nil)")
	}
	return &Leg{view: v}
}

// JoinUpstream produces an external leg over a locally materialized upstream
// collection — a core.NewExternalSchema describing the collection an
// UpstreamSubscription declares. Boot validates the subscription exists (a
// composed view never reads another service's live storage). .As is mandatory
// (a type-less schema cannot derive its Go segment).
func JoinUpstream(ts *core.TableSchema) *Leg {
	if ts == nil {
		panic("query.JoinUpstream(nil)")
	}
	if !ts.IsExternal() {
		panic(fmt.Sprintf(
			"query.JoinUpstream(%q): the schema is write-anchored — an external leg takes a core.NewExternalSchema "+
				"describing a locally materialized upstream collection; an internal view joins via query.JoinView",
			ts.Table()))
	}
	return &Leg{schema: ts}
}

// FK declares the join's foreign-key column. The FK always points at the other
// side's PK/_id; who holds it follows the multiplicity: on a 1:1 Link the
// column belongs to the PRIMARY (primary.fk → leg._id), on a 1:N LinkMany it
// belongs to the LEG (leg.fk → primary._id).
func (l *Leg) FK(column string) *Leg {
	l.fk = column
	return l
}

// As declares the Go segment name for this leg — what criteria and Response
// refer to (e.g. "UpstreamMirror"). Mandatory for an external leg (no Go type
// to derive it from); an optional override for an internal one (default: the
// leg view root type name, pluralized on a LinkMany).
func (l *Leg) As(goSegment string) *Leg {
	l.goSegment = goSegment
	return l
}

// OrderBy declares the deterministic order of a LinkMany segment by a leg
// column (default: _id ascending). Every 1:N segment always has a guaranteed
// order — with or without truncation. LinkMany only: declaring it on a 1:1
// Link is a fatal boot.
func (l *Leg) OrderBy(column string) *Leg {
	l.orderBy = column
	return l
}

// Desc inverts the OrderBy direction. LinkMany only.
func (l *Leg) Desc() *Leg {
	l.orderDesc = true
	if l.orderBy == "" {
		l.orderDescOnly = true
	}
	return l
}

// MaxLinkManyLimit declares the per-parent item ceiling of a LinkMany segment
// — named after the LinkMany builder it constrains, mirroring the view-level
// MaxLimit pattern. Resolution at read time: this value (when > 0) wins; else
// the yaml default query.maxLinkManyLimit; else
// FrameworkDefaultMaxLinkManyLimit. When the ceiling is hit the segment is
// truncated deterministically in the declared order — silently, never an
// error: the ceiling is payload protection, not validation. Operational state,
// like MaxLimit. LinkMany only: declaring it on a 1:1 Link is a fatal boot.
func (l *Leg) MaxLinkManyLimit(n int64) *Leg {
	l.maxItems = n
	return l
}

// Name returns the composed view's read-side identity.
func (c *ComposedViewDefinition) Name() string { return c.name }

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

// resolveLinkSegment resolves a link's Go segment: explicit .As wins; an
// internal leg derives it from the leg view root's Go type (pluralized on a
// LinkMany); an external leg without .As returns "" (rejected at boot).
func (c *ComposedViewDefinition) resolveLinkSegment(ln composedLinkDef) string {
	if ln.leg.goSegment != "" {
		return ln.leg.goSegment
	}
	if ln.leg.view != nil && ln.leg.view.schema != nil {
		name := ln.leg.view.schema.TypeName()
		if name == "" {
			return ""
		}
		if ln.many {
			return domain.PluralizeWord(name)
		}
		return name
	}
	return ""
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
	// FKColumn is the declared join column — on the primary for a 1:1 link,
	// on the leg for a 1:N link.
	FKColumn string
	// ParentKeyGoField is the Go-keyed field of the PRIMARY item carrying the
	// join value: "_id" on a 1:N link (leg.fk → primary._id) and on a 1:1 link
	// whose FK column is the primary's PK; otherwise the Go name of the
	// primary FK column.
	ParentKeyGoField string
	// OrderByColumn / OrderByDesc are the declared LinkMany order (column
	// resolved on the leg schema; empty column = _id ascending default).
	OrderByColumn string
	OrderByDesc   bool
	maxItems      int64
	node          *ViewNode
}

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
		if !ln.many && c.primary != nil && c.primary.schema != nil {
			if ln.leg.fk != c.primary.schema.PKColumn() {
				if goName, ok := c.primary.schema.GoNameForRead(ln.leg.fk); ok {
					parentKey = goName
				}
			}
		}
		out = append(out, ComposedLink{
			GoSegment:        c.resolveLinkSegment(ln),
			DocField:         ln.docField,
			Many:             ln.many,
			Collection:       legCollection(ln.leg),
			External:         ln.leg.view == nil,
			FKColumn:         ln.leg.fk,
			ParentKeyGoField: parentKey,
			OrderByColumn:    ln.leg.orderBy,
			OrderByDesc:      ln.leg.orderDesc,
			maxItems:         ln.leg.maxItems,
			node:             node,
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
			"materialized upstream collections, every link declares its FK, and LinkMany-only knobs stay off 1:1 links:\n  - %s",
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
		if ln.many {
			kind = "LinkMany"
		}
		seg := c.resolveLinkSegment(ln)
		if seg == "" {
			addf("composed view %q: external %s %q has no Go segment — declare it via .As(\"...\") "+
				"(a type-less schema cannot derive it from a Go type)", c.name, kind, ln.docField)
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
				addf("composed view %q: %s %q external schema (collection %q) declares no primary key — declare .PK(column)",
					c.name, kind, ln.docField, schema.Table())
			}
		}

		if ln.leg.fk == "" {
			holder := "the PRIMARY (primary.fk → leg._id)"
			if ln.many {
				holder = "the LEG (leg.fk → primary._id)"
			}
			addf("composed view %q: %s %q declares no .FK(column) — the FK column belongs to %s",
				c.name, kind, ln.docField, holder)
		} else if ln.many {
			if schema != nil {
				if _, ok := schema.GoNameForRead(ln.leg.fk); !ok {
					addf("composed view %q: LinkMany %q FK column %q does not exist on the leg schema (collection %q)",
						c.name, ln.docField, ln.leg.fk, schema.Table())
				}
			}
			// A LinkMany runs ONE find({fk: parent}) subquery PER PAGE PARENT;
			// without an index whose first key is the FK, each subquery is a
			// full collection scan — O(parents × leg docs) per request, the
			// classic silent monster. An internal leg declares its indexes on
			// the ViewDefinition, so this is verifiable at boot; reject it.
			// (An external leg has no index declaration to inspect — the
			// operator owns the upstream collection's indexes; the manual
			// carries the same rule as guidance.)
			if ln.leg.view != nil && !composedLegIndexCovers(ln.leg.view, ln.leg.fk) {
				addf("composed view %q: LinkMany %q joins view %q on FK column %q with NO covering index — "+
					"every page parent runs one find({%s: parent}) subquery, and without an index each one "+
					"is a full collection scan. Declare query.Index(%q) (or a compound index starting with it) "+
					"on the leg view",
					c.name, ln.docField, ln.leg.view.Name(), ln.leg.fk, ln.leg.fk, ln.leg.fk)
			}
		} else if c.primary.schema != nil {
			if ln.leg.fk != c.primary.schema.PKColumn() {
				if _, ok := c.primary.schema.GoNameForRead(ln.leg.fk); !ok {
					addf("composed view %q: Link %q FK column %q does not exist on the primary schema (table %q)",
						c.name, ln.docField, ln.leg.fk, c.primary.schema.Table())
				}
			}
		}

		if !ln.many {
			if ln.leg.orderBy != "" || ln.leg.orderDesc {
				addf("composed view %q: Link %q declares OrderBy/Desc — segment order applies to LinkMany only "+
					"(a 1:1 sub-document has no order)", c.name, ln.docField)
			}
			if ln.leg.maxItems != 0 {
				addf("composed view %q: Link %q declares MaxLinkManyLimit — the per-parent ceiling applies to LinkMany only "+
					"(a 1:1 sub-document cannot fan out)", c.name, ln.docField)
			}
		} else {
			if ln.leg.orderBy != "" && schema != nil {
				if _, ok := schema.GoNameForRead(ln.leg.orderBy); !ok {
					addf("composed view %q: LinkMany %q OrderBy column %q does not exist on the leg schema (collection %q)",
						c.name, ln.docField, ln.leg.orderBy, schema.Table())
				}
			}
			if ln.leg.orderDescOnly {
				addf("composed view %q: LinkMany %q declares .Desc() without .OrderBy(column) — name the order column",
					c.name, ln.docField)
			}
			if ln.leg.maxItems < 0 {
				addf("composed view %q: LinkMany %q declares a negative MaxLinkManyLimit", c.name, ln.docField)
			}
		}
	}
	return problems
}
