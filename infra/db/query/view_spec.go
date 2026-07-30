package query

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoSpec groups the declarative artifacts a ViewDefinition asks the
// framework to materialize on the Mongo side at boot. Kept as an unexported
// sub-struct of ViewDefinition so view.go stays focused on the document
// shape (root + embeds + archive policy) while this file owns indexes,
// validators, collation, capped and time-series options.
type mongoSpec struct {
	indexes    []*IndexSpec
	jsonSchema *JSONSchemaSpec
	collation  *CollationSpec
	capped     *CappedSpec
	timeSeries *TimeSeriesSpec
}

// IndexKeyOrder is the per-field direction within an IndexSpec.
// Encodes the Mongo wire vocabulary so we never invent a key shape the
// driver does not honor; values map 1:1 to the integers / strings the
// driver puts inside the IndexModel.Keys bson document.
type IndexKeyOrder string

const (
	IndexOrderAsc      IndexKeyOrder = "asc"      // {field: 1}
	IndexOrderDesc     IndexKeyOrder = "desc"     // {field: -1}
	IndexOrderText     IndexKeyOrder = "text"     // {field: "text"}
	IndexOrderGeo2D    IndexKeyOrder = "2d"       // {field: "2d"}
	IndexOrderGeo2DSph IndexKeyOrder = "2dsphere" // {field: "2dsphere"}
	IndexOrderHashed   IndexKeyOrder = "hashed"   // {field: "hashed"}
)

// IndexKey is one (field, order) pair within an IndexSpec.
type IndexKey struct {
	Field string
	Order IndexKeyOrder
}

// IndexSpec declares one Mongo index in the consumer's vocabulary. Built
// via the fluent constructors (Index, Compound, TextIndex, GeoIndex) and
// the setter methods below; consumed by ViewDefinition.Indexes(...). The
// driver-native IndexModel is materialized lazily via IndexModel() so the
// declarative spec stays the single source of truth (round-trippable,
// hashable, comparable across boots — properties Phase B's apply step
// relies on).
type IndexSpec struct {
	Keys               []IndexKey
	name               string
	unique             bool
	sparse             bool
	hidden             bool
	partialFilter      bson.M
	expireAfterSeconds *int32
	collation          *CollationSpec

	// Text-specific (only honored when at least one key has IndexOrderText)
	weights          map[string]int
	defaultLanguage  string
	languageOverride string

	// Geospatial-specific
	geoMin        *float64
	geoMax        *float64
	geoBits       *int32
	geoBucketSize *int32
}

// Index builds a single-field ascending B-tree index. Chain setters to
// configure (Unique, Sparse, Partial, TTL, …) and pass the result into
// ViewDefinition.Indexes(...).
func Index(field string) *IndexSpec {
	return &IndexSpec{Keys: []IndexKey{{Field: field, Order: IndexOrderAsc}}}
}

// Compound builds an ordered multi-field ascending index. The order of the
// fields matters: Mongo's ESR rule (equality → sort → range) drives which
// queries this index can serve.
func Compound(fields ...string) *IndexSpec {
	keys := make([]IndexKey, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, IndexKey{Field: f, Order: IndexOrderAsc})
	}
	return &IndexSpec{Keys: keys}
}

// TextIndex builds a $text index over the given fields. Mongo allows at
// most one text index per collection — the framework enforces this at
// boot via ViewDefinition.ValidateMongoSpec().
func TextIndex(fields ...string) *IndexSpec {
	keys := make([]IndexKey, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, IndexKey{Field: f, Order: IndexOrderText})
	}
	return &IndexSpec{Keys: keys}
}

// GeoIndex builds a 2dsphere geospatial index for $near / $geoWithin queries.
func GeoIndex(field string) *IndexSpec {
	return &IndexSpec{Keys: []IndexKey{{Field: field, Order: IndexOrderGeo2DSph}}}
}

// Name sets an explicit index name (overrides Mongo's auto-derivation
// like "email_1_created_at_-1"). Use sparingly; the auto name is stable
// and human-readable.
func (s *IndexSpec) Name(name string) *IndexSpec { s.name = name; return s }

// Unique marks the index unique.
func (s *IndexSpec) Unique() *IndexSpec { s.unique = true; return s }

// Sparse marks the index sparse (documents missing the field are skipped).
func (s *IndexSpec) Sparse() *IndexSpec { s.sparse = true; return s }

// Hidden makes the index invisible to the query planner without dropping
// it — useful to evaluate drop safety before removing the declaration.
func (s *IndexSpec) Hidden() *IndexSpec { s.hidden = true; return s }

// Desc flips the LAST ascending key of the spec to descending. Allows
// canonical chains like Index("created_at").Desc() and
// Compound("email", "created_at").Desc() (only the trailing key flips —
// declare both fields explicitly when both must be descending).
func (s *IndexSpec) Desc() *IndexSpec {
	if n := len(s.Keys); n > 0 && s.Keys[n-1].Order == IndexOrderAsc {
		s.Keys[n-1].Order = IndexOrderDesc
	}
	return s
}

// Hashed flips the LAST key of the spec to a hashed key — used to seed
// range-spread shard keys (the supporting index lives on the collection
// before sharding is enabled by the cluster operator).
func (s *IndexSpec) Hashed() *IndexSpec {
	if n := len(s.Keys); n > 0 {
		s.Keys[n-1].Order = IndexOrderHashed
	}
	return s
}

// Partial declares a partialFilterExpression. Pair with the Exists helper
// to scope an index to a subset of documents (canonical example:
// Index("deleted_at").Partial(Exists("deleted_at", false)) to thin the
// index of archived rows).
func (s *IndexSpec) Partial(filter bson.M) *IndexSpec {
	s.partialFilter = filter
	return s
}

// TTL turns the index into a TTL index — Mongo deletes documents whose
// indexed timestamp is older than d. Granularity is seconds (Mongo's
// expireAfterSeconds is int32); sub-second precision is rounded down.
func (s *IndexSpec) TTL(d time.Duration) *IndexSpec {
	secs := int32(d / time.Second)
	s.expireAfterSeconds = &secs
	return s
}

// Collation overrides the collection's default collation for this index.
func (s *IndexSpec) Collation(c *CollationSpec) *IndexSpec {
	s.collation = c
	return s
}

// Weights sets per-field weights for a text index (text-only). Higher
// weight pushes matches in that field higher in the $meta:"textScore"
// ranking.
func (s *IndexSpec) Weights(w map[string]int) *IndexSpec { s.weights = w; return s }

// DefaultLanguage sets the language used by the text-index tokenizer when
// the document does not carry a language override field (text-only).
func (s *IndexSpec) DefaultLanguage(lang string) *IndexSpec {
	s.defaultLanguage = lang
	return s
}

// LanguageOverride names the document field whose value overrides the
// default language per document (text-only). Useful for multilingual
// collections where each document carries its own "lang" field.
func (s *IndexSpec) LanguageOverride(field string) *IndexSpec {
	s.languageOverride = field
	return s
}

// Min sets the inclusive minimum boundary for legacy 2d indexes.
func (s *IndexSpec) Min(v float64) *IndexSpec { s.geoMin = &v; return s }

// Max sets the inclusive maximum boundary for legacy 2d indexes.
func (s *IndexSpec) Max(v float64) *IndexSpec { s.geoMax = &v; return s }

// Bits sets the geohash precision for 2dsphere indexes.
func (s *IndexSpec) Bits(v int32) *IndexSpec { s.geoBits = &v; return s }

// BucketSize sets the bucket size for haystack-style 2d indexes (legacy).
func (s *IndexSpec) BucketSize(v int32) *IndexSpec { s.geoBucketSize = &v; return s }

// IsText reports whether any of the spec's keys participate as a $text
// field — drives the at-most-one-text-index-per-view boot invariant.
func (s *IndexSpec) IsText() bool {
	for _, k := range s.Keys {
		if k.Order == IndexOrderText {
			return true
		}
	}
	return false
}

// IndexModel materializes the declarative spec into the driver-native
// mongo.IndexModel value Phase B's ApplyMongoSpecs feeds straight into
// Collection.Indexes().CreateMany. Keeping the conversion centralized
// here means a spec change touches exactly one place; consumers only
// see the declarative surface.
func (s *IndexSpec) IndexModel() mongo.IndexModel {
	keys := bson.D{}
	for _, k := range s.Keys {
		keys = append(keys, bson.E{Key: k.Field, Value: encodeOrder(k.Order)})
	}
	opts := options.Index()
	if s.name != "" {
		opts.SetName(s.name)
	}
	if s.unique {
		opts.SetUnique(true)
	}
	if s.sparse {
		opts.SetSparse(true)
	}
	if s.hidden {
		opts.SetHidden(true)
	}
	if s.partialFilter != nil {
		opts.SetPartialFilterExpression(s.partialFilter)
	}
	if s.expireAfterSeconds != nil {
		opts.SetExpireAfterSeconds(*s.expireAfterSeconds)
	}
	if s.collation != nil {
		opts.SetCollation(s.collation.DriverCollation())
	}
	if len(s.weights) > 0 {
		weightsDoc := bson.D{}
		for k, v := range s.weights {
			weightsDoc = append(weightsDoc, bson.E{Key: k, Value: v})
		}
		opts.SetWeights(weightsDoc)
	}
	if s.defaultLanguage != "" {
		opts.SetDefaultLanguage(s.defaultLanguage)
	}
	if s.languageOverride != "" {
		opts.SetLanguageOverride(s.languageOverride)
	}
	if s.geoMin != nil {
		opts.SetMin(*s.geoMin)
	}
	if s.geoMax != nil {
		opts.SetMax(*s.geoMax)
	}
	if s.geoBits != nil {
		opts.SetBits(*s.geoBits)
	}
	if s.geoBucketSize != nil {
		opts.SetBucketSize(*s.geoBucketSize)
	}
	return mongo.IndexModel{Keys: keys, Options: opts}
}

// encodeOrder turns the framework's IndexKeyOrder into the wire value
// Mongo expects inside the IndexModel.Keys bson document.
func encodeOrder(o IndexKeyOrder) any {
	switch o {
	case IndexOrderAsc:
		return int32(1)
	case IndexOrderDesc:
		return int32(-1)
	case IndexOrderText:
		return "text"
	case IndexOrderGeo2D:
		return "2d"
	case IndexOrderGeo2DSph:
		return "2dsphere"
	case IndexOrderHashed:
		return "hashed"
	}
	return int32(1)
}

// Exists is sugar for the partialFilterExpression {field: {$exists: v}}.
// Pair with IndexSpec.Partial to declare partial indexes that only cover
// documents where the named field is present (or absent).
//
//	Index("deleted_at").Partial(Exists("deleted_at", false))
func Exists(field string, exists bool) bson.M {
	return bson.M{field: bson.M{"$exists": exists}}
}

// CollationSpec mirrors options.Collation field-by-field so the framework
// public API stays driver-agnostic while the conversion to the native
// driver type is one-shot. Setting fields whose Mongo semantic is
// strength-dependent (CaseFirst, Backwards) is allowed; the framework
// does not gate cross-field combinations at boot — that is collation
// semantics, owned by Mongo.
type CollationSpec struct {
	Locale          string
	CaseLevel       bool
	CaseFirst       string
	Strength        int
	NumericOrdering bool
	Alternate       string
	MaxVariable     string
	Normalization   bool
	Backwards       bool
}

// DriverCollation converts the declarative spec to the driver-native
// *options.Collation. Returns nil on a nil receiver so call sites do not
// need a guard.
func (c *CollationSpec) DriverCollation() *options.Collation {
	if c == nil {
		return nil
	}
	return &options.Collation{
		Locale:          c.Locale,
		CaseLevel:       c.CaseLevel,
		CaseFirst:       c.CaseFirst,
		Strength:        c.Strength,
		NumericOrdering: c.NumericOrdering,
		Alternate:       c.Alternate,
		MaxVariable:     c.MaxVariable,
		Normalization:   c.Normalization,
		Backwards:       c.Backwards,
	}
}

// CappedSpec declares a capped collection. SizeBytes is mandatory and
// strictly positive (Mongo rejects a non-positive cap). MaxDocs is
// optional; when set, Mongo evicts on whichever bound triggers first.
type CappedSpec struct {
	SizeBytes int64
	MaxDocs   int64
}

// TimeSeriesSpec declares a time-series collection. TimeField is mandatory
// (the field carrying the per-document timestamp); MetaField is optional
// but recommended (cardinality knob — typically a sensor / source id);
// Granularity must be one of "seconds" | "minutes" | "hours".
type TimeSeriesSpec struct {
	TimeField   string
	MetaField   string
	Granularity string
}

// JSONSchemaValidationLevel and JSONSchemaValidationAction constants —
// kept as untyped strings to round-trip cleanly with Mongo's wire
// vocabulary; the boot validation enforces the closed set.
const (
	ValidationLevelStrict   = "strict"
	ValidationLevelModerate = "moderate"
	ValidationLevelOff      = "off"

	ValidationActionError = "error"
	ValidationActionWarn  = "warn"
)

// JSONSchemaSpec declares a $jsonSchema validator on the collection.
// Schema is the bson.M payload the framework forwards verbatim to Mongo
// under the {$jsonSchema: ...} key; ValidationLevel and ValidationAction
// default to "strict" / "error" when the declaration only carries
// .JSONSchema(schema) (the safe default — reject any non-conforming
// write).
type JSONSchemaSpec struct {
	Schema           bson.M
	ValidationLevel  string
	ValidationAction string
}

// Indexes registers one or more declarative index specs on the view.
// Accumulates across multiple calls so consumers can group declarations
// by intent (e.g. one Indexes(...) for lookup indexes, another for sort
// indexes) without losing earlier registrations.
func (v *ViewDefinition) Indexes(specs ...*IndexSpec) *ViewDefinition {
	v.mongoSpec.indexes = append(v.mongoSpec.indexes, specs...)
	return v
}

// JSONSchema declares a $jsonSchema validator on the collection. Calling
// this once installs the schema with the default validationLevel "strict"
// and validationAction "error"; the two are overridable via the methods
// below.
func (v *ViewDefinition) JSONSchema(schema bson.M) *ViewDefinition {
	if v.mongoSpec.jsonSchema == nil {
		v.mongoSpec.jsonSchema = &JSONSchemaSpec{
			ValidationLevel:  ValidationLevelStrict,
			ValidationAction: ValidationActionError,
		}
	}
	v.mongoSpec.jsonSchema.Schema = schema
	return v
}

// JSONSchemaValidationLevel overrides the default "strict" level.
// Accepts "strict" | "moderate" | "off"; rejected at boot otherwise.
func (v *ViewDefinition) JSONSchemaValidationLevel(level string) *ViewDefinition {
	if v.mongoSpec.jsonSchema == nil {
		v.mongoSpec.jsonSchema = &JSONSchemaSpec{ValidationAction: ValidationActionError}
	}
	v.mongoSpec.jsonSchema.ValidationLevel = level
	return v
}

// JSONSchemaValidationAction overrides the default "error" action.
// Accepts "error" | "warn"; rejected at boot otherwise.
func (v *ViewDefinition) JSONSchemaValidationAction(action string) *ViewDefinition {
	if v.mongoSpec.jsonSchema == nil {
		v.mongoSpec.jsonSchema = &JSONSchemaSpec{ValidationLevel: ValidationLevelStrict}
	}
	v.mongoSpec.jsonSchema.ValidationAction = action
	return v
}

// Collation declares the collection's default collation. Applied at
// createCollection time; Mongo treats the collection collation as
// immutable, so re-declaring a different collation on an existing
// collection aborts boot with a divergence diagnostic (Phase B).
func (v *ViewDefinition) Collation(c *CollationSpec) *ViewDefinition {
	v.mongoSpec.collation = c
	return v
}

// Capped declares the collection as capped. Mutually exclusive with
// TimeSeries — declaring both aborts boot via ValidateMongoSpec.
func (v *ViewDefinition) Capped(c *CappedSpec) *ViewDefinition {
	v.mongoSpec.capped = c
	return v
}

// TimeSeries declares the collection as a time-series collection.
// Mutually exclusive with Capped — declaring both aborts boot via
// ValidateMongoSpec.
func (v *ViewDefinition) TimeSeries(ts *TimeSeriesSpec) *ViewDefinition {
	v.mongoSpec.timeSeries = ts
	return v
}

// IndexSpecs returns the declared indexes — consumed by Phase B's
// ApplyMongoSpecs to feed Collection.Indexes().CreateMany, and by the
// boot guards in bootstrap/upstream_guards.go to check coverage on
// external JoinUpstream embed join fields (§8.1).
func (v *ViewDefinition) IndexSpecs() []*IndexSpec { return v.mongoSpec.indexes }

// KeyNames returns the field names of the spec's keys in declaration
// order. Used by the §8.1 boot guard to check "this index covers the
// joinField as its FIRST key" without exporting the IndexKey type or
// every field's Order. Empty slice for an index with no keys (should
// never happen — ValidateMongoSpec rejects it at boot — but stays safe).
func (s *IndexSpec) KeyNames() []string {
	if len(s.Keys) == 0 {
		return nil
	}
	out := make([]string, len(s.Keys))
	for i, k := range s.Keys {
		out[i] = k.Field
	}
	return out
}

// SchemaSpec returns the declared $jsonSchema validator or nil.
func (v *ViewDefinition) SchemaSpec() *JSONSchemaSpec { return v.mongoSpec.jsonSchema }

// CollectionCollation returns the declared collection-level collation or
// nil. Named distinctly from CollationSpec (the type) to avoid the
// "method named like its return type" reading.
func (v *ViewDefinition) CollectionCollation() *CollationSpec { return v.mongoSpec.collation }

// CappedDeclaration returns the declared CappedSpec or nil.
func (v *ViewDefinition) CappedDeclaration() *CappedSpec { return v.mongoSpec.capped }

// TimeSeriesDeclaration returns the declared TimeSeriesSpec or nil.
func (v *ViewDefinition) TimeSeriesDeclaration() *TimeSeriesSpec { return v.mongoSpec.timeSeries }

// ValidateMongoSpec runs the closed-set boot invariants on the declared
// spec. Phase B's ApplyMongoSpecs calls this before any round-trip to
// the cluster so a misconfigured view aborts boot loudly (with the view
// name in every diagnostic) rather than producing partial Mongo state.
//
// Enforced rules:
//
//  1. Capped and TimeSeries are mutually exclusive (Mongo rejects the
//     combination at createCollection).
//  2. Capped.SizeBytes must be strictly positive.
//  3. TimeSeries.TimeField is mandatory.
//  4. TimeSeries.Granularity (when set) must be one of "seconds" |
//     "minutes" | "hours".
//  5. At most one TextIndex per view (Mongo allows only one text index
//     per collection).
//  6. Every IndexSpec must declare at least one key.
//  7. JSONSchemaSpec.ValidationLevel ∈ {strict, moderate, off}.
//  8. JSONSchemaSpec.ValidationAction ∈ {error, warn}.
//  9. Every index key names a column the composer emits (no dead index on
//     a typo'd / undeclared field). Skipped for a schema-less view.
//  10. Every top-level $jsonSchema.required entry names a column the
//     composer emits (else, with validationAction=error, every upsert is
//     rejected and the projection silently stops updating).
func (v *ViewDefinition) ValidateMongoSpec() error {
	ms := &v.mongoSpec

	if v.version <= 0 {
		return fmt.Errorf("view %q: Version(N) is mandatory and must be > 0 (got %d) — declare via View(%q).Version(N).Schema(...) per tasks/mongo_schema_evolution_2.md §8", v.name, v.version, v.name)
	}

	if ms.capped != nil && ms.timeSeries != nil {
		return fmt.Errorf("view %q: Capped and TimeSeries are mutually exclusive", v.name)
	}
	if ms.capped != nil && ms.capped.SizeBytes <= 0 {
		return fmt.Errorf("view %q: CappedSpec.SizeBytes must be > 0 (got %d)", v.name, ms.capped.SizeBytes)
	}
	if ms.timeSeries != nil {
		if ms.timeSeries.TimeField == "" {
			return fmt.Errorf("view %q: TimeSeriesSpec.TimeField is required", v.name)
		}
		if ms.timeSeries.Granularity != "" {
			switch ms.timeSeries.Granularity {
			case "seconds", "minutes", "hours":
			default:
				return fmt.Errorf("view %q: TimeSeriesSpec.Granularity %q invalid (want seconds|minutes|hours)", v.name, ms.timeSeries.Granularity)
			}
		}
	}

	textCount := 0
	for i, s := range ms.indexes {
		if len(s.Keys) == 0 {
			return fmt.Errorf("view %q: index #%d has no keys", v.name, i)
		}
		if s.IsText() {
			textCount++
		}
	}
	if textCount > 1 {
		return fmt.Errorf("view %q: at most one TextIndex per collection (declared %d)", v.name, textCount)
	}

	if ms.jsonSchema != nil {
		switch ms.jsonSchema.ValidationLevel {
		case ValidationLevelStrict, ValidationLevelModerate, ValidationLevelOff, "":
		default:
			return fmt.Errorf("view %q: JSONSchemaSpec.ValidationLevel %q invalid (want strict|moderate|off)", v.name, ms.jsonSchema.ValidationLevel)
		}
		switch ms.jsonSchema.ValidationAction {
		case ValidationActionError, ValidationActionWarn, "":
		default:
			return fmt.Errorf("view %q: JSONSchemaSpec.ValidationAction %q invalid (want error|warn)", v.name, ms.jsonSchema.ValidationAction)
		}
	}

	// Shape guards (rules 9 + 10): an index key, or a top-level
	// $jsonSchema.required entry, must name a physical column the composer
	// actually emits. Only runs on a real registered view — schema present,
	// guaranteed by ValidateViewSchemas before ApplyMongoSpecs; the
	// schema-less views some unit tests build in isolation are skipped (no
	// column set to compare against).
	if v.schema != nil && (len(ms.indexes) > 0 || (ms.jsonSchema != nil && ms.jsonSchema.Schema != nil)) {
		cols := v.composedColumnSet()
		for i, s := range ms.indexes {
			for _, key := range s.KeyNames() {
				if _, ok := cols[key]; !ok {
					return fmt.Errorf("view %q: index #%d key %q is not a column the composer emits — "+
						"the index would never be used (typo, or a field not declared in the view's core.TableSchema). "+
						"Emitted columns: %s", v.name, i, key, sortedColumns(cols))
				}
			}
		}
		if ms.jsonSchema != nil && ms.jsonSchema.Schema != nil {
			if req, ok := ms.jsonSchema.Schema["required"]; ok {
				for _, field := range stringList(req) {
					if _, ok := cols[field]; !ok {
						return fmt.Errorf("view %q: $jsonSchema.required entry %q is not a column the composer emits — "+
							"with validationAction=error every SyncEngine upsert would be rejected and the projection would silently stop updating. "+
							"Emitted columns: %s", v.name, field, sortedColumns(cols))
					}
				}
			}
		}
	}

	return nil
}

// composedColumnSet returns the set of physical column PATHS (dotted for
// embeds) the composer can emit for this view: every root column + the
// columns of every embed subtree (prefixed by the embed's doc field), plus
// "_id" at the root. A field outside this set is one the composed Mongo
// document never carries — so an index on it is dead (never used) and a
// $jsonSchema.required on it (with the default error action) rejects every
// upsert.
func (v *ViewDefinition) composedColumnSet() map[string]struct{} {
	set := map[string]struct{}{"_id": {}}
	collectComposedColumns(v.schema, v.embeds, "", set)
	// SharedBaseView roles: each segment is addressable itself, and its
	// sub-document carries the role's flat columns + its own children —
	// mirroring composeBaseRootedRow (the base's flat columns and the
	// base-children live at the ROOT, never inside a segment).
	for _, r := range v.roles {
		set[r.segment] = struct{}{}
		collectRoleColumns(r.schema, r.segment, set)
	}
	return set
}

// collectComposedColumns walks a schema + its embeds, adding every physical
// column path into set. It mirrors what the composer actually emits (and what
// buildExportNode walks): the node's own columns, PLUS the FLAT columns of each
// sibling and of the SharedBase (mergeOwnerSiblings / mergeSharedBase merge
// them at this level), PLUS the base's native children nested under their
// derived segment (mergeSharedBaseChildren), PLUS external embeds. The embed's
// whole subtree is addressable at its doc field (e.g. "addresses"), and each
// nested column is prefixed by it (e.g. "addresses.zip_code"). ParentID + the three
// managed columns are included so a legitimately-indexed DeletedAt / ParentID
// column is not flagged.
func collectComposedColumns(schema *core.TableSchema, embeds []embedDef, prefix string, set map[string]struct{}) {
	if schema != nil {
		addSchemaFlatColumns(set, prefix, schema)
		// Siblings merge FLAT into this node (mergeOwnerSiblings) — their columns
		// are columns of this level.
		for _, sib := range schema.Siblings() {
			addSchemaFlatColumns(set, prefix, sib)
		}
		// The SharedBase merges FLAT too (mergeSharedBase) — every base column
		// (the base ID the merge skips is already covered by the role's ID).
		if base, _, ok := schema.SharedBaseRef(); ok {
			addSchemaFlatColumns(set, prefix, base)
		}
	}
	for _, e := range embeds {
		if e.leg == nil {
			continue
		}
		addComposedColumn(set, prefix, e.Field())
		embedPrefix := joinColumnPrefix(prefix, e.Field())
		collectComposedColumns(e.leg.schema, nil, embedPrefix, set)
		// A 1:N embed's join column lives on each embedded (leg) element, so it is a
		// queryable column of the embed segment (e.g. "addresses.user_id"). The 1:1
		// join column lives on the PARENT, emitted at the parent level if declared.
		if e.many {
			addComposedColumn(set, embedPrefix, e.joinCol)
		}
	}
	if schema != nil {
		// The SharedBase's native children (base-children) nest under their derived
		// segment (mergeSharedBaseChildren) — the same shape as an embed subtree.
		if base, _, ok := schema.SharedBaseRef(); ok {
			for _, bc := range base.ChildSchemas() {
				seg := childDocSegment(bc)
				addComposedColumn(set, prefix, seg)
				collectComposedColumns(bc, nil, joinColumnPrefix(prefix, seg), set)
			}
		}
		// The schema's OWN aggregate children nest under their derived segment too
		// (mergeOwnChildren) — without this walk a legitimate index on an
		// own-child path (e.g. "dependents.name") was falsely rejected.
		for _, child := range schema.ChildSchemas() {
			seg := childDocSegment(child)
			addComposedColumn(set, prefix, seg)
			collectComposedColumns(child, nil, joinColumnPrefix(prefix, seg), set)
		}
	}
}

// collectRoleColumns mirrors composeBaseRootedRow's role sub-document: the
// role's own flat columns plus its siblings' (merged flat), plus its own
// children nested under their derived segment. The base's flat columns and the
// base-children are deliberately NOT collected under the segment — the
// composer lands them at the person document's root only.
func collectRoleColumns(role *core.TableSchema, prefix string, set map[string]struct{}) {
	addSchemaFlatColumns(set, prefix, role)
	for _, sib := range role.Siblings() {
		addSchemaFlatColumns(set, prefix, sib)
	}
	for _, child := range role.ChildSchemas() {
		seg := childDocSegment(child)
		addComposedColumn(set, prefix, seg)
		collectComposedColumns(child, nil, joinColumnPrefix(prefix, seg), set)
	}
}

// addComposedColumn adds one (possibly prefixed) physical column path.
func addComposedColumn(set map[string]struct{}, prefix, col string) {
	if col == "" {
		return
	}
	set[joinColumnPrefix(prefix, col)] = struct{}{}
}

// addSchemaFlatColumns adds a schema's flat physical columns (ID, ParentID, managed,
// business) at the given prefix.
func addSchemaFlatColumns(set map[string]struct{}, prefix string, s *core.TableSchema) {
	addComposedColumn(set, prefix, s.IDColumn())
	addComposedColumn(set, prefix, s.ParentIDColumn())
	sd, _ := s.DeletedAtColumn()
	addComposedColumn(set, prefix, sd)
	addComposedColumn(set, prefix, s.CreatedAtColumn())
	addComposedColumn(set, prefix, s.UpdatedAtColumn())
	for _, col := range s.MappedColumns() {
		addComposedColumn(set, prefix, col)
	}
}

func joinColumnPrefix(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

// stringList normalizes a $jsonSchema "required" value (typically []string
// or bson.A) into a []string, dropping non-string entries.
func stringList(v any) []string {
	items, ok := asAnySlice(v)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sortedColumns renders the emitted-column set as a sorted comma list for
// the diagnostic so the operator sees exactly what the view does carry.
func sortedColumns(set map[string]struct{}) string {
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
