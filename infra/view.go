package infra

type ViewDefinition struct {
	name            string
	version         int
	rootTable       string
	schema          *TableSchema
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
// carries either a Postgres table (for sources produced by From) or a Mongo
// collection name (for sources produced by FromMongo); isMongo discriminates.
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
	schema    *TableSchema
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

// Schema attaches the view's root TableSchema (Go↔column + PK + soft-delete) —
// the same schema the repository declares. The composer uses it for the root
// PK + soft-delete column; the reader uses it to translate root leaf fields
// between Go field names and physical columns. Reuse the repo's schema so write
// and read agree.
func (v *ViewDefinition) Schema(ts *TableSchema) *ViewDefinition {
	v.schema = ts
	return v
}

// SchemaDef returns the view's root TableSchema (nil when unset).
func (v *ViewDefinition) SchemaDef() *TableSchema { return v.schema }

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

func (v *ViewDefinition) Name() string             { return v.name }
func (v *ViewDefinition) VersionNumber() int       { return v.version }
func (v *ViewDefinition) RootTable() string        { return v.rootTable }
func (v *ViewDefinition) Embeds() []embedDef       { return v.embeds }
func (v *ViewDefinition) DeletesOnArchive() bool   { return v.deleteOnArchive }

// MaxLimitValue returns the declared per-view cap or 0 when the consumer left
// the value unset. The reader is the only canonical consumer; it falls back
// to the yaml / framework defaults when this returns 0.
func (v *ViewDefinition) MaxLimitValue() int64 { return v.maxLimit }

// From produces a Postgres-kind Source. The composer reads from the connection
// pool (fetchRow / fetchWhere) using the joinKey set via .On(...). Use this for
// every embed whose data lives in the same Postgres database as the view's root
// table — the canonical case for local aggregate children.
func From(table string) *Source {
	return &Source{table: table}
}

// FromMongo produces a Mongo-kind Source. The composer reads from the local
// MongoDB via FindManyByField/fetchRow on the named collection. Use this for
// embeds whose data is upstream-projected into a B.Mongo collection by an
// UpstreamSubscription, or for composing one local Mongo view inside another.
// The collection name MUST resolve at boot to either an UpstreamSubscription
// or a local ViewDefinition.Name() — boot guard §8.3 enforces this and aborts
// otherwise so a typo never lands as a silently empty embed in production.
func FromMongo(collection string) *Source {
	return &Source{table: collection, isMongo: true}
}

func (s *Source) On(key string) *Source {
	s.joinKey = key
	return s
}

// Schema attaches the embed source's TableSchema (Go↔column map + PK +
// soft-delete). For a local From source, reuse the child's repository schema;
// for a FromMongo source, declare an external schema describing the upstream's
// columns. The reader uses it to translate the embed's leaf fields both ways.
func (s *Source) Schema(ts *TableSchema) *Source {
	s.schema = ts
	return s
}

// As declares the parent-side Go field name (segment) for this embed — what the
// criteria/Response refer to (e.g. "Addresses"), as opposed to the doc field
// where the composer lands the embed (the EmbedMany field, e.g. "addresses").
// Defaults to the doc field when unset.
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

// SchemaDef returns the embed source's TableSchema, or nil when the embed was
// declared without .Schema(...). Symmetric with ViewDefinition.SchemaDef();
// consumed by the bootstrap-side schema-less-embed advisory guard, which must
// inspect schema presence across the package boundary without reaching into
// the private field.
func (s *Source) SchemaDef() *TableSchema { return s.schema }

// viewIndex splits the rebuild lookup by source kind. The original
// single-map implementation conflated Postgres tables and Mongo collection
// names — a PG-root view named "users" would collide in the lookup with a
// view embedding FromMongo("users"). The split keeps the namespaces separate:
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
}

// DependentMongoViews returns the subset of views that embed the named
// Mongo collection via fwinfra.FromMongo (at any nesting level). Used by
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
		byPGTable:   make(map[string][]*ViewDefinition),
		byMongoColl: make(map[string][]*ViewDefinition),
	}
	for _, v := range views {
		// The root is always a Postgres table — UpstreamSubscription
		// projects upstream entities into Mongo collections that are
		// embedded, not chosen as a view root.
		idx.byPGTable[v.rootTable] = append(idx.byPGTable[v.rootTable], v)
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
