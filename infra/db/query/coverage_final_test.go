package query

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Coverage mop-up for the infra root package: the remaining REACHABLE branches
// of the Mongo read-side (composer, view, view_schema, view_hash, mongo_spec,
// mongo_drift formatters) that run without a live database. The composer reads
// through the engine seam — c.eng.Querier().QueryMaps — so its tests script a
// fakeEngine. White-box tests of code that moved to packages db / db/pg (the
// write binding, the criteria translator, the aggregate persister) live in those
// packages now.

// ─── exception.go: core.NewInfrastructureError + Error() ──────────────────────────

func TestNewInfrastructureError_AndError(t *testing.T) {
	ctx := domain.NewNotificationContext("Test")
	err := core.NewInfrastructureError([]*domain.NotificationContext{ctx})
	if err == nil || len(err.Contexts) != 1 {
		t.Fatalf("expected 1-context error, got %+v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "1 context") {
		t.Errorf("Error() = %q, want it to name the context count", msg)
	}
	// Zero-context shape still renders.
	if msg := core.NewInfrastructureError(nil).Error(); !strings.Contains(msg, "0 context") {
		t.Errorf("Error() on empty = %q", msg)
	}
}

// ─── view_schema.go: pure translator helpers ─────────────────────────────────

func TestViewNode_SchemaLessFallbacks(t *testing.T) {
	// A node with no schema (the defensive empty node) passes paths/docs through
	// and yields no soft-delete gate.
	n := &ViewNode{}
	if _, ok := n.SoftDeleteColumn(); ok {
		t.Error("schema-less node must report no soft-delete")
	}
	doc := map[string]any{"anything": 1}
	if got := n.ToGoDoc(doc); len(got) != 1 || got["anything"] != 1 {
		t.Errorf("schema-less ToGoDoc must pass through, got %v", got)
	}
	if path, ok := n.ColumnPath([]string{"X"}); !ok || path[0] != "X" {
		t.Errorf("schema-less ColumnPath must pass through, got %v,%v", path, ok)
	}
	// ToGoDoc on a nil doc returns nil.
	if got := n.ToGoDoc(nil); got != nil {
		t.Errorf("nil doc must return nil, got %v", got)
	}
}

func TestViewNode_ColumnPathEdges(t *testing.T) {
	var nilNode *ViewNode
	if _, ok := nilNode.ColumnPath([]string{"X"}); ok {
		t.Error("nil node must not resolve")
	}
	rootSchema := core.NewTableSchema[vsRoot]("people").PK("person_pk").Field("Email", "mail")
	childSchema := core.NewExternalSchema("tags").PK("tag_pk").FK("person_ref").Field("ZipCode", "zip")
	v := View("people").Schema(rootSchema).
		EmbedMany("addresses", FromSchema(childSchema).As("Addresses"))
	node := v.BuildViewNode()

	if _, ok := node.ColumnPath(nil); ok {
		t.Error("empty path must not resolve")
	}
	if _, ok := node.ColumnPath([]string{"Unknown"}); ok {
		t.Error("unknown root field must not resolve")
	}
	if _, ok := node.ColumnPath([]string{"NoSuchEmbed", "ZipCode"}); ok {
		t.Error("unknown embed segment must not resolve")
	}
	if _, ok := node.ColumnPath([]string{"Addresses", "Unknown"}); ok {
		t.Error("unknown nested field must not resolve")
	}
}

func TestViewNode_ToGoDoc_OneToOneEmbedAndDrop(t *testing.T) {
	rootSchema := core.NewTableSchema[vsRoot]("people").PK("person_pk").Field("Email", "mail")
	buyerSchema := core.NewExternalSchema("buyers").PK("b_pk").Field("Email", "b_mail")
	v := View("people").Schema(rootSchema).
		Embed("buyer", FromSchema(buyerSchema).FK("buyer_ref").As("Buyer"))
	node := v.BuildViewNode()

	doc := map[string]any{
		"person_pk": "p1",
		"mail":      "a@x",
		"unmapped":  "dropped", // not in schema → dropped
		"buyer":     map[string]any{"b_pk": "b1", "b_mail": "b@x"},
	}
	got := node.ToGoDoc(doc)
	if got["Email"] != "a@x" || got["ID"] != "p1" {
		t.Errorf("root read-back drifted: %v", got)
	}
	if _, present := got["unmapped"]; present {
		t.Error("unmapped column must be dropped")
	}
	buyer, ok := got["Buyer"].(map[string]any)
	if !ok || buyer["Email"] != "b@x" {
		t.Errorf("one-to-one embed read-back drifted: %v", got["Buyer"])
	}
}

func TestTranslateValue_MapAndPassthrough(t *testing.T) {
	childSchema := core.NewExternalSchema("tags").PK("tag_pk").FK("ref").Field("ZipCode", "zip")
	node := newViewNode(childSchema, nil)
	e := &viewEmbed{goSegment: "Addresses", docField: "addresses", node: node}

	// A single map (one-to-one shape) translates recursively.
	single := e.translateValue(map[string]any{"tag_pk": "t1", "zip": "10001"})
	m, ok := single.(map[string]any)
	if !ok || m["ZipCode"] != "10001" || m["ID"] != "t1" {
		t.Errorf("single-map translate drifted: %v", single)
	}
	// A scalar that is neither slice nor map passes through unchanged.
	if got := e.translateValue(42); got != 42 {
		t.Errorf("scalar passthrough = %v, want 42", got)
	}
	// A slice with a non-map element keeps that element verbatim.
	mixed := e.translateValue([]any{"raw", map[string]any{"tag_pk": "t2", "zip": "9"}})
	out, ok := mixed.([]any)
	if !ok || out[0] != "raw" {
		t.Errorf("mixed slice translate drifted: %v", mixed)
	}
}

func TestAsStringMap_AsAnySlice_ReflectAndNegatives(t *testing.T) {
	// asStringMap: reflect path over a non-map[string]any string-keyed map.
	if m, ok := asStringMap(map[string]int{"a": 1}); !ok || m["a"] != 1 {
		t.Errorf("reflect map normalize failed: %v,%v", m, ok)
	}
	// asStringMap: non-map returns false.
	if _, ok := asStringMap(7); ok {
		t.Error("non-map must not normalize to string map")
	}
	// asStringMap: map with non-string key returns false.
	if _, ok := asStringMap(map[int]string{1: "x"}); ok {
		t.Error("int-keyed map must not normalize")
	}
	// asAnySlice: reflect path over a typed slice.
	if s, ok := asAnySlice([]int{1, 2}); !ok || len(s) != 2 {
		t.Errorf("reflect slice normalize failed: %v,%v", s, ok)
	}
	// asAnySlice: non-slice returns false.
	if _, ok := asAnySlice("not-a-slice"); ok {
		t.Error("non-slice must not normalize")
	}
}

func TestNewViewNode_NilSourceAndSegmentFallback(t *testing.T) {
	// An embed with a nil source is skipped.
	n := newViewNode(builderTestSchema, []embedDef{{field: "skipme", source: nil}})
	if _, ok := n.embeds["skipme"]; ok {
		t.Error("nil-source embed must be skipped")
	}
	// An external embed with no .As falls back to the doc field as the segment.
	ext := FromSchema(core.NewExternalSchema("u").PK("id"))
	n2 := newViewNode(builderTestSchema, []embedDef{{field: "buyer", source: ext}})
	if _, ok := n2.embeds["buyer"]; !ok {
		t.Errorf("segment fallback to doc field expected, embeds=%v", n2.embeds)
	}
}

// ─── view.go: resolveGoSegment, DependentMongoViews, index ───────────────────

func TestResolveGoSegment_ExternalNoAsIsEmpty(t *testing.T) {
	ext := FromSchema(core.NewExternalSchema("users").PK("id"))
	e := embedDef{field: "buyer", source: ext, many: false}
	if seg := resolveGoSegment(e); seg != "" {
		t.Errorf("external embed without .As must resolve to empty segment, got %q", seg)
	}
	// nil source → empty.
	if seg := resolveGoSegment(embedDef{field: "x", source: nil}); seg != "" {
		t.Errorf("nil source must resolve to empty segment, got %q", seg)
	}
}

func TestValidateViewSchemas_ExternalEmbedMissingAs(t *testing.T) {
	src := FromSchema(core.NewExternalSchema("users").PK("id").FK("user_id")) // no .As
	v := View("orders").Version(1).
		Schema(rootSchema("orders")).
		EmbedMany("buyers", src)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no Go segment") {
		t.Fatalf("expected external-embed-missing-As error, got %v", err)
	}
}

func TestDependentMongoViews_Match(t *testing.T) {
	// A view embedding an external (Mongo) collection at the top level must be
	// reported by DependentMongoViews / viewEmbedsMongoCollection.
	buyer := FromSchema(core.NewExternalSchema("users").PK("id")).As("Buyer").FK("buyer_id")
	v := View("orders").Version(1).Schema(rootSchema("orders")).
		Embed("buyer", buyer)

	dep := DependentMongoViews([]*ViewDefinition{v}, "users")
	if len(dep) != 1 {
		t.Fatalf("expected 1 dependent view for the mongo embed, got %d", len(dep))
	}
	// A non-matching collection name yields nothing.
	if got := DependentMongoViews([]*ViewDefinition{v}, "nope"); len(got) != 0 {
		t.Errorf("unexpected dependents for unknown collection: %d", len(got))
	}
}

func TestBuildViewIndex_PGAndMongo(t *testing.T) {
	mongoSrc := FromSchema(core.NewExternalSchema("users").PK("id").FK("order_id")).As("Buyers")
	pgSrc := FromSchema(core.NewTableSchema[embedFixture]("lines").PK("id").FK("order_id"))
	v := View("orders").Version(1).Schema(rootSchema("orders")).
		EmbedMany("lines", pgSrc).
		EmbedMany("buyers", mongoSrc)

	idx := buildViewIndex([]*ViewDefinition{v})
	if len(idx.byPGTable["orders"]) != 1 {
		t.Errorf("root table must index into byPGTable")
	}
	if len(idx.byPGTable["lines"]) != 1 {
		t.Errorf("pg embed must index into byPGTable")
	}
	if len(idx.byMongoColl["users"]) != 1 {
		t.Errorf("mongo embed must index into byMongoColl")
	}
}

// ─── view_hash.go: indexCanonKey both branches ───────────────────────────────

func TestIndexCanonKey_NamedAndKeyed(t *testing.T) {
	named := Index("email").Name("custom_idx")
	if key := indexCanonKey(named); key != "n:custom_idx" {
		t.Errorf("named index key = %q, want n:custom_idx", key)
	}
	keyed := Compound("email", "created_at")
	got := indexCanonKey(keyed)
	if !strings.HasPrefix(got, "k:") || !strings.Contains(got, "email") {
		t.Errorf("keyed index key = %q, want k:...email...", got)
	}
}

// The relational embed path (fetchPGEmbed) was removed when embeds were narrowed
// to external sources only; its nil-key / missing-FK / child-error edge tests are
// gone with it. The external (Mongo) embed error + skip paths are covered in
// composer_mongo_test.go (TestFetchMongoEmbed_*).

// ─── view_schema.go: ToGoDoc _id passthrough ─────────────────────────────────

func TestToGoDoc_IDPassthrough(t *testing.T) {
	rootSchema := core.NewTableSchema[vsRoot]("people").PK("person_pk").Field("Email", "mail")
	node := newViewNode(rootSchema, nil)
	got := node.ToGoDoc(map[string]any{"_id": "doc-1", "mail": "a@x"})
	if got["_id"] != "doc-1" {
		t.Errorf("_id must pass through untouched, got %v", got["_id"])
	}
	if got["Email"] != "a@x" {
		t.Errorf("mail must translate to Email, got %v", got["Email"])
	}
}

// ─── view.go: nil-source branches + nested indexEmbeds ───────────────────────

func TestAppendEmbedSchemaProblems_NilSourceSkipped(t *testing.T) {
	// A nil-source embed is skipped without producing a problem.
	got := appendEmbedSchemaProblems(nil, "v", []embedDef{{field: "x", source: nil}})
	if len(got) != 0 {
		t.Errorf("nil-source embed must be skipped, got %v", got)
	}
}

func TestViewEmbedsMongoCollection_NilSourceSkipped(t *testing.T) {
	if viewEmbedsMongoCollection([]embedDef{{field: "x", source: nil}}, "users") {
		t.Error("nil-source embed must not match any collection")
	}
}

func TestValidateViewSchemas_NoRootSchema(t *testing.T) {
	v := View("orphan").Version(1) // no .Schema(...)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no root .Schema") {
		t.Fatalf("expected no-root-schema error, got %v", err)
	}
}

func TestBuildViewIndex_EmbedTableIndexed(t *testing.T) {
	// A top-level embed's source table is indexed by buildViewIndex so an
	// upstream change on it resolves back to the dependent view.
	lines := FromSchema(core.NewTableSchema[embedFixture]("lines").PK("id").FK("order_id"))
	v := View("orders").Version(1).Schema(rootSchema("orders")).
		EmbedMany("lines", lines)

	idx := buildViewIndex([]*ViewDefinition{v})
	if len(idx.byPGTable["lines"]) != 1 {
		t.Errorf("embed source table must be indexed, got %v", idx.byPGTable["lines"])
	}
}

// ─── view_hash.go: writeEmbedList nil-source + writeJSONSchema defaults ───────

func TestWriteEmbedList_NilSource(t *testing.T) {
	w := newCanonicalWriter()
	// Must not panic on a nil-source embed; the nil_source tag path runs.
	writeEmbedList(w, []embedDef{{field: "x", many: true, source: nil}})
	if w.hexDigest() == "" {
		t.Error("expected a non-empty digest")
	}
}

func TestWriteJSONSchema_DefaultsAndNil(t *testing.T) {
	// nil spec → "nil" tag, no panic.
	wNil := newCanonicalWriter()
	writeJSONSchema(wNil, nil)

	// Spec with empty level/action → both default branches run.
	wDef := newCanonicalWriter()
	writeJSONSchema(wDef, &JSONSchemaSpec{Schema: bson.M{"bsonType": "object"}})

	// Spec with explicit defaults must hash identically to the empty-defaults one.
	wExpl := newCanonicalWriter()
	writeJSONSchema(wExpl, &JSONSchemaSpec{
		Schema:           bson.M{"bsonType": "object"},
		ValidationLevel:  ValidationLevelStrict,
		ValidationAction: ValidationActionError,
	})
	if wDef.hexDigest() != wExpl.hexDigest() {
		t.Error("empty level/action must normalize to the explicit defaults (same hash)")
	}
}

// ─── composer.go: ComposeAll embed error ─────────────────────────────────────

func TestComposeAll_EmbedChildError(t *testing.T) {
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "a"}}), nil
	})
	// External embed whose Mongo fetch fails → applyEmbeds error → ComposeAll error.
	c := NewComposerWithMongo(eng, newFakeMongo(&fakeColl{findErr: context.Canceled}), identityResolver)
	view := View("orders").Version(1).Schema(composerRootSchema()).
		EmbedMany("buyers", FromSchema(core.NewExternalSchema("buyers").PK("id").FK("order_id")).As("Buyers"))
	if _, err := c.ComposeAll(context.Background(), view); err == nil {
		t.Fatal("expected child query error from ComposeAll")
	}
}

// ─── view_export.go: MaxExportRowsValue ──────────────────────────────────────

func TestMaxExportRowsValue(t *testing.T) {
	v := View("x").Version(1)
	if got := v.MaxExportRowsValue(); got != 0 {
		t.Errorf("default MaxExportRowsValue = %d, want 0", got)
	}
	v.MaxExportRows(250)
	if got := v.MaxExportRowsValue(); got != 250 {
		t.Errorf("MaxExportRowsValue after override = %d, want 250", got)
	}
}

// ─── view_mongo_spec.go: KeyNames / stringList / JSONSchemaValidationAction ──

func TestIndexSpec_KeyNames_EmptyKeys(t *testing.T) {
	// Empty-keys spec → nil (the defensive branch not covered elsewhere).
	if names := (&IndexSpec{}).KeyNames(); names != nil {
		t.Errorf("empty-keys KeyNames = %v, want nil", names)
	}
}

func TestStringList(t *testing.T) {
	if got := stringList([]string{"a", "b"}); len(got) != 2 {
		t.Errorf("[]string → %v, want 2 entries", got)
	}
	// Non-slice → nil.
	if got := stringList("nope"); got != nil {
		t.Errorf("non-slice stringList = %v, want nil", got)
	}
	// Mixed slice drops non-string entries.
	if got := stringList([]any{"keep", 7, "also"}); len(got) != 2 {
		t.Errorf("mixed stringList = %v, want 2 string entries", got)
	}
}

func TestJSONSchemaValidationAction_NilAndPresent(t *testing.T) {
	// nil jsonSchema → allocate-with-default-level branch.
	v := View("x").Version(1).JSONSchemaValidationAction(ValidationActionWarn)
	if s := v.SchemaSpec(); s == nil || s.ValidationAction != ValidationActionWarn || s.ValidationLevel != ValidationLevelStrict {
		t.Fatalf("nil-path spec drifted: %+v", v.SchemaSpec())
	}
	// Existing jsonSchema → just set the action.
	v2 := View("y").Version(1).JSONSchema(bson.M{"bsonType": "object"}).
		JSONSchemaValidationAction(ValidationActionWarn)
	if s := v2.SchemaSpec(); s == nil || s.ValidationAction != ValidationActionWarn {
		t.Fatalf("present-path spec drifted: %+v", v2.SchemaSpec())
	}
}

// ─── mongo_drift.go: Format* empty-plan guards ───────────────────────────────

func TestFormatDiagnostics_EmptyPlansReturnEmpty(t *testing.T) {
	empty := []DriftPlan{}
	formatters := map[string]func([]DriftPlan) string{
		"forgotToBump": FormatForgotToBumpDiagnostic,
		"downgrade":    FormatDowngradeDiagnostic,
		"mongoWiped":   FormatMongoWipedDiagnostic,
		"artifactOnly": FormatArtifactOnlyDiagnostic,
		"freshInit":    func(pl []DriftPlan) string { return FormatFreshInitDiagnostic("postgres", pl) },
	}
	for name, f := range formatters {
		if got := f(empty); got != "" {
			t.Errorf("%s on empty plans = %q, want empty string", name, got)
		}
	}
}
