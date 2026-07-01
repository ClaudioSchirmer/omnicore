package query

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

type ViewDefinition struct {
	name            string
	version         int
	rootTable       string
	schema          *core.TableSchema
	embeds          []embedDef
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
}

type embedDef struct {
	field  string
	source *Source
	many   bool
}

// Source exposes the read-only Source associated with this embed. Used by
// bootstrap-side boot guards that walk Embeds() and must inspect IsMongo /
// Collection / JoinKey without reaching into private fields across package
// boundaries.
func (e embedDef) Source() *Source { return e.source }

// Field returns the document field name where the embed lands in the
// composed Mongo document. Symmetric with Source() for boot guards.
func (e embedDef) Field() string { return e.field }

// Many reports whether the embed is EmbedMany (one-to-many) vs Embed
// (one-to-one). Boot guards do not consume it today; exposed for symmetry
// with Field/Source so future external inspection has the same surface as
// the framework's own dispatch.
func (e embedDef) Many() bool { return e.many }

// Source describes one fetch leaf inside a ViewDefinition. The `table` field
// carries either a Postgres table (for FromSchema over a type-anchored schema)
// or a Mongo collection name (for FromSchema over a type-less
// NewExternalSchema); isMongo discriminates.
// The composer dispatches on IsMongo() to pick the underlying store:
//
//   - false (default): fetch via Postgres pool (fetchRow / fetchWhere)
//   - true:            fetch via MongoDB.FindManyByField against the local DB
//
// Mongo-kind sources are the embedding surface for upstream-projected
// collections (declared via bootstrap.UpstreamSubscription) as well as
// composition between B's own Mongo views — the boot guard §8.3 enforces that
// `table` resolves either to an UpstreamSubscription.Collection or to a local
// ViewDefinition.Name().
type Source struct {
	table     string
	joinKey   string
	isMongo   bool
	schema    *core.TableSchema
	goSegment string
	embeds    []embedDef
}

func View(name string) *ViewDefinition {
	return &ViewDefinition{name: name}
}

func (v *ViewDefinition) Root(table string) *ViewDefinition {
	v.rootTable = table
	return v
}

// Schema attaches the view's root core.TableSchema (Go↔column + PK + soft-delete) —
// the same schema the repository declares. The composer uses it for the root
// PK + soft-delete column; the reader uses it to translate root leaf fields
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
// read side mirrors PostgreSQL symmetrically, and the composer omits the
// WHERE deleted_at IS NULL filter on both the root SELECT and on every
// EmbedMany/Embed source. When this builder is called, ARCHIVED events
// instead remove the document from the Mongo collection and the composer
// applies the WHERE deleted_at IS NULL filter on root + every embed source
// (cascade: the flag governs the whole aggregate projection — there is no
// per-embed override).
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
// delete-on-archive covers soft deletes only.
func (v *ViewDefinition) DeleteOnArchive() *ViewDefinition {
	v.deleteOnArchive = true
	return v
}

func (v *ViewDefinition) Embed(field string, src *Source) *ViewDefinition {
	v.embeds = append(v.embeds, embedDef{field: field, source: src, many: false})
	return v
}

func (v *ViewDefinition) EmbedMany(field string, src *Source) *ViewDefinition {
	v.embeds = append(v.embeds, embedDef{field: field, source: src, many: true})
	return v
}

// MaxLimit overrides the per-view ceiling on `?limit=` for endpoints reading
// from this projection. Applies uniformly to every endpoint that consults the
// view, regardless of how many handlers point at it — the cap describes the
// cost of reading this specific dataset, not the cost of any single endpoint.
//
// Resolution at read time: this value (when > 0) wins; otherwise the yaml
// default `query.maxLimit` wins; otherwise the framework default 100. A
// `?limit=N` greater than the resolved ceiling is rejected with 400
// SchemaViolationNotification at the wire boundary.
//
// NOT part of RebuildHash / ArtifactHash: the cap is operational state, not
// projection shape. Bumping it neither triggers a Mongo rebuild nor requires
// a Version(N) bump.
func (v *ViewDefinition) MaxLimit(n int64) *ViewDefinition {
	v.maxLimit = n
	return v
}

func (v *ViewDefinition) Name() string           { return v.name }
func (v *ViewDefinition) VersionNumber() int     { return v.version }
func (v *ViewDefinition) RootTable() string      { return v.rootTable }
func (v *ViewDefinition) Embeds() []embedDef     { return v.embeds }
func (v *ViewDefinition) DeletesOnArchive() bool { return v.deleteOnArchive }

// MaxLimitValue returns the declared per-view cap or 0 when the consumer left
// the value unset. The reader is the only canonical consumer; it falls back
// to the yaml / framework defaults when this returns 0.
func (v *ViewDefinition) MaxLimitValue() int64 { return v.maxLimit }

// FromSchema produces an embed source from a core.TableSchema — the single,
// mandatory source constructor. Everything physical is derived from the schema:
//
//   - the table / collection is ts.Table();
//   - the source kind is the schema's kind — a type-less core.NewExternalSchema
//     describes an upstream Mongo collection (Mongo source), a type-anchored
//     NewTableSchema a local Postgres table;
//   - for an EmbedMany the join key is the schema's FK column (declared via
//     .FK(...)), so it is never repeated on the embed.
//
// The schema is required on every embed: the read membrane translates Go↔column
// through it, so there is no convention fallback (no inferred "id"/"deleted_at",
// no identity pass-through). A local FromSchema source (type-anchored schema)
// reuses the child's repository schema; an upstream FromSchema source over a
// NewExternalSchema describes the upstream's columns.
func FromSchema(ts *core.TableSchema) *Source {
	return &Source{table: ts.Table(), isMongo: ts.IsExternal(), schema: ts}
}

// On declares the join key for a one-to-one Embed: the column on the PARENT
// document holding the foreign key that points at this source's primary key.
// It is NOT used for EmbedMany — there the join is the child FK declared on the
// source's own schema via .FK(...). Has no effect on an EmbedMany source.
func (s *Source) On(key string) *Source {
	s.joinKey = key
	return s
}

// As declares the parent-side Go field name (segment) for this embed — what the
// criteria/Response refer to (e.g. "Addresses"), as opposed to the doc field
// where the composer lands the embed (the EmbedMany field, e.g. "addresses").
// Required when the two differ (the common case: snake doc field vs PascalCase
// Go field); defaults to the doc field when unset.
func (s *Source) As(goSegment string) *Source {
	s.goSegment = goSegment
	return s
}

func (s *Source) Embed(field string, src *Source) *Source {
	s.embeds = append(s.embeds, embedDef{field: field, source: src, many: false})
	return s
}

func (s *Source) EmbedMany(field string, src *Source) *Source {
	s.embeds = append(s.embeds, embedDef{field: field, source: src, many: true})
	return s
}

func (s *Source) Table() string      { return s.table }
func (s *Source) JoinKey() string    { return s.joinKey }
func (s *Source) IsMongo() bool      { return s.isMongo }
func (s *Source) Collection() string { return s.table }
func (s *Source) Embeds() []embedDef { return s.embeds }

// SchemaDef returns the embed source's core.TableSchema (always set — FromSchema is
// the only constructor). Symmetric with ViewDefinition.SchemaDef().
func (s *Source) SchemaDef() *core.TableSchema { return s.schema }

// JoinColumn returns the physical column the composer joins this embed on:
// the source's FK column (from the schema) for a one-to-many embed, or the
// parent-side FK declared via .On for a one-to-one embed.
func (e embedDef) JoinColumn() string {
	if e.many {
		return e.source.schema.FKColumn()
	}
	return e.source.joinKey
}

// resolveGoSegment returns the parent-side Go field name for an embed (what the
// criteria/Response refer to). Resolution:
//
//   - an explicit .As value wins;
//   - otherwise, for a LOCAL (type-anchored) source it is derived from the
//     schema's Go type — pluralized for a one-to-many EmbedMany ("Address" →
//     "Addresses"), the type name itself for a one-to-one Embed;
//   - for an EXTERNAL (type-less) source with no .As it returns "" — there is
//     no Go type to inherit from, so .As is required; the boot guard rejects it.
func resolveGoSegment(e embedDef) string {
	if e.source == nil {
		return ""
	}
	if e.source.goSegment != "" {
		return e.source.goSegment
	}
	name := e.source.schema.TypeName() // "" for an external schema
	if name == "" {
		return ""
	}
	if e.many {
		return domain.PluralizeWord(name)
	}
	return name
}

// ValidateViewSchemas enforces the view-side mandatory-schema rule: every view
// declares a root schema, every embed declares one, and every external (type-
// less) embed declares its Go segment via .As(...) (a local embed derives it
// from its Go type). Returns a single error listing every offender so the
// operator sees them all in one boot attempt — a missing declaration is a fatal
// boot error, never a silent convention fallback.
func ValidateViewSchemas(views []*ViewDefinition) error {
	var problems []string
	for _, v := range views {
		if v.schema == nil {
			problems = append(problems, fmt.Sprintf("view %q: no root .Schema(...) declared", v.Name()))
		} else if !v.schema.HasPKDeclared() {
			problems = append(problems, fmt.Sprintf(
				"view %q: root schema (table %q) declares no primary key — declare .PK(column)",
				v.Name(), v.schema.Table()))
		}
		problems = appendSegmentCollisions(problems, v.Name(), v.schema, v.embeds)
		problems = appendEmbedSchemaProblems(problems, v.Name(), v.embeds)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(
		"view schema(s) incomplete — every view (root + every embed) must declare a core.TableSchema, "+
			"and every external embed must declare its Go segment via .As(...):\n  - %s",
		strings.Join(problems, "\n  - "),
	)
}

func appendEmbedSchemaProblems(acc []string, viewName string, embeds []embedDef) []string {
	for _, e := range embeds {
		if e.source == nil {
			continue
		}
		if e.source.schema == nil {
			acc = append(acc, fmt.Sprintf("view %q: embed %q (source %q) has no schema", viewName, e.field, e.source.table))
		} else {
			// Embeds compose ONLY external data — another service's read model
			// (UpstreamSubscription / FromMongo) or a derived projection. A
			// write-anchored schema is the aggregate's own data, which projects
			// automatically from the TableSchema (root / siblings / SharedBase /
			// own children); declaring it as an embed is the redundant second path
			// the canonical split removes. Reject an anchored embed source at boot.
			if !e.source.schema.IsExternal() {
				acc = append(acc, fmt.Sprintf(
					"view %q: embed %q (source %q) is a write-anchored schema — Embed/EmbedMany compose only "+
						"EXTERNAL data (another service's read model via UpstreamSubscription / FromMongo, or a "+
						"derived projection). A local aggregate's own data projects automatically from its "+
						"TableSchema; declare a 1:N child with .Child(...) on the root schema, not as an embed.",
					viewName, e.field, e.source.table))
			}
			if resolveGoSegment(e) == "" {
				acc = append(acc, fmt.Sprintf(
					"view %q: external embed %q (source %q) has no Go segment — declare it via .As(\"...\") "+
						"(an external/type-less schema cannot derive it from a Go type)",
					viewName, e.field, e.source.table))
			}
			if !e.source.schema.HasPKDeclared() {
				acc = append(acc, fmt.Sprintf(
					"view %q: embed %q (source %q) declares no primary key — declare .PK(column)",
					viewName, e.field, e.source.table))
			}
			// Join key is mandatory: EmbedMany joins on the child's FK (declared on
			// its schema via .FK), a one-to-one Embed joins on the parent's FK
			// (declared via .On). Either missing makes the composer emit broken SQL,
			// so reject it at boot instead.
			if e.many {
				if e.source.schema.FKColumn() == "" {
					acc = append(acc, fmt.Sprintf(
						"view %q: EmbedMany %q (source %q) declares no foreign key — declare .FK(col) on "+
							"its schema; the composer joins the child rows on it (child_fk = parent_pk)",
						viewName, e.field, e.source.table))
				}
			} else if e.source.joinKey == "" {
				acc = append(acc, fmt.Sprintf(
					"view %q: one-to-one Embed %q (source %q) declares no parent join key — declare "+
						".On(col) naming the parent column that holds the FK to this source's PK",
					viewName, e.field, e.source.table))
			}
		}
		acc = appendSegmentCollisions(acc, viewName, e.source.schema, e.source.embeds)
		acc = appendEmbedSchemaProblems(acc, viewName, e.source.embeds)
	}
	return acc
}

// appendSegmentCollisions flags a boot error when two sources would project into
// the SAME document segment at one schema level. Three producers can name a
// segment: an explicit embed field, an auto-derived base-child segment, and an
// auto-derived own-child segment (both the pluralized child type). Each segment
// must have exactly one producer — a name clash, or a redundant explicit
// EmbedMany of a child the schema already projects automatically, is a boot error
// rather than a silent double projection / overwrite. A nil schema (already
// flagged elsewhere) contributes nothing.
func appendSegmentCollisions(acc []string, viewName string, schema *core.TableSchema, embeds []embedDef) []string {
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
		claim(resolveGoSegment(e), fmt.Sprintf("embed %q", e.field))
	}
	if base, _, ok := schema.SharedBaseRef(); ok {
		for _, bc := range base.ChildSchemas() {
			claim(childDocSegment(bc), fmt.Sprintf("base-child %q", bc.TypeName()))
		}
	}
	for _, child := range schema.ChildSchemas() {
		claim(childDocSegment(child), fmt.Sprintf("own child %q", child.TypeName()))
	}
	return acc
}

// viewIndex splits the rebuild lookup by source kind. The original
// single-map implementation conflated Postgres tables and Mongo collection
// names — a PG-root view named "users" would collide in the lookup with a
// view embedding FromSchema(core.NewExternalSchema("users")). The split keeps the namespaces separate:
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
}

// DependentMongoViews returns the subset of views that embed the named
// Mongo collection via an external fwinfra.FromSchema (at any nesting level). Used by
// bootstrap when wiring UpstreamSubscriber instances — the subscriber
// receives this slice as its recompose-ripple targets.
//
// O(views × embeds × nesting); typically small enough that a single linear
// walk at boot is cheaper than wiring a shared index map across packages.
func DependentMongoViews(views []*ViewDefinition, collection string) []*ViewDefinition {
	var out []*ViewDefinition
	for _, v := range views {
		if viewEmbedsMongoCollection(v.embeds, collection) {
			out = append(out, v)
		}
	}
	return out
}

func viewEmbedsMongoCollection(embeds []embedDef, collection string) bool {
	for _, e := range embeds {
		if e.source == nil {
			continue
		}
		if e.source.IsMongo() && e.source.Collection() == collection {
			return true
		}
		if len(e.source.embeds) > 0 && viewEmbedsMongoCollection(e.source.embeds, collection) {
			return true
		}
	}
	return false
}

func buildViewIndex(views []*ViewDefinition) viewIndex {
	idx := viewIndex{
		byPGTable:    make(map[string][]*ViewDefinition),
		byMongoColl:  make(map[string][]*ViewDefinition),
		bySharedBase: make(map[string][]*ViewDefinition),
	}
	for _, v := range views {
		// The root is always a Postgres table — UpstreamSubscription
		// projects upstream entities into Mongo collections that are
		// embedded, not chosen as a view root.
		idx.byPGTable[v.rootTable] = append(idx.byPGTable[v.rootTable], v)
		// A role view referencing a SharedBase is indexed by the base table, so a
		// base change fans out to every role view (SyncEngine.process).
		if v.schema != nil {
			if base, _, ok := v.schema.SharedBaseRef(); ok {
				idx.bySharedBase[base.Table()] = append(idx.bySharedBase[base.Table()], v)
			}
		}
		indexEmbeds(v.embeds, v, idx)
	}
	return idx
}

func indexEmbeds(embeds []embedDef, v *ViewDefinition, idx viewIndex) {
	for _, e := range embeds {
		if e.source.isMongo {
			idx.byMongoColl[e.source.table] = append(idx.byMongoColl[e.source.table], v)
		} else {
			idx.byPGTable[e.source.table] = append(idx.byPGTable[e.source.table], v)
		}
		if len(e.source.embeds) > 0 {
			indexEmbeds(e.source.embeds, v, idx)
		}
	}
}
