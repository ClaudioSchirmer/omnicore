package infra

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
)

// This file is the final coverage mop-up for the infra root package: it drives
// the remaining REACHABLE branches that run without a live Postgres/Mongo/Kafka.
// Everything here reuses the in-package fakes (fakePool/fakeTx/fakeRows in
// pg_unit_fake_test.go, loaderPostgres in aggregate_loader_live_test.go,
// composerRows in composer_unit_test.go) and the shared builderTestEntity /
// builderTestSchema fixtures.

// ─── exception.go: NewInfrastructureError + Error() ──────────────────────────

func TestNewInfrastructureError_AndError(t *testing.T) {
	ctx := domain.NewNotificationContext("Test")
	err := NewInfrastructureError([]*domain.NotificationContext{ctx})
	if err == nil || len(err.Contexts) != 1 {
		t.Fatalf("expected 1-context error, got %+v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "1 context") {
		t.Errorf("Error() = %q, want it to name the context count", msg)
	}
	// Zero-context shape still renders.
	if msg := NewInfrastructureError(nil).Error(); !strings.Contains(msg, "0 context") {
		t.Errorf("Error() on empty = %q", msg)
	}
}

// ─── base_repository.go: Scope + boundWriter delegation ──────────────────────

func newScopedRepo(pool pgxPool) *BaseRepository[*builderTestEntity] {
	return &BaseRepository[*builderTestEntity]{
		Postgres:  newFakePostgres(pool),
		Schema:    builderTestSchema,
		NewEntity: func() *builderTestEntity { return &builderTestEntity{} },
	}
}

func appCtx() *configuration.AppContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func TestBoundWriter_AllVerbs_HappyPath(t *testing.T) {
	pool := newFakePool()
	w := newScopedRepo(pool).Scope(appCtx())

	id, err := w.Insert(mustInsertable(t, &builderTestEntity{Name: "a", Email: "a@x.com"}))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id.Value() == "" {
		t.Error("Insert returned empty id")
	}
	if err := w.Update(mustUpdatable(t, newFlatEntity())); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := w.Archive(mustArchivable(t, newFlatEntity())); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := w.Unarchive(mustUnarchivable(t, newFlatEntity())); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if err := w.Delete(mustDeletable(t, newFlatEntity())); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestBoundWriter_InsertError_MapsThroughMapErr(t *testing.T) {
	pool := newFakePool()
	pool.tx.queryRowErr = errFake // the RETURNING-id scan fails
	w := newScopedRepo(pool).Scope(appCtx())
	if _, err := w.Insert(mustInsertable(t, &builderTestEntity{Name: "a", Email: "a@x.com"})); err == nil {
		t.Fatal("expected Insert error to surface through mapErr")
	}
}

func TestBoundWriter_UpdateError_MapsThroughMapErr(t *testing.T) {
	pool := newFakePool()
	pool.tx.queryRowErr = errFake
	w := newScopedRepo(pool).Scope(appCtx())
	if err := w.Update(mustUpdatable(t, newFlatEntity())); err == nil {
		t.Fatal("expected Update error through mapErr")
	}
}

func TestBoundWriter_ArchiveDeleteUnarchiveError(t *testing.T) {
	pool := newFakePool()
	pool.tx.execErr = errFake // Archive/Unarchive/Delete data write is an Exec
	w := newScopedRepo(pool).Scope(appCtx())
	if err := w.Archive(mustArchivable(t, newFlatEntity())); err == nil {
		t.Fatal("expected Archive error")
	}
	if err := w.Unarchive(mustUnarchivable(t, newFlatEntity())); err == nil {
		t.Fatal("expected Unarchive error")
	}
	if err := w.Delete(mustDeletable(t, newFlatEntity())); err == nil {
		t.Fatal("expected Delete error")
	}
}

// ─── base_aggregate_repository.go: FindByID + FindArchivedByID ────────────────

func TestBaseAggregateRepository_FindByID_NotFound(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 0}, nil
	})
	repo := NewBaseAggregateRepository[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} })
	repo.WithSchema(builderTestSchema)

	if _, err := repo.FindByID(domain.NewID(uuid.NewString())); err == nil {
		t.Fatal("expected NotFound from FindByID on zero rows")
	}
	if _, err := repo.FindArchivedByID(domain.NewID(uuid.NewString())); err == nil {
		t.Fatal("expected NotFound from FindArchivedByID on zero rows")
	}
}

func TestBaseAggregateRepository_FindByID_Found(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "Jane", "jane@x.com")
			return nil
		}}, nil
	})
	repo := NewBaseAggregateRepository[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} })
	repo.WithSchema(builderTestSchema)

	got, err := repo.FindByID(domain.NewID(liveRootID))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.GetID() == nil || got.GetID().Value() != liveRootID {
		t.Errorf("FindByID id = %v, want %q", got.GetID(), liveRootID)
	}
}

// ─── view_schema.go: pure translator helpers ─────────────────────────────────

func TestViewNode_SchemaLessFallbacks(t *testing.T) {
	// A node with no schema (the defensive empty node) passes paths/docs through
	// and yields no soft-delete gate.
	n := &viewNode{}
	if _, ok := n.softDeleteColumn(); ok {
		t.Error("schema-less node must report no soft-delete")
	}
	doc := map[string]any{"anything": 1}
	if got := n.toGoDoc(doc); len(got) != 1 || got["anything"] != 1 {
		t.Errorf("schema-less toGoDoc must pass through, got %v", got)
	}
	if path, ok := n.columnPath([]string{"X"}); !ok || path[0] != "X" {
		t.Errorf("schema-less columnPath must pass through, got %v,%v", path, ok)
	}
	// toGoDoc on a nil doc returns nil.
	if got := n.toGoDoc(nil); got != nil {
		t.Errorf("nil doc must return nil, got %v", got)
	}
}

func TestViewNode_ColumnPathEdges(t *testing.T) {
	var nilNode *viewNode
	if _, ok := nilNode.columnPath([]string{"X"}); ok {
		t.Error("nil node must not resolve")
	}
	rootSchema := NewTableSchema[vsRoot]("people").PK("person_pk").Field("Email", "mail")
	childSchema := NewExternalSchema("tags").PK("tag_pk").FK("person_ref").Field("ZipCode", "zip")
	v := View("people").Root("people").Schema(rootSchema).
		EmbedMany("addresses", FromSchema(childSchema).As("Addresses"))
	node := v.buildViewNode()

	if _, ok := node.columnPath(nil); ok {
		t.Error("empty path must not resolve")
	}
	if _, ok := node.columnPath([]string{"Unknown"}); ok {
		t.Error("unknown root field must not resolve")
	}
	if _, ok := node.columnPath([]string{"NoSuchEmbed", "ZipCode"}); ok {
		t.Error("unknown embed segment must not resolve")
	}
	if _, ok := node.columnPath([]string{"Addresses", "Unknown"}); ok {
		t.Error("unknown nested field must not resolve")
	}
}

func TestViewNode_ToGoDoc_OneToOneEmbedAndDrop(t *testing.T) {
	rootSchema := NewTableSchema[vsRoot]("people").PK("person_pk").Field("Email", "mail")
	buyerSchema := NewExternalSchema("buyers").PK("b_pk").Field("Email", "b_mail")
	v := View("people").Root("people").Schema(rootSchema).
		Embed("buyer", FromSchema(buyerSchema).On("buyer_ref").As("Buyer"))
	node := v.buildViewNode()

	doc := map[string]any{
		"person_pk": "p1",
		"mail":      "a@x",
		"unmapped":  "dropped", // not in schema → dropped
		"buyer":     map[string]any{"b_pk": "b1", "b_mail": "b@x"},
	}
	got := node.toGoDoc(doc)
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
	childSchema := NewExternalSchema("tags").PK("tag_pk").FK("ref").Field("ZipCode", "zip")
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
	ext := FromSchema(NewExternalSchema("u").PK("id"))
	n2 := newViewNode(builderTestSchema, []embedDef{{field: "buyer", source: ext}})
	if _, ok := n2.embeds["buyer"]; !ok {
		t.Errorf("segment fallback to doc field expected, embeds=%v", n2.embeds)
	}
}

// ─── view.go: Source.Embeds, resolveGoSegment, DependentMongoViews, index ────

func TestSource_Embeds(t *testing.T) {
	src := FromSchema(NewTableSchema[embedFixture]("orders").PK("id")).
		EmbedMany("lines", FromSchema(NewTableSchema[embedFixture]("lines").PK("id").FK("order_id")))
	if len(src.Embeds()) != 1 {
		t.Fatalf("Source.Embeds() = %d, want 1", len(src.Embeds()))
	}
}

func TestResolveGoSegment_ExternalNoAsIsEmpty(t *testing.T) {
	ext := FromSchema(NewExternalSchema("users").PK("id"))
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
	src := FromSchema(NewExternalSchema("users").PK("id").FK("user_id")) // no .As
	v := View("orders").Version(1).Root("orders").
		Schema(rootSchema("orders")).
		EmbedMany("buyers", src)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no Go segment") {
		t.Fatalf("expected external-embed-missing-As error, got %v", err)
	}
}

func TestDependentMongoViews_NestedMatch(t *testing.T) {
	// A view embedding an external (Mongo) collection at a nested level must be
	// reported by DependentMongoViews / viewEmbedsMongoCollection.
	nestedMongo := FromSchema(NewExternalSchema("users").PK("id").FK("order_id")).As("Buyer").On("buyer_id")
	pgLines := FromSchema(NewTableSchema[embedFixture]("lines").PK("id").FK("order_id")).
		Embed("buyer", nestedMongo)
	v := View("orders").Version(1).Root("orders").Schema(rootSchema("orders")).
		EmbedMany("lines", pgLines)

	dep := DependentMongoViews([]*ViewDefinition{v}, "users")
	if len(dep) != 1 {
		t.Fatalf("expected 1 dependent view for nested mongo embed, got %d", len(dep))
	}
	// A non-matching collection name yields nothing.
	if got := DependentMongoViews([]*ViewDefinition{v}, "nope"); len(got) != 0 {
		t.Errorf("unexpected dependents for unknown collection: %d", len(got))
	}
}

func TestBuildViewIndex_PGAndMongo(t *testing.T) {
	mongoSrc := FromSchema(NewExternalSchema("users").PK("id").FK("order_id")).As("Buyers")
	pgSrc := FromSchema(NewTableSchema[embedFixture]("lines").PK("id").FK("order_id"))
	v := View("orders").Version(1).Root("orders").Schema(rootSchema("orders")).
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

// ─── criteria_pg.go: VisitNot / VisitLogical / childScopeFilter remainder ────

func TestPgVisitor_NotErrors(t *testing.T) {
	// NOT with a nil inner expression.
	if _, _, err := compileWhere(criteria.Not(nil), testResolver()); err == nil {
		t.Error("expected error: NOT with nil inner")
	}
	// NOT propagates an inner error (unknown field).
	if _, _, err := compileWhere(criteria.Not(criteria.Eq("Nope", "x")), testResolver()); err == nil {
		t.Error("expected NOT to propagate inner unknown-field error")
	}
}

func TestPgVisitor_LogicalInnerErrorPropagates(t *testing.T) {
	if _, _, err := compileWhere(criteria.And(criteria.Eq("Name", "ok"), criteria.Eq("Nope", "x")), testResolver()); err == nil {
		t.Error("expected AND to propagate inner unknown-field error")
	}
	// OR joiner branch.
	sql, _, err := compileWhere(criteria.Or(criteria.Eq("Name", "a"), criteria.Eq("Email", "b")), testResolver())
	if err != nil || !strings.Contains(sql, " OR ") {
		t.Errorf("OR sql = %q err=%v", sql, err)
	}
}

func TestPgVisitor_UnsupportedOperator(t *testing.T) {
	// A Comparison with an operator outside binaryOps/in/null hits the default
	// "unsupported operator" branch.
	bad := criteria.Comparison{Field: "Name", Op: criteria.Operator("bogus"), Values: []any{"x"}}
	if _, _, err := compileWhere(bad, testResolver()); err == nil {
		t.Error("expected unsupported-operator error")
	}
}

func TestChildScopeFilter_NoSoftDelete(t *testing.T) {
	off := NewExternalSchema("t") // no SoftDelete
	if got := childScopeFilter(criteria.ScopeActive, off); got != "" {
		t.Errorf("no-soft-delete child must yield no filter, got %q", got)
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

// ─── composer.go: fetchPGEmbed nil-key + applyEmbeds error ───────────────────

func TestFetchPGEmbed_MissingParentKey_NoOp(t *testing.T) {
	// EmbedMany whose root row lacks the parent PK column → child fetch skipped,
	// the embed simply does not appear.
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		// Root row omits "id" entirely.
		return &composerRows{cols: []string{"name"}, data: [][]any{{"first"}}}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("lines", FromSchema(composerLineSchema()))
	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, present := doc["lines"]; present {
		t.Error("missing parent key must skip the embed (no lines key)")
	}
}

func TestFetchPGEmbed_OneToOneMissingFK_NoOp(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		// Root row has no buyer_id FK.
		return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		Embed("buyer", FromSchema(composerBuyerSchema()).On("buyer_id").As("Buyer"))
	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, present := doc["buyer"]; present {
		t.Error("missing FK must skip the one-to-one embed")
	}
}

func TestComposer_EmbedChildQueryError(t *testing.T) {
	// The child SELECT errors → fetchWhere error → applyEmbeds error → Compose error.
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		if strings.Contains(sql, "FROM lines") {
			return nil, errFake
		}
		return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("lines", FromSchema(composerLineSchema()))
	if _, err := c.Compose(context.Background(), view, "o1"); err == nil {
		t.Fatal("expected child query error to surface from Compose")
	}
}

// ─── view_schema.go: toGoDoc _id passthrough ─────────────────────────────────

func TestToGoDoc_IDPassthrough(t *testing.T) {
	rootSchema := NewTableSchema[vsRoot]("people").PK("person_pk").Field("Email", "mail")
	node := newViewNode(rootSchema, nil)
	got := node.toGoDoc(map[string]any{"_id": "doc-1", "mail": "a@x"})
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
	v := View("orphan").Version(1).Root("orphan") // no .Schema(...)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no root .Schema") {
		t.Fatalf("expected no-root-schema error, got %v", err)
	}
}

func TestBuildViewIndex_NestedEmbedRecursion(t *testing.T) {
	grandchild := FromSchema(NewTableSchema[embedFixture]("tags").PK("id").FK("line_id"))
	lines := FromSchema(NewTableSchema[embedFixture]("lines").PK("id").FK("order_id")).
		EmbedMany("tags", grandchild)
	v := View("orders").Version(1).Root("orders").Schema(rootSchema("orders")).
		EmbedMany("lines", lines)

	idx := buildViewIndex([]*ViewDefinition{v})
	if len(idx.byPGTable["tags"]) != 1 {
		t.Errorf("nested grandchild table must be indexed via recursion, got %v", idx.byPGTable["tags"])
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

// ─── criteria_pg.go: NotNull-with-values cardinality error ───────────────────

func TestPgVisitor_NotNullWithValuesErrors(t *testing.T) {
	bad := criteria.Comparison{Field: "Phone", Op: criteria.OpNotNull, Values: []any{"x"}}
	if _, _, err := compileWhere(bad, testResolver()); err == nil {
		t.Error("expected error: IS NOT NULL with values")
	}
}

// ─── composer.go: ComposeAll embed error ─────────────────────────────────────

func TestComposeAll_EmbedChildError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		if strings.Contains(sql, "FROM lines") {
			return nil, errFake
		}
		return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "a"}}}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("lines", FromSchema(composerLineSchema()))
	if _, err := c.ComposeAll(context.Background(), view); err == nil {
		t.Fatal("expected child query error from ComposeAll")
	}
}

// ─── aggregate_persister.go: commit/exec error branches per verb ─────────────

// aggInsert/aggUpdate/aggArchive/aggUnarchive/aggDelete drive one aggregate
// verb against the supplied Postgres. Update carries a Changed child so the
// updateChild Exec path is reachable under an injected execErr.
func aggInsert(t *testing.T, pg *Postgres) error {
	root := &covAgg{Name: "agg"}
	domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	_, e := pg.Insert(newBuilderCtx(), ins, covAggSchema, writeHook{})
	return e
}

func aggUpdateWithChangedChild(t *testing.T, pg *Postgres) error {
	root := newCovAgg(t, covChild{ID: "c1", Label: "old"})
	u, err := domain.GetUpdatable(root, func(r *covAgg) error {
		domain.ChangeAggregateChild(r, covChild{ID: "c1", Label: "old"}, covChild{ID: "c1", Label: "new"})
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	_, e := pg.Update(newBuilderCtx(), u, covAggSchema, writeHook{})
	return e
}

func aggArchive(t *testing.T, pg *Postgres) error {
	ar, _ := domain.GetArchivable(newCovAgg(t, covChild{ID: "c1", Label: "x"}), nil, "GetArchivable")
	return pg.Archive(newBuilderCtx(), ar, covAggSchema, writeHook{})
}

func aggUnarchive(t *testing.T, pg *Postgres) error {
	un, _ := domain.GetUnarchivable(newCovAgg(t, covChild{ID: "c1", Label: "x"}), nil, "GetUnarchivable")
	return pg.Unarchive(newBuilderCtx(), un, covAggSchema, writeHook{})
}

func aggDelete(t *testing.T, pg *Postgres) error {
	d, _ := domain.GetDeletable(newCovAgg(t, covChild{ID: "c1", Label: "x"}), nil, "GetDeletable")
	return pg.Delete(newBuilderCtx(), d, covAggSchema, writeHook{})
}

func TestAggregateVerbs_CommitError(t *testing.T) {
	verbs := map[string]func(*testing.T, *Postgres) error{
		"Insert":    aggInsert,
		"Update":    aggUpdateWithChangedChild,
		"Archive":   aggArchive,
		"Unarchive": aggUnarchive,
		"Delete":    aggDelete,
	}
	for name, drive := range verbs {
		t.Run(name, func(t *testing.T) {
			pool := newFakePool()
			pool.tx.commitErr = errFake
			pg := auditedPostgres(pool)
			if err := drive(t, pg); err == nil {
				t.Fatalf("%s: expected commit error", name)
			}
			if pool.tx.committed && pool.tx.commitErr != nil {
				// Commit was attempted (committed flag set) but returned the error.
			}
		})
	}
}

// TestAggregateVerbs_DataExecError covers the data-write Exec failure branch for
// the verbs whose root/cascade write is an Exec (Archive/Unarchive/Delete) and
// the outbox Exec failure for Insert (root + children go through QueryRow, so the
// first Exec that fails is the outbox INSERT).
func TestAggregateVerbs_DataExecError(t *testing.T) {
	verbs := map[string]func(*testing.T, *Postgres) error{
		"Insert":    aggInsert,
		"Update":    aggUpdateWithChangedChild,
		"Archive":   aggArchive,
		"Unarchive": aggUnarchive,
		"Delete":    aggDelete,
	}
	for name, drive := range verbs {
		t.Run(name, func(t *testing.T) {
			pool := newFakePool()
			pool.tx.execErr = errFake
			pg := newFakePostgres(pool)
			if err := drive(t, pg); err == nil {
				t.Fatalf("%s: expected exec error", name)
			}
			if !pool.tx.rolledBack {
				t.Errorf("%s: expected rollback after exec error", name)
			}
			if pool.tx.committed {
				t.Errorf("%s: must not commit after exec error", name)
			}
		})
	}
}

// TestAggregateVerbs_HookErrors covers the fireAfterBegin / fireBeforeCommit
// error-return branches in every aggregate verb (each rolls the TX back).
func TestAggregateVerbs_HookErrors(t *testing.T) {
	drive := func(t *testing.T, pg *Postgres, hook writeHook, verb string) error {
		switch verb {
		case "Insert":
			root := &covAgg{Name: "agg"}
			domain.AddAggregateChild(root, covChild{ID: "c1", Label: "x"})
			ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
			_, e := pg.Insert(newBuilderCtx(), ins, covAggSchema, hook)
			return e
		case "Update":
			u, _ := domain.GetUpdatable(newCovAgg(t, covChild{ID: "c1", Label: "x"}), func(*covAgg) error { return nil }, nil, "GetUpdatable")
			_, e := pg.Update(newBuilderCtx(), u, covAggSchema, hook)
			return e
		case "Archive":
			ar, _ := domain.GetArchivable(newCovAgg(t, covChild{ID: "c1", Label: "x"}), nil, "GetArchivable")
			return pg.Archive(newBuilderCtx(), ar, covAggSchema, hook)
		case "Unarchive":
			un, _ := domain.GetUnarchivable(newCovAgg(t, covChild{ID: "c1", Label: "x"}), nil, "GetUnarchivable")
			return pg.Unarchive(newBuilderCtx(), un, covAggSchema, hook)
		default:
			d, _ := domain.GetDeletable(newCovAgg(t, covChild{ID: "c1", Label: "x"}), nil, "GetDeletable")
			return pg.Delete(newBuilderCtx(), d, covAggSchema, hook)
		}
	}
	afterBeginErr := writeHook{AfterBegin: func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error { return errFake }}
	beforeCommitErr := writeHook{BeforeCommit: func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error { return errFake }}
	for _, verb := range []string{"Insert", "Update", "Archive", "Unarchive", "Delete"} {
		for _, h := range []struct {
			name string
			hook writeHook
		}{{"afterBegin", afterBeginErr}, {"beforeCommit", beforeCommitErr}} {
			t.Run(verb+"/"+h.name, func(t *testing.T) {
				pool := newFakePool()
				pg := newFakePostgres(pool)
				if err := drive(t, pg, h.hook, verb); err == nil {
					t.Fatalf("%s/%s: expected hook error", verb, h.name)
				}
				if !pool.tx.rolledBack {
					t.Errorf("%s/%s: expected rollback", verb, h.name)
				}
			})
		}
	}
}

// ─── view_export.go: MaxExportRowsValue ──────────────────────────────────────

func TestMaxExportRowsValue(t *testing.T) {
	v := View("x").Version(1).Root("x")
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
	v := View("x").Version(1).Root("x").JSONSchemaValidationAction(ValidationActionWarn)
	if s := v.SchemaSpec(); s == nil || s.ValidationAction != ValidationActionWarn || s.ValidationLevel != ValidationLevelStrict {
		t.Fatalf("nil-path spec drifted: %+v", v.SchemaSpec())
	}
	// Existing jsonSchema → just set the action.
	v2 := View("y").Version(1).Root("y").JSONSchema(bson.M{"bsonType": "object"}).
		JSONSchemaValidationAction(ValidationActionWarn)
	if s := v2.SchemaSpec(); s == nil || s.ValidationAction != ValidationActionWarn {
		t.Fatalf("present-path spec drifted: %+v", v2.SchemaSpec())
	}
}

// ─── mongo_spec.go: deriveIndexName / collation+timeSeries divergence ─────────

func TestDeriveIndexName_OrderTokens(t *testing.T) {
	if got := Index("email").Name("custom"); deriveIndexName(got) != "custom" {
		t.Errorf("explicit name must win, got %q", deriveIndexName(got))
	}
	asc := deriveIndexName(Index("email"))
	if asc != "email_1" {
		t.Errorf("asc index name = %q, want email_1", asc)
	}
	desc := deriveIndexName(Index("email").Desc())
	if desc != "email_-1" {
		t.Errorf("desc index name = %q, want email_-1", desc)
	}
	if got := deriveIndexName(Index("phone").Hashed()); got != "phone_hashed" {
		t.Errorf("hashed index name = %q, want phone_hashed", got)
	}
	if got := deriveIndexName(TextIndex("name")); got != "name_text" {
		t.Errorf("text index name = %q, want name_text", got)
	}
	if got := deriveIndexName(GeoIndex("location")); got != "location_2dsphere" {
		t.Errorf("geo index name = %q, want location_2dsphere", got)
	}
}

func TestCollationDivergence_Branches(t *testing.T) {
	decl := &CollationSpec{Locale: "pt", Strength: 1, NumericOrdering: true, Alternate: "shifted"}
	// declared but absent on collection.
	if d := collationDivergence(decl, bson.M{}); d == "" {
		t.Error("declared-but-absent must diverge")
	}
	// present on collection but absent from declaration.
	if d := collationDivergence(nil, bson.M{"collation": bson.M{"locale": "pt"}}); d == "" {
		t.Error("present-but-undeclared must diverge")
	}
	// both nil → match.
	if d := collationDivergence(nil, bson.M{}); d != "" {
		t.Errorf("both-absent must match, got %q", d)
	}
	// locale divergence.
	if d := collationDivergence(decl, bson.M{"collation": bson.M{"locale": "en"}}); d == "" {
		t.Error("locale divergence expected")
	}
	// strength divergence.
	if d := collationDivergence(decl, bson.M{"collation": bson.M{"locale": "pt", "strength": int32(2)}}); d == "" {
		t.Error("strength divergence expected")
	}
	// numericOrdering divergence.
	if d := collationDivergence(decl, bson.M{"collation": bson.M{"locale": "pt", "strength": int32(1), "numericOrdering": false}}); d == "" {
		t.Error("numericOrdering divergence expected")
	}
	// alternate divergence.
	if d := collationDivergence(decl, bson.M{"collation": bson.M{"locale": "pt", "strength": int32(1), "numericOrdering": true, "alternate": "non-ignorable"}}); d == "" {
		t.Error("alternate divergence expected")
	}
	// full match.
	if d := collationDivergence(decl, bson.M{"collation": bson.M{"locale": "pt", "strength": int32(1), "numericOrdering": true, "alternate": "shifted"}}); d != "" {
		t.Errorf("full match expected, got %q", d)
	}
}

func TestTimeSeriesDivergence_Branches(t *testing.T) {
	decl := &TimeSeriesSpec{TimeField: "ts", MetaField: "sensor", Granularity: "seconds"}
	if d := timeSeriesDivergence(decl, bson.M{}); d == "" {
		t.Error("declared-but-absent must diverge")
	}
	if d := timeSeriesDivergence(nil, bson.M{"timeseries": bson.M{"timeField": "ts"}}); d == "" {
		t.Error("present-but-undeclared must diverge")
	}
	if d := timeSeriesDivergence(nil, bson.M{}); d != "" {
		t.Errorf("both-absent must match, got %q", d)
	}
	if d := timeSeriesDivergence(decl, bson.M{"timeseries": bson.M{"timeField": "other"}}); d == "" {
		t.Error("timeField divergence expected")
	}
	if d := timeSeriesDivergence(decl, bson.M{"timeseries": bson.M{"timeField": "ts", "metaField": "other"}}); d == "" {
		t.Error("metaField divergence expected")
	}
	if d := timeSeriesDivergence(decl, bson.M{"timeseries": bson.M{"timeField": "ts", "metaField": "sensor", "granularity": "minutes"}}); d == "" {
		t.Error("granularity divergence expected")
	}
	if d := timeSeriesDivergence(decl, bson.M{"timeseries": bson.M{"timeField": "ts", "metaField": "sensor", "granularity": "seconds"}}); d != "" {
		t.Errorf("full match expected, got %q", d)
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
		"freshInit":    FormatFreshInitDiagnostic,
	}
	for name, f := range formatters {
		if got := f(empty); got != "" {
			t.Errorf("%s on empty plans = %q, want empty string", name, got)
		}
	}
}
