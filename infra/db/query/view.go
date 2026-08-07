package query

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

type ViewDefinition struct {
	name            string
	version         int
	schema          *core.TableSchema
	embeds          []embedDef
	childEmbeds     []childEmbedDef
	deleteOnArchive bool
	mongoSpec       mongoSpec
	// maxLimit caps the per-page document count returned by ViewReader.ReadPage.
	// Zero (unset) defers to the global yaml-declared default (cfg.Query.MaxLimit),
	// which in turn falls back to the framework default 100 when the yaml is also
	// silent. Deliberately NOT included in RebuildHash / ArtifactHash — the cap
	// is operational and may be tuned without triggering a Mongo rebuild or a
	// version bump.
	maxLimit int64
	// maxExportRows caps the row count a tabular export (CSV/XLSX) of this view
	// streams. Zero (unset) defers to the yaml default (cfg.Query.MaxExportRows),
	// then to DefaultMaxExportRows. Like maxLimit it is operational state — NOT
	// part of RebuildHash / ArtifactHash.
	maxExportRows int64
	// isSharedBaseView marks a view rooted at a shared base (SharedBaseView):
	// schema is the base's core.NewSharedBaseSchema declaration (whose table is the
	// root table) and roles carries one segment per declared specialization.
	// False for a regular query.View.
	isSharedBaseView bool
	roles            []roleDef
	// relationalReader, when non-nil, marks the view as read directly from the
	// relational backend (SoR) instead of the materialized Mongo projection: it is
	// the aggregate loader bound to the view's root entity, handed in via
	// RelationalSource(...). Nil for a Mongo-backed view.
	relationalReader RelationalReader
}

type embedDef struct {
	leg     *Leg
	many    bool
	joinCol string
	// orderBy / orderDesc are the declared order of a 1:N segment's elements
	// (EmbedMany only). Empty column = unordered: the array carries whatever
	// order the writes produced, which is the historical behavior and stays the
	// default. orderDescOnly marks ".Desc() without .OrderBy(...)" — a
	// declaration mistake surfaced at boot rather than silently ignored.
	orderBy       string
	orderDesc     bool
	orderDescOnly bool
}

// OrderColumn / OrderDesc expose the declared 1:N segment order to the boot
// guards and the hash. Empty column = unordered.
func (e embedDef) OrderColumn() string { return e.orderBy }
func (e embedDef) OrderDesc() bool     { return e.orderDesc }

// Source exposes the read-only Leg associated with this embed. Used by
// bootstrap-side boot guards that walk Embeds() and must inspect IsMongo /
// Collection / SchemaDef without reaching into private fields across package
// boundaries.
func (e embedDef) Source() *Leg { return e.leg }

// childEmbedDef is one EmbedInChild declaration: a 1:1 enrichment applied to
// every element of a NATIVE aggregate-child array of the view (declared via
// root.Child(childSchema)). Unlike embedDef (which joins off the root doc), the
// join key lives INSIDE each child element: for each element the composer reads
// element[joinCol] and looks the leg doc up by _id, landing it under
// leg.externalName. There is deliberately no EmbedMany-in-child: a 1:N enrichment
// would nest an array inside a child-array element (a third collection level, the
// forbidden "grandchild" shape), so only the 1:1 form exists.
type childEmbedDef struct {
	// childSchema identifies WHICH native child of the view root this enriches.
	// Validated at boot against root.ChildSchemas() by table identity (schema
	// constructors return fresh instances, so the match is by table, not pointer).
	childSchema *core.TableSchema
	// leg is the external enrichment source (a Mongo collection).
	leg *Leg
	// joinCol is the ParentID column that lives INSIDE each child element (e.g.
	// "product_id"), resolved against the leg's _id. Declared via .On(...).
	joinCol string
}

// ChildSegment is the doc field where the enriched child array lives — derived
// from the child schema exactly like the composer names it, so the ripple's
// nested-field fan-out and the required multikey index agree on the path.
func (c childEmbedDef) ChildSegment() string { return childDocSegment(c.childSchema) }

// ParentIDColumn is the join column inside each child element (declared via .On).
func (c childEmbedDef) ParentIDColumn() string { return c.joinCol }

// Field returns the doc field the enrichment lands under inside each element.
func (c childEmbedDef) Field() string { return c.leg.externalName }

// Source returns the external enrichment leg.
func (c childEmbedDef) Source() *Leg { return c.leg }

// ChildSchema returns the native child schema this enrichment targets.
func (c childEmbedDef) ChildSchema() *core.TableSchema { return c.childSchema }

// Field returns the document field name where the embed lands in the
// composed Mongo document. Symmetric with Source() for boot guards.
func (e embedDef) Field() string { return e.leg.externalName }

// Many reports whether the embed is EmbedMany (one-to-many) vs Embed
// (one-to-one).
func (e embedDef) Many() bool { return e.many }

func View(name string) *ViewDefinition {
	return &ViewDefinition{name: name}
}

// Schema attaches the view's root core.TableSchema (Go↔column + ID + DeletedAt) —
// the same schema the repository declares. The composer uses it for the root
// ID + DeletedAt column; the reader uses it to translate root leaf fields
// between Go field names and physical columns. Reuse the repo's schema so write
// and read agree.
func (v *ViewDefinition) Schema(ts *core.TableSchema) *ViewDefinition {
	v.schema = ts
	return v
}

// SchemaDef returns the view's root core.TableSchema (nil when unset).
func (v *ViewDefinition) SchemaDef() *core.TableSchema { return v.schema }

// Version declares the shape version of the view. Mandatory — the framework
// rejects views with version <= 0 at boot via ValidateMongoSpec.
//
// Bump the integer every time the view's declarative shape changes in a way
// that requires recomposing every Mongo document (root table, embeds,
// DeleteOnArchive flag, $jsonSchema validator, collation, capped, time-series).
// Index-only changes do NOT require a version bump — they flow through
// ApplyMongoSpecs without document recomposition.
//
// The version participates in RebuildHash, so changing the spec without
// bumping the version produces a hash mismatch the framework detects as
// DriftForgotToBump and aborts boot (no escape via autoRun). See
// tasks/mongo_schema_evolution_2.md §8.
func (v *ViewDefinition) Version(n int) *ViewDefinition {
	v.version = n
	return v
}

// DeleteOnArchive opts the view in to dropping archived rows from the Mongo
// projection. By default (flag absent), an ARCHIVED outbox event triggers a
// compose+upsert so the document survives with deleted_at populated — the
// read side mirrors the relational backend symmetrically, and the composer
// omits the WHERE deleted_at IS NULL filter on the root SELECT and on every
// archiving relational source of the aggregate's closure (child
// collections, the role-remnant pick). When this builder is called, ARCHIVED
// events instead remove the document from the Mongo collection and the
// composer applies the WHERE deleted_at IS NULL filter across that closure
// (cascade: the flag governs the aggregate's own projection — there is no
// per-child override). Embed segments are untouched by the flag: an embed is
// a Mongo read of its source and always mirrors the source's own archive
// state (the read-time archived strip is governed by the SOURCE schema's
// DeletedAt declaration, not by this flag).
//
// Reader semantics are unchanged: by-id and list queries default to
// IncludeArchived=false (filter applied at the Mongo layer); the consumer
// must opt in via the existing IncludeArchived path (e.g. the
// ?includeArchived=true query parameter) to see archived documents. With the
// default (keep-archived), `?includeArchived=true` reaches the document and
// returns it; with DeleteOnArchive(), the document is absent and the
// reader returns 404 — the explicit hot-tier choice consumers make when
// declaring this option.
//
// Hard DELETE always removes the document from Mongo regardless of this flag —
// delete-on-archive covers archives only.
func (v *ViewDefinition) DeleteOnArchive() *ViewDefinition {
	v.deleteOnArchive = true
	return v
}

// RelationalSource marks the view as read directly from the relational backend
// (the SoR) instead of the materialized Mongo projection, and binds it to the
// aggregate loader that reads its root entity — normally the declaring feature's
// repository loader (repo.Loader), threaded down through the view constructor.
// Handing the loader here is what lets the relational ViewReader load the
// aggregate by the view's name without the framework naming the entity's Go
// type: the loader rides on the ViewDefinition the bootstrap already collects,
// so the relational read reuses the exact typed loader the write side built.
//
// A marked view is served fresh — strong read-your-writes, no projection lag —
// at the cost of the query limitations relational views carry (no free-text
// search, no filter or sort on a child field). Removing the marker returns the
// view to the Mongo projection with no other change. reader must be non-nil.
func (v *ViewDefinition) RelationalSource(reader RelationalReader) *ViewDefinition {
	v.relationalReader = reader
	return v
}

// IsRelational reports whether the view was marked with RelationalSource() — read
// from the SoR instead of the Mongo projection.
func (v *ViewDefinition) IsRelational() bool { return v.relationalReader != nil }

// RelationalReader returns the aggregate loader a RelationalSource() view carries,
// or nil for a Mongo-backed view. The relational ViewReader consults it to load
// the aggregate for the view.
func (v *ViewDefinition) RelationalReader() RelationalReader { return v.relationalReader }

// Embed declares a 1:1 materialized enrichment: the leg's document (matched by
// the PARENT's ParentID column, named on the returned binding via .On(col)) lands
// under leg.externalName as a sub-document. The leg is either a JoinUpstream leg
// (a locally materialized upstream collection) or a JoinView leg (a local
// registered view, materialized into this one and kept fresh by the recompose
// ripple every write to that view signals; the view→view embed graph must be
// acyclic, boot-enforced). .On(col) is mandatory — the binding it returns is the
// only route back to the ViewDefinition, so a missing join key does not compile.
func (v *ViewDefinition) Embed(leg *Leg) *embedBinding {
	return &embedBinding{v: v, leg: leg, many: false}
}

// EmbedMany declares a 1:N materialized enrichment: every leg document whose ParentID
// column (named on the returned binding via .On(col)) points at this view's _id
// lands under leg.externalName as an array. Same leg kinds (JoinUpstream or
// JoinView) and mandatory .On as Embed.
func (v *ViewDefinition) EmbedMany(leg *Leg) *embedManyBinding {
	return &embedManyBinding{v: v, leg: leg}
}

// embedManyBinding is what EmbedMany returns: the 1:N-only knob (OrderBy/Desc)
// chains on it, and the mandatory terminal On names the leg-side ParentID column. A
// dedicated type — not a flag on the shared binding — so declaring an order on
// a 1:1 Embed or on an EmbedInChild is NOT EXPRESSIBLE rather than rejected at
// boot, the same way the ComposedView's LinkMany binding works.
type embedManyBinding struct {
	v         *ViewDefinition
	leg       *Leg
	orderBy   string
	orderDesc bool
}

// OrderBy declares the deterministic order of the materialized 1:N segment by a
// column of the SOURCE (leg) documents. Without it the array keeps whatever
// order the writes produced — the historical behavior, unchanged for every view
// that does not declare one.
//
// The order is materialized, not applied at read time: every writer of the
// segment (first compose, rebuild backfill, and the surgical ripple) sorts
// through the SAME MongoDB pipeline operator, so the stored array is already in
// order and all writers converge byte for byte. The sort is TOTAL — the declared
// column, then `_id` — because neither the driver nor the server promises a
// stable sort on ties, and two writers producing different arrays for identical
// state would surface as a blue-green verify divergence.
//
// Requires MongoDB 5.2+ ($sortArray). There is deliberately no per-parent
// ceiling to go with it: truncating a MATERIALIZED array would discard elements
// no later edit could promote back (the surgical edit never sees what was cut),
// so a cap belongs to the read-time twin — ComposedView's LinkMany.
func (b *embedManyBinding) OrderBy(column string) *embedManyBinding {
	b.orderBy = column
	return b
}

// Desc inverts the OrderBy direction.
func (b *embedManyBinding) Desc() *embedManyBinding {
	b.orderDesc = true
	return b
}

// On names the leg column holding the ParentID to this view's _id and completes the
// 1:N embed.
func (b *embedManyBinding) On(joinColumn string) *ViewDefinition {
	b.v.embeds = append(b.v.embeds, embedDef{
		leg:           b.leg,
		many:          true,
		joinCol:       joinColumn,
		orderBy:       b.orderBy,
		orderDesc:     b.orderDesc,
		orderDescOnly: b.orderDesc && b.orderBy == "",
	})
	return b.v
}

// embedBinding is the intermediate a root Embed/EmbedMany/EmbedInChild returns.
// Its only method, On, names the join column and returns the ViewDefinition, so
// the join is a compile-time requirement (the chain cannot continue without it).
type embedBinding struct {
	v     *ViewDefinition
	leg   *Leg
	many  bool
	child *core.TableSchema // non-nil for EmbedInChild
}

// On names the join column and completes the embed. For Embed/EmbedInChild (1:1)
// it is the PARENT-side column holding the ParentID to the leg's _id (for EmbedInChild,
// the ParentID column inside each child element); for EmbedMany (1:N) it is the LEG
// column pointing back at this view's _id. The ParentID always points at the other
// side's ID — who holds it follows the verb's multiplicity.
func (b *embedBinding) On(joinColumn string) *ViewDefinition {
	if b.child != nil {
		b.v.childEmbeds = append(b.v.childEmbeds, childEmbedDef{childSchema: b.child, leg: b.leg, joinCol: joinColumn})
		return b.v
	}
	b.v.embeds = append(b.v.embeds, embedDef{leg: b.leg, many: b.many, joinCol: joinColumn})
	return b.v
}

// EmbedInChild enriches every element of a NATIVE aggregate-child array of this
// view with a 1:1 external lookup — the read-side denormalization for the
// "list of X with the name of Y per line" shape (e.g. a sale view whose line
// items each carry product_id, enriched with the product name from an upstream
// projection). The write model stays normalized (the element keeps only its ParentID);
// the enrichment lives only in the view, kept fresh by the recompose ripple.
//
//		.EmbedInChild(SaleItemSchema(),
//		    query.JoinUpstream(query.NewExternalSchema("upstream_products"), "Product", "product")).
//		    On("product_id")
//
//	  - childSchema MUST be a native child declared on the view root via
//	    .Child(...) (validated at boot against root.ChildSchemas() by table
//	    identity). For a SharedBaseView it targets the BASE's native children;
//	    role-nested children are not supported.
//	  - leg is a JoinUpstream leg (an external NewExternalSchema) carrying the Go
//	    and doc segment names; .On(col) names the join column that lives INSIDE
//	    each child element, resolved against the leg's _id.
//	  - 1:1 only — there is no EmbedManyInChild (a 1:N would nest an array inside a
//	    child element, the forbidden grandchild shape).
//
// The ripple's fan-out reverse-scans the view on "<childSegment>.<fk>", so a
// covering multikey index on that path is REQUIRED and enforced at boot.
func (v *ViewDefinition) EmbedInChild(childSchema *core.TableSchema, leg *Leg) *embedBinding {
	return &embedBinding{v: v, leg: leg, child: childSchema}
}

// MaxLimit overrides the per-view page-size ceiling (`?first=`/`?last=`) for endpoints reading
// from this projection. Applies uniformly to every endpoint that consults the
// view, regardless of how many handlers point at it — the cap describes the
// cost of reading this specific dataset, not the cost of any single endpoint.
//
// Resolution at read time: this value (when > 0) wins; otherwise the yaml
// default `query.maxLimit` wins; otherwise the framework default 100. A
// `?first=N`/`?last=N` greater than the resolved ceiling is rejected by the
// reader with the 400 LimitExceededNotification.
//
// NOT part of RebuildHash / ArtifactHash: the cap is operational state, not
// projection shape. Bumping it neither triggers a Mongo rebuild nor requires
// a Version(N) bump.
func (v *ViewDefinition) MaxLimit(n int64) *ViewDefinition {
	v.maxLimit = n
	return v
}

func (v *ViewDefinition) Name() string                 { return v.name }
func (v *ViewDefinition) VersionNumber() int           { return v.version }
func (v *ViewDefinition) Embeds() []embedDef           { return v.embeds }
func (v *ViewDefinition) ChildEmbeds() []childEmbedDef { return v.childEmbeds }
func (v *ViewDefinition) DeletesOnArchive() bool       { return v.deleteOnArchive }

// RootTable is the physical aggregate-root table the view is fed from (the
// broker routing key) and the composer reads FROM. It is DERIVED from the
// attached root schema — a regular view's .Schema(...) and a SharedBaseView's
// base both carry it — so there is no separate declaration to keep in sync and
// no way to misspell it. A view with no schema is rejected at boot
// (ValidateViewSchemas), so in a booted service this is always the schema's
// table.
func (v *ViewDefinition) RootTable() string {
	if v.schema == nil {
		return ""
	}
	return v.schema.Table()
}

// MaxLimitValue returns the declared per-view cap or 0 when the consumer left
// the value unset. The reader is the only canonical consumer; it falls back
// to the yaml / framework defaults when this returns 0.
func (v *ViewDefinition) MaxLimitValue() int64 { return v.maxLimit }

// A leg (JoinUpstream / JoinView, defined in leg.go) deliberately
// exposes NO Embed/EmbedMany builder AND carries no embeds of its own: embed
// declarations are single-level BY CONSTRUCTION. Only a ViewDefinition declares
// embeds (top-level, any number); an embed's leg cannot nest a further one
// inline, so embed-of-embed is not expressible and does not compile. Depth
// beyond one hop comes from a JoinView leg's OWN declared embeds instead: the
// materialized segment carries whatever that view's document carries, and it
// stays fresh because every write to a view signals the views embedding it
// (the chain terminates because the embed graph is acyclic, boot-enforced by
// appendEmbedCycles).

// JoinColumn returns the physical column the composer joins this embed on — the
// column named via .On(...): the LEG's ParentID column for a one-to-many embed, the
// PARENT-side ParentID for a one-to-one embed. The verb's multiplicity fixes which
// side holds it; the column value is the same declaration either way.
func (e embedDef) JoinColumn() string { return e.joinCol }

// resolveGoSegment returns the parent-side Go field name for an embed (what the
// criteria/Response refer to) — the mandatory Go name declared on the leg
// constructor (JoinUpstream/JoinView).
func resolveGoSegment(e embedDef) string {
	if e.leg == nil {
		return ""
	}
	return e.leg.goSegment
}

// ValidateViewSchemas enforces the view-side mandatory-schema rule: every view
// declares a root schema, every embed declares one, and every external (type-
// less) embed declares its Go segment via .As(...) (a local embed derives it
// from its Go type). Returns a single error listing every offender so the
// operator sees them all in one boot attempt — a missing declaration is a fatal
// boot error, never a silent convention fallback.
func ValidateViewSchemas(views []*ViewDefinition) error {
	var problems []string
	// The registered set — a JoinView embed leg must name a view this service
	// contributes (mirrors the internal-leg check ValidateComposedViews runs).
	registered := make(map[string]bool, len(views))
	for _, v := range views {
		registered[v.Name()] = true
	}
	for _, v := range views {
		if v.schema == nil {
			problems = append(problems, fmt.Sprintf("view %q: no root .Schema(...) declared", v.Name()))
		} else if !v.schema.HasPKDeclared() {
			problems = append(problems, fmt.Sprintf(
				"view %q: root schema (table %q) declares no primary key — declare .ID(column)",
				v.Name(), v.schema.Table()))
		}
		// A SharedBaseView's .Schema(...) must be the identity's core.NewSharedBaseSchema
		// declaration (a role/table schema roots a regular query.View instead).
		if v.isSharedBaseView && v.schema != nil && !v.schema.IsSharedBase() {
			problems = append(problems, fmt.Sprintf(
				"view %q: SharedBaseView .Schema(...) must be a core.NewSharedBaseSchema declaration — "+
					"a role/table schema roots a regular query.View instead", v.Name()))
		}
		// The mirror rule: a regular query.View must NOT be rooted at a shared-base
		// schema — a shared identity (base + role sub-documents) is projected by
		// query.SharedBaseView(...).Role(...), never a plain view. Reject the
		// mis-wire at boot so the two constructors stay type-exclusive both ways.
		if !v.isSharedBaseView && v.schema != nil && v.schema.IsSharedBase() {
			problems = append(problems, fmt.Sprintf(
				"view %q: root schema (table %q) is a core.NewSharedBaseSchema — a shared-base identity is "+
					"projected by query.SharedBaseView(name).Schema(base).Role(...), not a plain query.View",
				v.Name(), v.schema.Table()))
		}
		// A SharedBaseView with no roles is a person view with nothing to
		// compose beyond the base — declare at least one specialization.
		// (Everything else about a role — type anchor, SharedBaseRef to this
		// base, declaration equivalence, duplicate segment — panics at
		// declaration time in Role().)
		if v.isSharedBaseView && len(v.roles) == 0 {
			problems = append(problems, fmt.Sprintf(
				"view %q: SharedBaseView declares no .Role(...) — add every role that specializes this identity",
				v.Name()))
		}
		// RelationalSource() is v1-limited to a plain, single-aggregate query.View
		// read from the SoR: a SharedBaseView or the Embed family is a
		// multi-source / read-time-join shape a single relational aggregate load
		// cannot serve. Reject the combination at boot (the escape is to drop the
		// marker and serve the view from Mongo).
		if v.IsRelational() {
			if v.isSharedBaseView {
				problems = append(problems, fmt.Sprintf(
					"view %q: RelationalSource() is not supported on a SharedBaseView (v1 serves a single plain query.View from the SoR)",
					v.Name()))
			}
			if len(v.embeds) > 0 {
				problems = append(problems, fmt.Sprintf(
					"view %q: RelationalSource() cannot be combined with Embed/EmbedMany — an embed is a Mongo read, not part of the relational aggregate load",
					v.Name()))
			}
			if len(v.childEmbeds) > 0 {
				problems = append(problems, fmt.Sprintf(
					"view %q: RelationalSource() cannot be combined with EmbedInChild",
					v.Name()))
			}
		}
		problems = appendSegmentCollisions(problems, v.Name(), v.schema, v.embeds, v.roles)
		problems = appendEmbedSchemaProblems(problems, v.Name(), v.embeds, registered)
		problems = appendChildEmbedProblems(problems, v.Name(), v, registered)
		problems = appendEmbedIndexProblems(problems, v.Name(), v)
	}
	// The view→view embed graph must be ACYCLIC: each hop's ripple writes the
	// embedding view, which fires the next hop's signal, so a cycle recomposes
	// forever. Checked across the whole set (a cycle is a property of the graph,
	// not of one declaration).
	problems = appendEmbedCycles(problems, views)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(
		"view schema(s) incomplete — every view (root + every embed) must declare a core.TableSchema, "+
			"and every embed names its join column via .On(...):\n  - %s",
		strings.Join(problems, "\n  - "),
	)
}

// viewHasIndexPrefix reports whether some declared index of the view has col as
// its FIRST key — a covering index for an equality (FindIDsByField) on col. A
// compound index serves a prefix equality, so only the first key qualifies.
func viewHasIndexPrefix(v *ViewDefinition, col string) bool {
	for _, spec := range v.IndexSpecs() {
		if len(spec.Keys) > 0 && spec.Keys[0].Field == col {
			return true
		}
	}
	return false
}

// appendChildEmbedProblems validates every EmbedInChild: the targeted schema MUST
// be a NATIVE child of the view root (declared via root.Child(...)); the leg MUST
// be a JoinUpstream leg (an external Mongo collection) with a ID; and the ParentID column
// inside the element MUST be named via .On(...). For a SharedBaseView the root
// native children are the BASE's children (role-nested children are out of scope).
func appendChildEmbedProblems(acc []string, viewName string, v *ViewDefinition, registered map[string]bool) []string {
	if v.schema == nil || len(v.childEmbeds) == 0 {
		return acc
	}
	nativeChildren := map[string]struct{}{}
	for _, ch := range v.schema.ChildSchemas() {
		nativeChildren[ch.Table()] = struct{}{}
	}
	for _, ce := range v.childEmbeds {
		if ce.childSchema == nil {
			acc = append(acc, fmt.Sprintf("view %q: EmbedInChild(...) has no child schema", viewName))
			continue
		}
		if _, ok := nativeChildren[ce.childSchema.Table()]; !ok {
			acc = append(acc, fmt.Sprintf(
				"view %q: EmbedInChild(%q, ...) — %q is NOT a native child of the view root %q; only a "+
					"schema declared via root.Child(...) can be enriched (for a SharedBaseView, a child of the BASE). "+
					"Declare it as a child, or embed at the root.",
				viewName, ce.childSchema.Table(), ce.childSchema.Table(), v.schema.Table()))
			continue
		}
		if ce.leg == nil {
			acc = append(acc, fmt.Sprintf("view %q: EmbedInChild(%q, ...) has no leg", viewName, ce.childSchema.Table()))
			continue
		}
		acc = appendLegFieldsProblems(acc, viewName,
			fmt.Sprintf("EmbedInChild(%q, ...)", ce.childSchema.Table()), ce.leg)
		if ce.leg.view != nil {
			// A JoinView enrichment: the source is a LOCAL view's own collection,
			// kept fresh by the same ripple (the SyncEngine signals every view it
			// materializes). Validate it like any internal leg; the external-only
			// checks below do not apply.
			acc = appendViewLegProblems(acc, viewName,
				fmt.Sprintf("EmbedInChild(%q, ...)", ce.childSchema.Table()), ce.leg.view, registered)
			if ce.joinCol == "" {
				acc = append(acc, fmt.Sprintf(
					"view %q: EmbedInChild(%q, ...) declares an empty join column — name the element's ParentID column via .On(\"...\").",
					viewName, ce.childSchema.Table()))
			}
			continue
		}
		if ce.leg.schema == nil {
			acc = append(acc, fmt.Sprintf("view %q: EmbedInChild(%q, ...) has no source schema", viewName, ce.childSchema.Table()))
			continue
		}
		if !ce.leg.schema.IsExternal() {
			acc = append(acc, fmt.Sprintf(
				"view %q: EmbedInChild(%q, ...) source %q is write-anchored — the source must be an EXTERNAL "+
					"collection (NewExternalSchema over an upstream projection), like a root Embed.",
				viewName, ce.childSchema.Table(), ce.leg.schema.Table()))
		}
		if !ce.leg.schema.HasPKDeclared() {
			acc = append(acc, fmt.Sprintf(
				"view %q: EmbedInChild(%q, ...) source %q declares no primary key — declare .ID(column)",
				viewName, ce.childSchema.Table(), ce.leg.schema.Table()))
		}
		if ce.joinCol == "" {
			acc = append(acc, fmt.Sprintf(
				"view %q: EmbedInChild(%q, ...) declares an empty join column — name the element's ParentID column via .On(\"...\").",
				viewName, ce.childSchema.Table()))
		}
	}
	return acc
}

// appendEmbedIndexProblems enforces the covering index the recompose ripple needs
// (a boot requirement, breaking retroactively for existing embeds). Only embeds
// whose ripple REVERSE-SCANS the view need one:
//   - a 1:1 root Embed → an index prefix on its parent join column;
//   - an EmbedInChild → a multikey index prefix on "<childSegment>.<fk>".
//
// EmbedMany is exempt: its ripple resolves the parent by the child's ParentID → parent
// _id (always indexed), never a reverse scan of the view.
func appendEmbedIndexProblems(acc []string, viewName string, v *ViewDefinition) []string {
	for _, e := range v.embeds {
		if e.many || e.leg == nil {
			continue
		}
		col := e.joinCol
		if col == "" {
			continue // schema-level problem already reported elsewhere
		}
		if !viewHasIndexPrefix(v, col) {
			acc = append(acc, fmt.Sprintf(
				"view %q: 1:1 Embed %q requires a covering index on its parent join column %q for the recompose "+
					"ripple's reverse scan — declare .Indexes(query.Index(%q)).",
				viewName, e.leg.externalName, col, col))
		}
	}
	for _, ce := range v.childEmbeds {
		if ce.leg == nil || ce.childSchema == nil || ce.joinCol == "" {
			continue
		}
		col := ce.ChildSegment() + "." + ce.joinCol
		if !viewHasIndexPrefix(v, col) {
			acc = append(acc, fmt.Sprintf(
				"view %q: EmbedInChild(%q, ...) requires a multikey index on %q for the recompose ripple's "+
					"reverse scan — declare .Indexes(query.Index(%q)).",
				viewName, ce.childSchema.Table(), col, col))
		}
	}
	return acc
}

func appendEmbedSchemaProblems(acc []string, viewName string, embeds []embedDef, registered map[string]bool) []string {
	// A leg exposes no Embed/EmbedMany builder of its own, so an embed declared
	// INSIDE a leg is not expressible. Depth beyond one hop comes from the leg
	// VIEW's own declared embeds instead, and stays fresh because every write to
	// that view signals the next hop (the graph is acyclic, enforced by
	// appendEmbedCycles). This validator checks each top-level embed's own leg.
	for _, e := range embeds {
		if e.leg == nil {
			continue
		}
		field := e.leg.externalName
		// The declared 1:N order is validated for BOTH leg kinds, before the
		// kind-specific branches below (each of which returns early).
		acc = appendEmbedOrderProblems(acc, viewName, e)
		// A declared Fields allowlist is validated for both kinds too: rejected
		// outright on an external leg (JoinView-only), entry-checked on a view leg.
		acc = appendLegFieldsProblems(acc, viewName, fmt.Sprintf("embed %q", field), e.leg)
		// A JoinView leg materializes a LOCAL view into this one. Validate it as an
		// internal leg (registered + schema + ID) plus, for a 1:N EmbedMany, the
		// covering index its per-parent lookup needs on the leg view.
		if e.leg.view != nil {
			kind := "Embed"
			if e.many {
				kind = "EmbedMany"
			}
			acc = appendViewLegProblems(acc, viewName, fmt.Sprintf("%s %q", kind, field), e.leg.view, registered)
			if e.joinCol == "" {
				acc = append(acc, fmt.Sprintf(
					"view %q: %s %q declares an empty join column — name it via .On(\"...\").",
					viewName, kind, field))
			} else if e.many && !viewHasIndexPrefix(e.leg.view, e.joinCol) {
				// The composer runs one find({fk: parent}) against the leg view per
				// parent document; without an index whose FIRST key is the ParentID, each
				// one is a full collection scan. The leg view declares its indexes,
				// so this is verifiable at boot — the same rule LinkMany applies to
				// an internal leg (composedLegIndexCovers).
				acc = append(acc, fmt.Sprintf(
					"view %q: EmbedMany %q materializes view %q on join column %q with NO covering index — "+
						"every parent document runs one find({%s: parent}) against %q, and without an index each "+
						"one is a full collection scan. Declare query.Index(%q) (or a compound index starting "+
						"with it) on the embedded view.",
					viewName, field, e.leg.view.Name(), e.joinCol, e.joinCol, e.leg.view.Name(), e.joinCol))
			}
			continue
		}
		if e.leg.schema == nil {
			acc = append(acc, fmt.Sprintf("view %q: embed %q has no schema", viewName, field))
		} else {
			// Embeds compose ONLY external data — another service's read model
			// (UpstreamSubscription / FromMongo) or a derived projection. A
			// write-anchored schema is the aggregate's own data, which projects
			// automatically from the TableSchema (root / siblings / SharedBase /
			// own children); declaring it as an embed is the redundant second path
			// the canonical split removes. Reject an anchored embed source at boot.
			if !e.leg.schema.IsExternal() {
				acc = append(acc, fmt.Sprintf(
					"view %q: embed %q (source %q) is a write-anchored schema — Embed/EmbedMany compose only "+
						"EXTERNAL data (another service's read model via UpstreamSubscription / FromMongo, or a "+
						"derived projection). A local aggregate's own data projects automatically from its "+
						"TableSchema; declare a 1:N child with .Child(...) on the root schema, not as an embed.",
					viewName, field, e.leg.schema.Table()))
			}
			if !e.leg.schema.HasPKDeclared() {
				acc = append(acc, fmt.Sprintf(
					"view %q: embed %q (source %q) declares no primary key — declare .ID(column)",
					viewName, field, e.leg.schema.Table()))
			}
			// Join key is mandatory and named via .On(...): EmbedMany joins on the
			// leg's ParentID (child_fk = parent_pk), a one-to-one Embed on the parent's ParentID.
			// An empty column makes the composer emit broken SQL, so reject it at boot.
			if e.joinCol == "" {
				kind := "one-to-one Embed"
				if e.many {
					kind = "EmbedMany"
				}
				acc = append(acc, fmt.Sprintf(
					"view %q: %s %q declares an empty join column — name it via .On(\"...\").",
					viewName, kind, field))
			}
		}
		// The leg's own schema-derived child segments still get a collision check;
		// embeds are single-level, so a leg contributes no further embeds to validate.
		acc = appendSegmentCollisions(acc, viewName, e.leg.schema, nil, nil)
	}
	return acc
}

// appendEmbedOrderProblems validates a declared 1:N element order: the column
// must exist on the SOURCE (the leg view's root schema, or the external
// schema), and .Desc() without .OrderBy(...) is a declaration mistake rather
// than a silent no-op. Runs for both leg kinds.
func appendEmbedOrderProblems(acc []string, viewName string, e embedDef) []string {
	if e.leg == nil {
		return acc
	}
	if e.orderDescOnly {
		acc = append(acc, fmt.Sprintf(
			"view %q: EmbedMany %q declares .Desc() without .OrderBy(column) — name the order column",
			viewName, e.Field()))
		return acc
	}
	if e.orderBy == "" {
		return acc
	}
	src := e.leg.schema
	if e.leg.view != nil {
		src = e.leg.view.schema
	}
	if src == nil {
		return acc // the missing-schema problem is reported elsewhere
	}
	if _, ok := src.GoNameForRead(e.orderBy); !ok {
		acc = append(acc, fmt.Sprintf(
			"view %q: EmbedMany %q declares OrderBy(%q), which is not a column of the embedded source %q — "+
				"the order is materialized INTO this view's documents, so it must name a column the source "+
				"projects", viewName, e.Field(), e.orderBy, e.leg.Collection()))
	}
	return acc
}

// appendViewLegProblems validates a JoinView embed leg — the source view must be
// contributed by a ReadableFeature (registered) and carry the root schema + ID the
// composer keys the lookup on. what describes the declaration site for the
// diagnostic (e.g. `EmbedMany "sales"`).
func appendViewLegProblems(acc []string, viewName, what string, leg *ViewDefinition, registered map[string]bool) []string {
	if leg.IsRelational() {
		acc = append(acc, fmt.Sprintf(
			"view %q: %s materializes view %q, which is a RelationalSource() view — a relational view is served from "+
				"the SoR and has NO Mongo collection to read, so it cannot be an embed source (the enrichment would "+
				"silently materialize an empty segment). Drop RelationalSource() from %q to make it a materialized "+
				"source, or embed a different view.",
			viewName, what, leg.Name(), leg.Name()))
		return acc
	}
	if !registered[leg.Name()] {
		acc = append(acc, fmt.Sprintf(
			"view %q: %s materializes view %q, which is not registered — an embedded view must be contributed "+
				"by a ReadableFeature (Views()), exactly like a composed view's internal leg.",
			viewName, what, leg.Name()))
		return acc
	}
	if leg.schema == nil {
		acc = append(acc, fmt.Sprintf(
			"view %q: %s materializes view %q, which declares no root .Schema(...)", viewName, what, leg.Name()))
		return acc
	}
	if !leg.schema.HasPKDeclared() {
		acc = append(acc, fmt.Sprintf(
			"view %q: %s materializes view %q, whose root schema (table %q) declares no primary key — "+
				"declare .ID(column)", viewName, what, leg.Name(), leg.schema.Table()))
	}
	return acc
}

// appendEmbedCycles rejects a CYCLE in the view→view embed graph. Every write to
// a view signals the views that embed it, so A embedding B while B embeds A (or
// any longer loop, or a view embedding itself) would recompose forever, each hop
// re-triggering the next. Depth is unlimited otherwise: an acyclic chain
// terminates because every path ends at a view with no view-sourced embed.
//
// Iterative DFS with a three-color marking; the reported path is the cycle as
// walked, so the operator sees exactly which declarations close the loop. Only
// view legs form edges — an external (JoinUpstream) leg is always a leaf.
func appendEmbedCycles(acc []string, views []*ViewDefinition) []string {
	edges := make(map[string][]string, len(views))
	for _, v := range views {
		var out []string
		for _, e := range v.embeds {
			if e.leg != nil && e.leg.view != nil {
				out = append(out, e.leg.view.Name())
			}
		}
		for _, ce := range v.childEmbeds {
			if ce.leg != nil && ce.leg.view != nil {
				out = append(out, ce.leg.view.Name())
			}
		}
		if len(out) > 0 {
			edges[v.Name()] = out
		}
	}
	if len(edges) == 0 {
		return acc
	}
	const (
		white = 0 // unvisited
		grey  = 1 // on the current path
		black = 2 // fully explored
	)
	color := map[string]int{}
	reported := map[string]bool{}
	var path []string
	var walk func(name string)
	walk = func(name string) {
		color[name] = grey
		path = append(path, name)
		for _, next := range edges[name] {
			switch color[next] {
			case grey:
				// Found the loop: report it from where `next` sits on the path.
				start := 0
				for i, p := range path {
					if p == next {
						start = i
						break
					}
				}
				cycle := append(append([]string(nil), path[start:]...), next)
				key := strings.Join(cycle, "\x00")
				if !reported[key] {
					reported[key] = true
					acc = append(acc, fmt.Sprintf(
						"view embed cycle: %s — a view materialized into another is refreshed by a ripple on every "+
							"write, so a loop would recompose forever. Break the cycle: keep the materialized "+
							"direction one-way and read the other side at request time with query.ComposedView.",
						strings.Join(cycle, " → ")))
				}
			case white:
				walk(next)
			}
		}
		path = path[:len(path)-1]
		color[name] = black
	}
	// Deterministic order: walk the views in declaration order so the same
	// declaration set always yields the same diagnostic.
	for _, v := range views {
		if color[v.Name()] == white {
			walk(v.Name())
		}
	}
	return acc
}

// appendSegmentCollisions flags a boot error when two sources would project into
// the SAME document segment at one schema level. Four producers can name a
// segment: an explicit embed field, an auto-derived base-child segment, an
// auto-derived own-child segment (both the pluralized child type), and — on a
// SharedBaseView root — a role segment (the role's type name). Each segment
// must have exactly one producer — a name clash, or a redundant explicit
// EmbedMany of a child the schema already projects automatically, is a boot error
// rather than a silent double projection / overwrite. A nil schema (already
// flagged elsewhere) contributes nothing. roles is non-empty only at a
// SharedBaseView root; the embed recursion passes nil.
func appendSegmentCollisions(acc []string, viewName string, schema *core.TableSchema, embeds []embedDef, roles []roleDef) []string {
	if schema == nil {
		return acc
	}
	owner := map[string]string{} // segment → producer description
	claim := func(seg, producer string) {
		if seg == "" {
			return
		}
		if prev, dup := owner[seg]; dup {
			acc = append(acc, fmt.Sprintf(
				"view %q: document segment %q is produced by both %s and %s — each segment has exactly one "+
					"source. A schema's own children (and a shared base's children) project automatically; drop the "+
					"redundant embed, or rename it.",
				viewName, seg, prev, producer))
			return
		}
		owner[seg] = producer
	}
	for _, e := range embeds {
		field := ""
		if e.leg != nil {
			field = e.leg.externalName
		}
		claim(resolveGoSegment(e), fmt.Sprintf("embed %q", field))
	}
	if base, _, ok := schema.SharedBaseRef(); ok {
		for _, bc := range base.ChildSchemas() {
			claim(childDocSegment(bc), fmt.Sprintf("base-child %q", bc.TypeName()))
		}
	}
	for _, child := range schema.ChildSchemas() {
		claim(childDocSegment(child), fmt.Sprintf("own child %q", child.TypeName()))
	}
	for _, r := range roles {
		claim(r.segment, fmt.Sprintf("role %q", r.segment))
	}
	return acc
}

// viewIndex splits the rebuild lookup by source kind. The original
// single-map implementation conflated Postgres tables and Mongo collection
// names — a PG-root view named "users" would collide in the lookup with a
// view embedding JoinUpstream(core.NewExternalSchema("users"), ...). The split keeps the namespaces separate:
//
//   - byPGTable: SyncEngine consults this on each Kafka message
//     (aggregate_type ≡ PG root table) to find every view that needs to be
//     recomposed.
//   - byMongoColl: UpstreamSubscriber consults this on each successful local
//     write (subscription.Collection ≡ key) to find every view embedding the
//     upstream collection that needs recompose-ripple.
//
// Each view contributes to both maps: its root table goes into byPGTable; for
// every embed, the embed's source goes into byPGTable when IsMongo()==false or
// into byMongoColl when IsMongo()==true. Recursive embeds traverse both maps
// uniformly.
type viewIndex struct {
	byPGTable   map[string][]*ViewDefinition
	byMongoColl map[string][]*ViewDefinition
	// bySharedBase maps a SharedBase table (e.g. "pessoa") to the role views that
	// reference it. A change to the shared identity fans out: every role view's
	// document referencing that identity is recomposed (SyncEngine.process).
	bySharedBase map[string][]*ViewDefinition
	// byViewSource maps a local VIEW NAME to the views that materialize it via a
	// JoinView embed leg. The SyncEngine consults it after every write to a view
	// document: the write is the signal the embedding views need, exactly as an
	// upstream mirror write is the signal byMongoColl serves. Keyed by the view's
	// logical name (never a physical slot) — the resolver maps it to the active
	// collection at read/write time.
	byViewSource map[string][]*ViewDefinition
	// byRoleTable maps a ROLE table to the base-rooted SharedBaseViews that
	// declare it as a role — the INVERSE direction of bySharedBase: a role
	// event (its table is not the view's root) must recompose the person
	// document of the identity the role references. The route carries the
	// roleDef so the SyncEngine can resolve the base id per the role's link
	// model (shared-ID vs separate-ParentID).
	byRoleTable map[string][]roleRoute
	// baseOfRole maps a ROLE table to its shared-base table. Since the
	// outbox payload carries the base id (_ids.base_id) on every role event,
	// the SyncEngine drives the shared-identity fan-out from the ROLE event
	// itself: baseOfRole names which bySharedBase entry to fan out for. Filled
	// from role views AND from SharedBaseView role declarations, so the
	// fan-out fires even when only one of the two exists.
	baseOfRole map[string]string
}

// roleRoute pairs a SharedBaseView with one of its declared roles — the
// recompose target for an event on that role's table.
type roleRoute struct {
	view *ViewDefinition
	role roleDef
}

// DependentMongoViews returns the subset of views that embed the named
// Mongo collection via an external query.JoinUpstream (at any nesting level). Used by
// bootstrap when wiring UpstreamSubscriber instances — the subscriber
// receives this slice as its recompose-ripple targets.
//
// O(views × embeds × nesting); typically small enough that a single linear
// walk at boot is cheaper than wiring a shared index map across packages.
func DependentMongoViews(views []*ViewDefinition, collection string) []*ViewDefinition {
	var out []*ViewDefinition
	for _, v := range views {
		if viewEmbedsMongoCollection(v.embeds, collection) || viewChildEmbedsMongoCollection(v.childEmbeds, collection) {
			out = append(out, v)
		}
	}
	return out
}

// EmbedSourceViews returns the names of the LOCAL views a view materializes
// through a query.JoinView leg (root embeds and child embeds alike) — its
// direct edges in the embed graph, and therefore the views that must be
// rebuilt BEFORE it.
func EmbedSourceViews(v *ViewDefinition) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(leg *Leg) {
		if leg == nil || leg.view == nil {
			return
		}
		if _, dup := seen[leg.view.Name()]; dup {
			return
		}
		seen[leg.view.Name()] = struct{}{}
		out = append(out, leg.view.Name())
	}
	for _, e := range v.embeds {
		add(e.leg)
	}
	for _, ce := range v.childEmbeds {
		add(ce.leg)
	}
	return out
}

// OrderViewsByEmbedDependency returns views reordered so that every view a
// query.JoinView leg materializes comes BEFORE the view embedding it — the
// order a rebuild must follow.
//
// Why it is not cosmetic: a rebuild composes its embed segments by reading the
// SOURCE view's active collection, and that pointer flips only when the
// source's own rebuild completes. Rebuilding an embedder first would therefore
// materialize copies of the source's pre-flip content and finish stale, with no
// event left to repair it (rebuild writes bypass the embed signal by design).
// The same bump that changes a source's shape forces its embedders to rebuild
// too (the leg's version rides in their hash), so unordered runs are the COMMON
// case, not an exotic one.
//
// Stable: views with no dependency between them keep their declaration order,
// so a service without view legs gets its input back untouched. The graph is
// boot-validated acyclic (appendEmbedCycles); a cycle that reached here anyway
// (a caller skipping validation) degrades to declaration order rather than
// looping.
func OrderViewsByEmbedDependency(views []*ViewDefinition) []*ViewDefinition {
	byName := make(map[string]*ViewDefinition, len(views))
	for _, v := range views {
		byName[v.Name()] = v
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make(map[string]int, len(views))
	out := make([]*ViewDefinition, 0, len(views))
	var visit func(v *ViewDefinition)
	visit = func(v *ViewDefinition) {
		switch color[v.Name()] {
		case black:
			return
		case grey:
			return // cycle (already rejected at boot) — do not loop
		}
		color[v.Name()] = grey
		for _, srcName := range EmbedSourceViews(v) {
			// A source outside this set (not registered, or not part of the
			// caller's subset) contributes no ordering constraint here.
			if src, ok := byName[srcName]; ok {
				visit(src)
			}
		}
		color[v.Name()] = black
		out = append(out, v)
	}
	for _, v := range views {
		visit(v)
	}
	return out
}

// DependentViewViews returns the subset of views that materialize the named
// LOCAL view via a JoinView embed leg (root Embed/EmbedMany or EmbedInChild) —
// the recompose-ripple targets a write to that view must refresh. The
// byViewSource counterpart of DependentMongoViews, kept as a standalone walk so
// bootstrap can wire the SyncEngine's ripplers without reaching into the index.
func DependentViewViews(views []*ViewDefinition, viewName string) []*ViewDefinition {
	var out []*ViewDefinition
	for _, v := range views {
		if viewEmbedsView(v.embeds, viewName) || viewChildEmbedsView(v.childEmbeds, viewName) {
			out = append(out, v)
		}
	}
	return out
}

func viewEmbedsView(embeds []embedDef, viewName string) bool {
	for _, e := range embeds {
		if e.leg != nil && e.leg.view != nil && e.leg.view.Name() == viewName {
			return true
		}
	}
	return false
}

func viewChildEmbedsView(childEmbeds []childEmbedDef, viewName string) bool {
	for _, ce := range childEmbeds {
		if ce.leg != nil && ce.leg.view != nil && ce.leg.view.Name() == viewName {
			return true
		}
	}
	return false
}

// viewChildEmbedsMongoCollection reports whether some EmbedInChild of the view
// enriches from the named collection — so a change to that upstream collection
// ripples into the view's child arrays.
func viewChildEmbedsMongoCollection(childEmbeds []childEmbedDef, collection string) bool {
	for _, ce := range childEmbeds {
		if ce.leg == nil {
			continue
		}
		if ce.leg.IsMongo() && ce.leg.Collection() == collection {
			return true
		}
	}
	return false
}

func viewEmbedsMongoCollection(embeds []embedDef, collection string) bool {
	for _, e := range embeds {
		if e.leg == nil {
			continue
		}
		if e.leg.IsMongo() && e.leg.Collection() == collection {
			return true
		}
	}
	return false
}

func buildViewIndex(views []*ViewDefinition) viewIndex {
	idx := viewIndex{
		byPGTable:    make(map[string][]*ViewDefinition),
		byMongoColl:  make(map[string][]*ViewDefinition),
		byViewSource: make(map[string][]*ViewDefinition),
		bySharedBase: make(map[string][]*ViewDefinition),
		byRoleTable:  make(map[string][]roleRoute),
		baseOfRole:   make(map[string]string),
	}
	for _, v := range views {
		// The root is always a Postgres table — UpstreamSubscription
		// projects upstream entities into Mongo collections that are
		// embedded, not chosen as a view root.
		idx.byPGTable[v.RootTable()] = append(idx.byPGTable[v.RootTable()], v)
		// A role view referencing a SharedBase is indexed by the base table, so a
		// base change fans out to every role view (SyncEngine.process). A
		// base-rooted SharedBaseView never lands here — its root schema IS the
		// base (no SharedBaseRef); base events reach it through byPGTable.
		if v.schema != nil {
			if base, _, ok := v.schema.SharedBaseRef(); ok {
				idx.bySharedBase[base.Table()] = append(idx.bySharedBase[base.Table()], v)
				idx.baseOfRole[v.RootTable()] = base.Table()
			}
		}
		// A SharedBaseView is indexed by each ROLE table too: a role event must
		// recompose the person document (the inverse of bySharedBase).
		for _, r := range v.roles {
			idx.byRoleTable[r.schema.Table()] = append(idx.byRoleTable[r.schema.Table()], roleRoute{view: v, role: r})
			if base, _, ok := r.schema.SharedBaseRef(); ok {
				idx.baseOfRole[r.schema.Table()] = base.Table()
			}
		}
		indexEmbeds(v.embeds, v.childEmbeds, v, idx)
	}
	return idx
}

// indexEmbeds routes each embed source into the map that serves ITS writer:
// a JoinView leg → byViewSource (the SyncEngine signals on every write to that
// view), an external mirror → byMongoColl (the UpstreamSubscriber signals),
// anything else → byPGTable. The child-embed sources are routed too: an
// EmbedInChild enrichment needs the identical signal, only landing inside a
// child array.
func indexEmbeds(embeds []embedDef, childEmbeds []childEmbedDef, v *ViewDefinition, idx viewIndex) {
	route := func(leg *Leg) {
		if leg == nil {
			return
		}
		switch {
		case leg.view != nil:
			name := leg.view.Name()
			idx.byViewSource[name] = append(idx.byViewSource[name], v)
		case leg.IsMongo():
			idx.byMongoColl[leg.Collection()] = append(idx.byMongoColl[leg.Collection()], v)
		default:
			idx.byPGTable[leg.Collection()] = append(idx.byPGTable[leg.Collection()], v)
		}
	}
	for _, e := range embeds {
		route(e.leg)
	}
	for _, ce := range childEmbeds {
		// A child-embed of a MIRROR is already reached through DependentMongoViews
		// at subscriber wiring; only the view-sourced ones need the index entry, and
		// routing both keeps the map complete for any future consumer.
		if ce.leg != nil && ce.leg.view != nil {
			route(ce.leg)
		}
	}
}
