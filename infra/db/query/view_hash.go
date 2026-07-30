package query

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"math"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RebuildHash returns the SHA-256 of the declarative fields whose change
// requires recomposing every document in the collection: root table, embed
// shape (recursive), DeleteOnArchive flag, $jsonSchema validator, default
// collation, capped declaration, and time-series declaration. Index changes
// are NOT in this hash — see ArtifactHash.
//
// Deterministic across runs and across processes: the same ViewDefinition
// (regardless of declaration order on a map, regardless of pointer identity)
// hashes to the same 64-char hex value.
func (v *ViewDefinition) RebuildHash() string {
	w := newCanonicalWriter()
	v.writeRebuildShape(w)
	return w.hexDigest()
}

// ArtifactHash returns the SHA-256 of the declared indexes — the artifacts
// the framework drops and recreates on divergence without touching document
// shape. Independent of RebuildHash so an index-only change can take the
// fast path (re-apply indexes, update marker, no rebuild).
func (v *ViewDefinition) ArtifactHash() string {
	w := newCanonicalWriter()
	v.writeArtifactShape(w)
	return w.hexDigest()
}

// Hash is the combined identity of the view's declarative state: the
// concatenation of RebuildHash and ArtifactHash, hashed again. Stamped on
// the per-collection marker doc and used at boot as the primary drift key.
func (v *ViewDefinition) Hash() string {
	r := v.RebuildHash()
	a := v.ArtifactHash()
	w := newCanonicalWriter()
	w.writeString(r)
	w.writeString(a)
	return w.hexDigest()
}

// writeRebuildShape walks the rebuild-relevant fields in a fixed order so
// the byte stream feeding the hash is deterministic. The order itself is
// part of the canonical form: changing it changes the hash on every view
// even if no semantic field changed. Pick once, never touch.
//
// Tag "rebuild_v2" — bumped from "rebuild_v1" when the framework added the
// mandatory Version(N) declaration. Version is the first field of the
// partition because changing it is what most often drives a rebuild, and
// putting it first surfaces the change cleanly in any hash-diff trace.
func (v *ViewDefinition) writeRebuildShape(w *canonicalWriter) {
	w.writeTag("rebuild_v2")
	w.writeInt(int64(v.version))
	w.writeString(v.RootTable())
	w.writeBool(v.deleteOnArchive)

	w.writeTag("embeds")
	writeEmbedList(w, v.embeds)

	// The aggregate's PROJECTED shape — the physical columns the composer
	// materializes from the write TableSchema (root + siblings + SharedBase +
	// children). Internal data auto-projects (no embed), so without this a schema
	// change (new column / .Child / sibling field) would leave the RebuildHash
	// unchanged and the forgot-to-bump guard would never fire — the projection
	// would silently go stale.
	w.writeTag("root_schema")
	writeSchemaShape(w, v.schema)

	// SharedBaseView roles — the role set IS projection shape: adding/removing
	// a role, renaming its segment, changing its ParentID column or any part of its
	// schema closure (fields, siblings, children — writeSchemaShape recurses,
	// its shared_base branch included) must move the RebuildHash so the
	// forgot-to-bump guard fires. Sorted by segment (order-independent).
	// Emitted ONLY for a SharedBaseView: a regular view's canonical byte
	// stream stays byte-identical to rebuild_v2, so upgrading the framework
	// moves no existing view's hash.
	if v.isSharedBaseView {
		w.writeTag("roles")
		roles := append([]roleDef(nil), v.roles...)
		sort.Slice(roles, func(i, j int) bool { return roles[i].segment < roles[j].segment })
		w.writeInt(int64(len(roles)))
		for _, r := range roles {
			w.writeString(r.segment)
			_, fk, _ := r.schema.SharedBaseRef()
			w.writeString(fk)
			writeSchemaShape(w, r.schema)
		}
	}

	// EmbedInChild enrichments are projection shape: the enriched field, the
	// child it lands in, the source collection and the element ParentID all change the
	// materialized document, so a change without a Version bump must move the
	// RebuildHash. Emitted ONLY when the view declares child-embeds, so a view
	// without them keeps its byte-identical rebuild_v2 stream (no existing view's
	// hash moves on upgrade). Sorted by (childSegment, field) for order-independence.
	if len(v.childEmbeds) > 0 {
		w.writeTag("child_embeds")
		ces := append([]childEmbedDef(nil), v.childEmbeds...)
		sort.Slice(ces, func(i, j int) bool {
			return ces[i].ChildSegment()+"\x00"+ces[i].Field() < ces[j].ChildSegment()+"\x00"+ces[j].Field()
		})
		w.writeInt(int64(len(ces)))
		for _, ce := range ces {
			w.writeString(ce.childSchema.Table())
			w.writeString(ce.Field())
			w.writeString(ce.leg.Table())
			w.writeString(ce.joinCol)
			w.writeString(ce.leg.goSegment)
			// Same version coupling as a root view leg (see writeEmbedList):
			// conditional, so an external-sourced EmbedInChild keeps its stream.
			if ce.leg.view != nil {
				w.writeTag("leg_view")
				w.writeInt(int64(ce.leg.view.VersionNumber()))
			}
		}
	}

	w.writeTag("schema")
	writeJSONSchema(w, v.mongoSpec.jsonSchema)

	w.writeTag("collation")
	writeCollation(w, v.mongoSpec.collation)

	w.writeTag("capped")
	writeCapped(w, v.mongoSpec.capped)

	w.writeTag("timeseries")
	writeTimeSeries(w, v.mongoSpec.timeSeries)
}

// writeArtifactShape walks every declared index in declaration order. The
// order matters for the hash — semantically the framework treats two views
// declaring the same indexes in different order as the same shape, but the
// canonical form sorts the index list by deterministic key before writing,
// so order-of-call on Indexes(...) does not change the hash.
func (v *ViewDefinition) writeArtifactShape(w *canonicalWriter) {
	w.writeTag("artifact_v1")
	specs := append([]*IndexSpec(nil), v.mongoSpec.indexes...)
	sort.SliceStable(specs, func(i, j int) bool {
		return indexCanonKey(specs[i]) < indexCanonKey(specs[j])
	})
	w.writeInt(int64(len(specs)))
	for _, s := range specs {
		writeIndexSpec(w, s)
	}
}

// indexCanonKey produces a deterministic comparison string for sorting the
// index list — explicit Name() wins (a renamed index is semantically a
// different artifact), otherwise the concatenation of "field:order" pairs
// gives the natural ordering.
func indexCanonKey(s *IndexSpec) string {
	if s.name != "" {
		return "n:" + s.name
	}
	parts := make([]string, 0, len(s.Keys))
	for _, k := range s.Keys {
		parts = append(parts, k.Field+":"+string(k.Order))
	}
	return "k:" + strings.Join(parts, "|")
}

func writeEmbedList(w *canonicalWriter, embeds []embedDef) {
	w.writeInt(int64(len(embeds)))
	for _, e := range embeds {
		if e.leg == nil {
			w.writeString("")
			w.writeBool(e.many)
			w.writeTag("nil_source")
			continue
		}
		w.writeString(e.Field())
		w.writeBool(e.many)
		w.writeTag("source")
		w.writeString(e.leg.Collection())
		// The join column named via .On — one-to-many (leg ParentID) or one-to-one
		// (parent ParentID) alike — so an ParentID change still moves the rebuild hash.
		w.writeString(e.JoinColumn())
		w.writeBool(e.leg.IsMongo())
		// A JoinView leg couples this view's rebuild identity to the EMBEDDED
		// view's declared Version: bumping the source view moves this hash too, so
		// the forgot-to-bump guard fires here and the embedding projection is
		// rebuilt against the new shape instead of silently keeping copies of the
		// old one. Emitted ONLY for a view leg, so every existing (external-leg)
		// view keeps its byte-identical rebuild_v2 stream.
		if e.leg.view != nil {
			w.writeTag("leg_view")
			w.writeInt(int64(e.leg.view.VersionNumber()))
		}
		// A declared 1:N element order is materialized INTO the document, so it
		// is projection shape: changing it must move the rebuild hash. Emitted
		// only when declared, so an unordered EmbedMany keeps its byte-identical
		// stream and no deployed view rebuilds on upgrade.
		if e.orderBy != "" {
			w.writeTag("order")
			w.writeString(e.orderBy)
			w.writeBool(e.orderDesc)
		}
		// Embeds are single-level: a source carries no nested embeds. Emit the
		// empty-list encoding (length 0) that the old nested writeEmbedList call
		// produced for every real (non-nested) view, so the RebuildHash stays
		// byte-identical across this refactor — no spurious drift/rebuild.
		w.writeInt(0)
	}
}

// writeSchemaShape hashes one schema node's PROJECTED shape and recurses into the
// closure the composer materializes: the node's own physical columns (ID +
// business + managed), then its siblings and SharedBase (merged FLAT) and its
// children (nested). Column-granular, not Go-field: a rename that keeps the same
// column does not force a rebuild; adding a column / .Child / sibling field does.
// Order-independent within a node — columns, siblings and children are sorted, so
// reordering declarations (which does not change the document) leaves the hash
// stable. Recursion is bounded: a child is a leaf (no grandchildren, no SharedBase
// role), and a shared base carries only its native children.
func writeSchemaShape(w *canonicalWriter, s *core.TableSchema) {
	if s == nil {
		w.writeTag("nil_schema")
		return
	}
	w.writeTag("schema_node")
	w.writeString(s.Table())
	w.writeString(s.IDColumn())
	// Managed columns land in the projected document too.
	sd, _ := s.DeletedAtColumn()
	w.writeString(sd)
	writeSortedStrings(w, s.InsertNowColumns()) // created_at, updated_at (when enabled)
	// Business columns — physical, sorted (order-independent).
	writeSortedStrings(w, physicalColumns(s))

	// Siblings (FLAT), sorted by table for determinism.
	sibs := append([]*core.TableSchema(nil), s.Siblings()...)
	sort.Slice(sibs, func(i, j int) bool { return sibs[i].Table() < sibs[j].Table() })
	w.writeTag("siblings")
	w.writeInt(int64(len(sibs)))
	for _, sib := range sibs {
		writeSchemaShape(w, sib)
	}

	// SharedBase (FLAT): the role ParentID + the base closure (incl. its base-children).
	if base, fk, ok := s.SharedBaseRef(); ok {
		w.writeTag("shared_base")
		w.writeString(fk)
		w.writeString(base.NaturalIDColumn())
		writeSchemaShape(w, base)
	} else {
		w.writeTag("no_shared_base")
	}

	// Own children (nested), already table-sorted by ChildSchemas().
	children := s.ChildSchemas()
	w.writeTag("children")
	w.writeInt(int64(len(children)))
	for _, child := range children {
		w.writeString(child.ParentIDColumn())
		writeSchemaShape(w, child)
	}
}

// physicalColumns returns a schema's Field-declared business columns (physical
// names), used to hash the projected document shape.
func physicalColumns(s *core.TableSchema) []string {
	gfs := s.GoFields()
	cols := make([]string, 0, len(gfs))
	for _, gf := range gfs {
		if c, ok := s.ColumnOf(gf); ok {
			cols = append(cols, c)
		}
	}
	return cols
}

// writeSortedStrings hashes a string slice order-independently.
func writeSortedStrings(w *canonicalWriter, ss []string) {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	w.writeInt(int64(len(out)))
	for _, s := range out {
		w.writeString(s)
	}
}

func writeJSONSchema(w *canonicalWriter, s *JSONSchemaSpec) {
	if s == nil {
		w.writeTag("nil")
		return
	}
	w.writeTag("present")
	// ValidationLevel and ValidationAction normalize empty → defaults so
	// declaring JSONSchema(schema) without explicit level/action produces
	// the same hash as declaring them explicitly with the defaults. Matches
	// the apply-step's resolution at JSONSchema()/JSONSchemaValidationLevel()
	// in view_mongo_spec.go.
	level := s.ValidationLevel
	if level == "" {
		level = ValidationLevelStrict
	}
	action := s.ValidationAction
	if action == "" {
		action = ValidationActionError
	}
	w.writeString(level)
	w.writeString(action)
	writeBSONValue(w, s.Schema)
}

func writeCollation(w *canonicalWriter, c *CollationSpec) {
	if c == nil {
		w.writeTag("nil")
		return
	}
	w.writeTag("present")
	w.writeString(c.Locale)
	w.writeBool(c.CaseLevel)
	w.writeString(c.CaseFirst)
	w.writeInt(int64(c.Strength))
	w.writeBool(c.NumericOrdering)
	w.writeString(c.Alternate)
	w.writeString(c.MaxVariable)
	w.writeBool(c.Normalization)
	w.writeBool(c.Backwards)
}

func writeCapped(w *canonicalWriter, c *CappedSpec) {
	if c == nil {
		w.writeTag("nil")
		return
	}
	w.writeTag("present")
	w.writeInt(c.SizeBytes)
	w.writeInt(c.MaxDocs)
}

func writeTimeSeries(w *canonicalWriter, t *TimeSeriesSpec) {
	if t == nil {
		w.writeTag("nil")
		return
	}
	w.writeTag("present")
	w.writeString(t.TimeField)
	w.writeString(t.MetaField)
	// Granularity normalized to lowercase so "Seconds" and "seconds" hash
	// equal — they refer to the same Mongo wire value.
	w.writeString(strings.ToLower(t.Granularity))
}

func writeIndexSpec(w *canonicalWriter, s *IndexSpec) {
	w.writeTag("index")
	w.writeString(s.name)
	w.writeBool(s.unique)
	w.writeBool(s.sparse)
	w.writeBool(s.hidden)

	w.writeInt(int64(len(s.Keys)))
	for _, k := range s.Keys {
		w.writeString(k.Field)
		w.writeString(string(k.Order))
	}

	if s.partialFilter == nil {
		w.writeTag("nil_pf")
	} else {
		w.writeTag("pf")
		writeBSONValue(w, s.partialFilter)
	}

	if s.expireAfterSeconds == nil {
		w.writeTag("nil_ttl")
	} else {
		w.writeTag("ttl")
		w.writeInt(int64(*s.expireAfterSeconds))
	}

	writeCollation(w, s.collation)

	// Weights: deterministic order via sorted keys.
	if len(s.weights) == 0 {
		w.writeTag("nil_w")
	} else {
		w.writeTag("w")
		keys := make([]string, 0, len(s.weights))
		for k := range s.weights {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.writeInt(int64(len(keys)))
		for _, k := range keys {
			w.writeString(k)
			w.writeInt(int64(s.weights[k]))
		}
	}

	w.writeString(s.defaultLanguage)
	w.writeString(s.languageOverride)

	writeFloatPtr(w, s.geoMin)
	writeFloatPtr(w, s.geoMax)
	writeInt32Ptr(w, s.geoBits)
	writeInt32Ptr(w, s.geoBucketSize)
}

func writeFloatPtr(w *canonicalWriter, p *float64) {
	if p == nil {
		w.writeTag("nil")
		return
	}
	w.writeTag("v")
	w.writeFloat(*p)
}

func writeInt32Ptr(w *canonicalWriter, p *int32) {
	if p == nil {
		w.writeTag("nil")
		return
	}
	w.writeTag("v")
	w.writeInt(int64(*p))
}

// writeBSONValue walks an arbitrary bson value (bson.M, bson.D, bson.A,
// scalars) and writes it deterministically. Map keys sorted alphabetically;
// bson.D preserved in declaration order (the type itself encodes order);
// arrays preserved in index order.
//
// Numeric normalization: every int-category lands as int64; every
// float-category as float64. So `42` (int) and `int32(42)` hash equal —
// same Mongo wire value.
func writeBSONValue(w *canonicalWriter, v any) {
	switch x := v.(type) {
	case nil:
		w.writeTag("nil")
	case bool:
		w.writeTag("b")
		w.writeBool(x)
	case string:
		w.writeTag("s")
		w.writeString(x)
	case int:
		w.writeTag("i")
		w.writeInt(int64(x))
	case int32:
		w.writeTag("i")
		w.writeInt(int64(x))
	case int64:
		w.writeTag("i")
		w.writeInt(x)
	case float32:
		w.writeTag("f")
		w.writeFloat(float64(x))
	case float64:
		w.writeTag("f")
		w.writeFloat(x)
	case bson.M:
		writeSortedMap(w, x)
	case map[string]any:
		writeSortedMap(w, x)
	case bson.D:
		w.writeTag("d")
		w.writeInt(int64(len(x)))
		for _, e := range x {
			w.writeString(e.Key)
			writeBSONValue(w, e.Value)
		}
	case bson.A:
		w.writeTag("a")
		w.writeInt(int64(len(x)))
		for _, item := range x {
			writeBSONValue(w, item)
		}
	case []any:
		w.writeTag("a")
		w.writeInt(int64(len(x)))
		for _, item := range x {
			writeBSONValue(w, item)
		}
	case []string:
		w.writeTag("ss")
		w.writeInt(int64(len(x)))
		for _, s := range x {
			w.writeString(s)
		}
	default:
		// Stringified fallback for value types the framework does not
		// expect inside a declarative spec. Stable across runs (fmt
		// output of the same value matches) but not order-invariant
		// across struct types — by design: a custom type leaking into
		// a spec means a consumer is using something the canonical
		// surface does not model, and the hash should change loudly.
		w.writeTag("?")
		w.writeString(fmt.Sprintf("%T:%v", v, v))
	}
}

func writeSortedMap(w *canonicalWriter, m map[string]any) {
	w.writeTag("m")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.writeInt(int64(len(keys)))
	for _, k := range keys {
		w.writeString(k)
		writeBSONValue(w, m[k])
	}
}

// canonicalWriter buffers bytes into a SHA-256 hash. All write* methods
// emit a length prefix before variable-length data so adjacent fields
// cannot collide via concatenation.
type canonicalWriter struct {
	h hash.Hash
}

func newCanonicalWriter() *canonicalWriter {
	return &canonicalWriter{h: sha256.New()}
}

func (w *canonicalWriter) writeTag(tag string) {
	w.h.Write([]byte{byte(len(tag))})
	w.h.Write([]byte(tag))
}

func (w *canonicalWriter) writeString(s string) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s)))
	w.h.Write(lenBuf[:])
	w.h.Write([]byte(s))
}

func (w *canonicalWriter) writeInt(i int64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(i))
	w.h.Write(buf[:])
}

func (w *canonicalWriter) writeFloat(f float64) {
	var buf [8]byte
	// math.Float64bits gives IEEE 754 bit pattern — deterministic across
	// platforms, matches the Mongo wire form.
	binary.BigEndian.PutUint64(buf[:], math.Float64bits(f))
	w.h.Write(buf[:])
}

func (w *canonicalWriter) writeBool(b bool) {
	if b {
		w.h.Write([]byte{1})
	} else {
		w.h.Write([]byte{0})
	}
}

func (w *canonicalWriter) hexDigest() string {
	return hex.EncodeToString(w.h.Sum(nil))
}
