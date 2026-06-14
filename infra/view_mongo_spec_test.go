package infra

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ─── IndexSpec constructors ──────────────────────────────────────────────────

func TestIndex_AscendingByDefault(t *testing.T) {
	s := Index("email")
	if got := len(s.Keys); got != 1 {
		t.Fatalf("Keys length = %d, want 1", got)
	}
	if s.Keys[0].Field != "email" {
		t.Errorf("field = %q, want %q", s.Keys[0].Field, "email")
	}
	if s.Keys[0].Order != IndexOrderAsc {
		t.Errorf("order = %q, want %q", s.Keys[0].Order, IndexOrderAsc)
	}
}

func TestCompound_PreservesFieldOrder(t *testing.T) {
	s := Compound("email", "created_at", "status")
	want := []string{"email", "created_at", "status"}
	if got := len(s.Keys); got != len(want) {
		t.Fatalf("Keys length = %d, want %d", got, len(want))
	}
	for i, k := range s.Keys {
		if k.Field != want[i] {
			t.Errorf("Keys[%d].Field = %q, want %q", i, k.Field, want[i])
		}
		if k.Order != IndexOrderAsc {
			t.Errorf("Keys[%d].Order = %q, want %q", i, k.Order, IndexOrderAsc)
		}
	}
}

func TestTextIndex_AllKeysAreText(t *testing.T) {
	s := TextIndex("name", "email", "bio")
	if got := len(s.Keys); got != 3 {
		t.Fatalf("Keys length = %d, want 3", got)
	}
	for _, k := range s.Keys {
		if k.Order != IndexOrderText {
			t.Errorf("key %q order = %q, want %q", k.Field, k.Order, IndexOrderText)
		}
	}
	if !s.IsText() {
		t.Error("IsText() = false, want true on a TextIndex")
	}
}

func TestGeoIndex_Uses2DSphere(t *testing.T) {
	s := GeoIndex("location")
	if s.Keys[0].Order != IndexOrderGeo2DSph {
		t.Errorf("order = %q, want %q", s.Keys[0].Order, IndexOrderGeo2DSph)
	}
}

// ─── Fluent setters ──────────────────────────────────────────────────────────

func TestIndexSpec_FluentChain(t *testing.T) {
	s := Index("email").Unique().Sparse().Name("custom_idx").Hidden()
	if !s.unique {
		t.Error("Unique() did not flip unique")
	}
	if !s.sparse {
		t.Error("Sparse() did not flip sparse")
	}
	if !s.hidden {
		t.Error("Hidden() did not flip hidden")
	}
	if s.name != "custom_idx" {
		t.Errorf("Name() result = %q, want %q", s.name, "custom_idx")
	}
}

func TestIndexSpec_Desc_FlipsTrailingAscKey(t *testing.T) {
	s := Compound("email", "created_at").Desc()
	if s.Keys[0].Order != IndexOrderAsc {
		t.Errorf("Keys[0].Order = %q, want unchanged %q", s.Keys[0].Order, IndexOrderAsc)
	}
	if s.Keys[1].Order != IndexOrderDesc {
		t.Errorf("Keys[1].Order = %q, want %q after Desc()", s.Keys[1].Order, IndexOrderDesc)
	}
}

func TestIndexSpec_Desc_NoOpOnAlreadyDesc(t *testing.T) {
	// Desc() flips ONLY ascending keys (the documented "trailing Asc → Desc"
	// behavior). A trailing key that is not ascending stays as-is.
	s := TextIndex("name").Desc()
	if s.Keys[0].Order != IndexOrderText {
		t.Errorf("Keys[0].Order = %q, want text (Desc must be a no-op on non-asc)", s.Keys[0].Order)
	}
}

func TestIndexSpec_Hashed_FlipsTrailingKey(t *testing.T) {
	s := Index("user_id").Hashed()
	if s.Keys[0].Order != IndexOrderHashed {
		t.Errorf("order = %q, want %q", s.Keys[0].Order, IndexOrderHashed)
	}
}

func TestIndexSpec_TTL_ConvertsToSeconds(t *testing.T) {
	s := Index("expires_at").TTL(7 * 24 * time.Hour)
	if s.expireAfterSeconds == nil {
		t.Fatal("expireAfterSeconds = nil after TTL()")
	}
	if got := *s.expireAfterSeconds; got != 7*24*3600 {
		t.Errorf("expireAfterSeconds = %d, want %d", got, 7*24*3600)
	}
}

func TestIndexSpec_Partial_StoresFilter(t *testing.T) {
	filter := Exists("deleted_at", false)
	s := Index("email").Partial(filter)
	if s.partialFilter == nil {
		t.Fatal("partialFilter = nil after Partial()")
	}
	inner, ok := s.partialFilter["deleted_at"].(bson.M)
	if !ok {
		t.Fatalf("partialFilter.deleted_at type = %T, want bson.M", s.partialFilter["deleted_at"])
	}
	if exists, _ := inner["$exists"].(bool); exists {
		t.Error("$exists = true, want false")
	}
}

func TestIndexSpec_TextOptions(t *testing.T) {
	s := TextIndex("name", "email").
		DefaultLanguage("portuguese").
		LanguageOverride("lang").
		Weights(map[string]int{"name": 10, "email": 5})
	if s.defaultLanguage != "portuguese" {
		t.Errorf("defaultLanguage = %q, want %q", s.defaultLanguage, "portuguese")
	}
	if s.languageOverride != "lang" {
		t.Errorf("languageOverride = %q, want %q", s.languageOverride, "lang")
	}
	if len(s.weights) != 2 || s.weights["name"] != 10 || s.weights["email"] != 5 {
		t.Errorf("weights = %v, want {name:10, email:5}", s.weights)
	}
}

// ─── Exists helper ────────────────────────────────────────────────────────────

func TestExists_FalseShape(t *testing.T) {
	got := Exists("deleted_at", false)
	if len(got) != 1 {
		t.Fatalf("Exists returned %d keys, want 1", len(got))
	}
	inner, ok := got["deleted_at"].(bson.M)
	if !ok {
		t.Fatalf("Exists value type = %T, want bson.M", got["deleted_at"])
	}
	exists, ok := inner["$exists"].(bool)
	if !ok || exists {
		t.Errorf("$exists = %v (ok=%v), want false", inner["$exists"], ok)
	}
}

func TestExists_TrueShape(t *testing.T) {
	inner := Exists("phone", true)["phone"].(bson.M)
	if exists, _ := inner["$exists"].(bool); !exists {
		t.Error("$exists = false, want true")
	}
}

// ─── IndexModel conversion ────────────────────────────────────────────────────

func TestIndexModel_KeysPreserveOrderAndDirection(t *testing.T) {
	s := Compound("email", "created_at").Desc()
	im := s.IndexModel()
	keys, ok := im.Keys.(bson.D)
	if !ok {
		t.Fatalf("Keys type = %T, want bson.D (order-preserving)", im.Keys)
	}
	if len(keys) != 2 {
		t.Fatalf("Keys length = %d, want 2", len(keys))
	}
	if keys[0].Key != "email" || keys[0].Value != int32(1) {
		t.Errorf("Keys[0] = %+v, want {email, 1}", keys[0])
	}
	if keys[1].Key != "created_at" || keys[1].Value != int32(-1) {
		t.Errorf("Keys[1] = %+v, want {created_at, -1}", keys[1])
	}
}

func TestIndexModel_TextKey(t *testing.T) {
	s := TextIndex("name", "email")
	im := s.IndexModel()
	keys := im.Keys.(bson.D)
	for _, k := range keys {
		if k.Value != "text" {
			t.Errorf("Key %q value = %v, want \"text\"", k.Key, k.Value)
		}
	}
}

func TestIndexModel_HashedKey(t *testing.T) {
	im := Index("user_id").Hashed().IndexModel()
	keys := im.Keys.(bson.D)
	if keys[0].Value != "hashed" {
		t.Errorf("hashed key value = %v, want \"hashed\"", keys[0].Value)
	}
}

func TestIndexModel_OptionsBuilderNotNil(t *testing.T) {
	// The driver's builder pattern returns *IndexOptionsBuilder; we don't
	// reach into its private state — verifying it is non-nil is enough to
	// confirm we actually built and attached it. The wire-level assertion
	// lives in Phase B's apply-step integration tests.
	im := Index("email").Unique().Name("custom").IndexModel()
	if im.Options == nil {
		t.Fatal("IndexModel.Options = nil, want non-nil builder")
	}
}

// ─── CollationSpec ────────────────────────────────────────────────────────────

func TestCollationSpec_DriverCollation_NilSafe(t *testing.T) {
	var c *CollationSpec
	if c.DriverCollation() != nil {
		t.Error("nil CollationSpec.DriverCollation() != nil")
	}
}

func TestCollationSpec_DriverCollation_FieldMirror(t *testing.T) {
	c := &CollationSpec{
		Locale: "pt", Strength: 1, NumericOrdering: true,
		Alternate: "shifted", MaxVariable: "punct",
	}
	got := c.DriverCollation()
	if got.Locale != "pt" || got.Strength != 1 || !got.NumericOrdering {
		t.Errorf("DriverCollation mismatch: got=%+v", got)
	}
	if got.Alternate != "shifted" || got.MaxVariable != "punct" {
		t.Errorf("alternate/maxVariable mismatch: %+v", got)
	}
}

// ─── ViewDefinition extensions ────────────────────────────────────────────────

func TestViewDefinition_Indexes_Accumulates(t *testing.T) {
	v := View("users").Root("users").
		Indexes(Index("email").Unique()).
		Indexes(Index("created_at").Desc(), Compound("email", "created_at"))
	if got := len(v.IndexSpecs()); got != 3 {
		t.Errorf("IndexSpecs len = %d, want 3 (accumulated across two calls)", got)
	}
}

func TestViewDefinition_JSONSchema_DefaultsToStrictError(t *testing.T) {
	v := View("users").Root("users").JSONSchema(bson.M{"bsonType": "object"})
	js := v.SchemaSpec()
	if js == nil {
		t.Fatal("SchemaSpec() = nil after JSONSchema()")
	}
	if js.ValidationLevel != ValidationLevelStrict {
		t.Errorf("validationLevel = %q, want %q", js.ValidationLevel, ValidationLevelStrict)
	}
	if js.ValidationAction != ValidationActionError {
		t.Errorf("validationAction = %q, want %q", js.ValidationAction, ValidationActionError)
	}
}

func TestViewDefinition_JSONSchema_OverrideLevelAndAction(t *testing.T) {
	v := View("users").Root("users").
		JSONSchema(bson.M{"bsonType": "object"}).
		JSONSchemaValidationLevel(ValidationLevelModerate).
		JSONSchemaValidationAction(ValidationActionWarn)
	js := v.SchemaSpec()
	if js.ValidationLevel != ValidationLevelModerate {
		t.Errorf("validationLevel = %q, want %q", js.ValidationLevel, ValidationLevelModerate)
	}
	if js.ValidationAction != ValidationActionWarn {
		t.Errorf("validationAction = %q, want %q", js.ValidationAction, ValidationActionWarn)
	}
}

func TestViewDefinition_JSONSchemaValidationLevel_BeforeSchema(t *testing.T) {
	// Order independence: calling the level setter before JSONSchema(schema)
	// must initialize the spec with the override + default action.
	v := View("users").Root("users").
		JSONSchemaValidationLevel(ValidationLevelOff)
	js := v.SchemaSpec()
	if js == nil {
		t.Fatal("SchemaSpec() = nil after JSONSchemaValidationLevel()")
	}
	if js.ValidationLevel != ValidationLevelOff {
		t.Errorf("validationLevel = %q, want %q", js.ValidationLevel, ValidationLevelOff)
	}
	if js.ValidationAction != ValidationActionError {
		t.Errorf("default validationAction = %q, want %q", js.ValidationAction, ValidationActionError)
	}
}

func TestViewDefinition_Collation_StoresPointer(t *testing.T) {
	c := &CollationSpec{Locale: "pt", Strength: 1}
	v := View("users").Root("users").Collation(c)
	if v.CollectionCollation() != c {
		t.Error("CollectionCollation() != injected pointer")
	}
}

func TestViewDefinition_CappedAndTimeSeries_StorageRoundTrip(t *testing.T) {
	cap := &CappedSpec{SizeBytes: 1 << 30, MaxDocs: 1_000_000}
	v := View("audit").Root("audit").Capped(cap)
	if v.CappedDeclaration() != cap {
		t.Error("CappedDeclaration() != injected pointer")
	}

	ts := &TimeSeriesSpec{TimeField: "ts", MetaField: "sensor_id", Granularity: "seconds"}
	v2 := View("metrics").Root("metrics").TimeSeries(ts)
	if v2.TimeSeriesDeclaration() != ts {
		t.Error("TimeSeriesDeclaration() != injected pointer")
	}
}

// ─── ValidateMongoSpec ───────────────────────────────────────────────────────

func TestValidateMongoSpec_OK(t *testing.T) {
	v := View("users").Version(1).Root("users").
		Indexes(
			Index("email").Unique(),
			Compound("email", "created_at").Desc(),
			Index("deleted_at").Partial(Exists("deleted_at", false)),
			TextIndex("name", "email").DefaultLanguage("portuguese"),
		).
		JSONSchema(bson.M{"bsonType": "object"})
	if err := v.ValidateMongoSpec(); err != nil {
		t.Errorf("ValidateMongoSpec() = %v, want nil on well-formed view", err)
	}
}

func TestValidateMongoSpec_CappedAndTimeSeriesMutuallyExclusive(t *testing.T) {
	v := View("x").Version(1).Root("x").
		Capped(&CappedSpec{SizeBytes: 1 << 20}).
		TimeSeries(&TimeSeriesSpec{TimeField: "ts"})
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want \"mutually exclusive\"", err)
	}
}

func TestValidateMongoSpec_CappedRequiresPositiveSize(t *testing.T) {
	v := View("x").Version(1).Root("x").Capped(&CappedSpec{SizeBytes: 0})
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "SizeBytes") {
		t.Errorf("err = %v, want SizeBytes diagnostic", err)
	}
}

func TestValidateMongoSpec_TimeSeriesRequiresTimeField(t *testing.T) {
	v := View("x").Version(1).Root("x").TimeSeries(&TimeSeriesSpec{Granularity: "seconds"})
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "TimeField") {
		t.Errorf("err = %v, want TimeField diagnostic", err)
	}
}

func TestValidateMongoSpec_TimeSeriesGranularityClosedSet(t *testing.T) {
	v := View("x").Version(1).Root("x").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "days"})
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "Granularity") {
		t.Errorf("err = %v, want Granularity diagnostic", err)
	}
}

func TestValidateMongoSpec_TextIndexAtMostOne(t *testing.T) {
	v := View("x").Version(1).Root("x").Indexes(
		TextIndex("name"),
		TextIndex("email"),
	)
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "TextIndex") {
		t.Errorf("err = %v, want TextIndex cap diagnostic", err)
	}
}

func TestValidateMongoSpec_RejectsEmptyKeys(t *testing.T) {
	v := View("x").Version(1).Root("x").Indexes(&IndexSpec{})
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "no keys") {
		t.Errorf("err = %v, want no-keys diagnostic", err)
	}
}

func TestValidateMongoSpec_JSONSchemaLevelClosedSet(t *testing.T) {
	v := View("x").Version(1).Root("x").
		JSONSchema(bson.M{}).
		JSONSchemaValidationLevel("loose") // not in {strict, moderate, off}
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "ValidationLevel") {
		t.Errorf("err = %v, want ValidationLevel diagnostic", err)
	}
}

func TestValidateMongoSpec_JSONSchemaActionClosedSet(t *testing.T) {
	v := View("x").Version(1).Root("x").
		JSONSchema(bson.M{}).
		JSONSchemaValidationAction("drop") // not in {error, warn}
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), "ValidationAction") {
		t.Errorf("err = %v, want ValidationAction diagnostic", err)
	}
}

func TestValidateMongoSpec_DiagnosticCarriesViewName(t *testing.T) {
	v := View("orders").Version(1).Root("orders").
		Capped(&CappedSpec{SizeBytes: 1 << 20}).
		TimeSeries(&TimeSeriesSpec{TimeField: "ts"})
	err := v.ValidateMongoSpec()
	if err == nil || !strings.Contains(err.Error(), `"orders"`) {
		t.Errorf("err = %v, want diagnostic naming view %q", err, "orders")
	}
}

// ─── Default state guarantees ────────────────────────────────────────────────

// A freshly created view (no mongo-spec calls) must round-trip the
// accessors as nil/empty so Phase B's ApplyMongoSpecs can skip the view
// entirely without an explicit "is configured" probe. Version(1) declared
// so ValidateMongoSpec passes — the mandatory Version is orthogonal to
// the mongo-spec accessors being unset.
func TestViewDefinition_DefaultMongoSpec_AllNil(t *testing.T) {
	v := View("x").Version(1).Root("x")
	if len(v.IndexSpecs()) != 0 {
		t.Errorf("IndexSpecs len = %d, want 0", len(v.IndexSpecs()))
	}
	if v.SchemaSpec() != nil {
		t.Error("SchemaSpec() != nil on default view")
	}
	if v.CollectionCollation() != nil {
		t.Error("CollectionCollation() != nil on default view")
	}
	if v.CappedDeclaration() != nil {
		t.Error("CappedDeclaration() != nil on default view")
	}
	if v.TimeSeriesDeclaration() != nil {
		t.Error("TimeSeriesDeclaration() != nil on default view")
	}
	if err := v.ValidateMongoSpec(); err != nil {
		t.Errorf("ValidateMongoSpec() = %v, want nil on default view", err)
	}
}
