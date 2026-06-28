package query

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Determinism — same spec hashes to the same value across distinct
// ViewDefinition instances. Guards against accidental dependence on
// pointer identity or map iteration order.

func TestHash_DeterministicAcrossInstances(t *testing.T) {
	build := func() *ViewDefinition {
		return View("users").Root("users").
			EmbedMany("addresses", pgEmbed("addresses", "user_id")).
			Indexes(Index("email").Unique(), Compound("email", "created_at"))
	}
	a, b := build(), build()
	if a.Hash() != b.Hash() {
		t.Fatalf("Hash() not deterministic: %q vs %q", a.Hash(), b.Hash())
	}
	if a.RebuildHash() != b.RebuildHash() {
		t.Errorf("RebuildHash() not deterministic")
	}
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Errorf("ArtifactHash() not deterministic")
	}
}

func TestHash_LengthAndFormat(t *testing.T) {
	v := View("users").Root("users")
	r, a, h := v.RebuildHash(), v.ArtifactHash(), v.Hash()
	if len(r) != 64 {
		t.Errorf("RebuildHash len = %d, want 64 hex chars", len(r))
	}
	if len(a) != 64 {
		t.Errorf("ArtifactHash len = %d, want 64 hex chars", len(a))
	}
	if len(h) != 64 {
		t.Errorf("Hash len = %d, want 64 hex chars", len(h))
	}
	for _, c := range r {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("RebuildHash contains non-hex char %q", c)
		}
	}
}

// RebuildHash partition — fields that should change the rebuild hash.

func TestRebuildHash_RootTableChange(t *testing.T) {
	a := View("users").Root("users")
	b := View("users").Root("user_records")
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different rootTable")
	}
}

func TestRebuildHash_EmbedShapeChange(t *testing.T) {
	a := View("users").Root("users").
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	b := View("users").Root("users").
		EmbedMany("addresses", pgEmbed("addresses", "uid")) // different joinKey
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different embed joinKey")
	}
}

func TestRebuildHash_EmbedAddition(t *testing.T) {
	a := View("orders").Root("orders")
	b := View("orders").Root("orders").
		EmbedMany("lines", pgEmbed("order_lines", "order_id"))
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite added embed")
	}
}

func TestRebuildHash_DeleteOnArchiveFlag(t *testing.T) {
	a := View("users").Root("users")
	b := View("users").Root("users").DeleteOnArchive()
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite DeleteOnArchive flag change")
	}
}

func TestRebuildHash_JSONSchemaChange(t *testing.T) {
	a := View("users").Root("users").
		JSONSchema(bson.M{"bsonType": "object", "required": []string{"_id", "email"}})
	b := View("users").Root("users").
		JSONSchema(bson.M{"bsonType": "object", "required": []string{"_id"}})
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite JSONSchema required change")
	}
}

func TestRebuildHash_JSONSchemaDefaults_NormalizeEmptyToStrictError(t *testing.T) {
	// Declaring JSONSchema without explicit level/action must hash the same
	// as declaring them explicitly with the strict/error defaults — matches
	// the runtime resolution in view_mongo_spec.go.
	schema := bson.M{"bsonType": "object"}
	a := View("u").Root("u").JSONSchema(schema)
	b := View("u").Root("u").JSONSchema(schema).
		JSONSchemaValidationLevel(ValidationLevelStrict).
		JSONSchemaValidationAction(ValidationActionError)
	if a.RebuildHash() != b.RebuildHash() {
		t.Errorf("RebuildHash differs between implicit defaults and explicit defaults: %q vs %q", a.RebuildHash(), b.RebuildHash())
	}
}

func TestRebuildHash_CollationChange(t *testing.T) {
	a := View("u").Root("u").Collation(&CollationSpec{Locale: "pt", Strength: 1})
	b := View("u").Root("u").Collation(&CollationSpec{Locale: "pt", Strength: 2})
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different Collation strength")
	}
}

func TestRebuildHash_CappedChange(t *testing.T) {
	a := View("u").Root("u").Capped(&CappedSpec{SizeBytes: 1 << 20})
	b := View("u").Root("u").Capped(&CappedSpec{SizeBytes: 1 << 30})
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different Capped size")
	}
}

func TestRebuildHash_TimeSeriesChange(t *testing.T) {
	a := View("u").Root("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "seconds"})
	b := View("u").Root("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "minutes"})
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different TimeSeries granularity")
	}
}

func TestRebuildHash_TimeSeriesGranularityCaseInsensitive(t *testing.T) {
	// "Seconds" and "seconds" must hash equal — same Mongo wire value.
	a := View("u").Root("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "Seconds"})
	b := View("u").Root("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "seconds"})
	if a.RebuildHash() != b.RebuildHash() {
		t.Error("RebuildHash differs between 'Seconds' and 'seconds' — normalization broken")
	}
}

// ArtifactHash partition — fields that should NOT change RebuildHash but
// should change ArtifactHash.

func TestArtifactHash_IndexAddition(t *testing.T) {
	a := View("u").Root("u")
	b := View("u").Root("u").Indexes(Index("email").Unique())
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite added index")
	}
	if a.RebuildHash() != b.RebuildHash() {
		t.Error("RebuildHash should NOT change for index addition")
	}
}

func TestArtifactHash_IndexNameOverride(t *testing.T) {
	a := View("u").Root("u").Indexes(Index("email").Unique())
	b := View("u").Root("u").Indexes(Index("email").Unique().Name("email_unique_custom"))
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite Name override on index")
	}
}

func TestArtifactHash_PartialFilterChange(t *testing.T) {
	a := View("u").Root("u").Indexes(Index("deleted_at").Partial(Exists("deleted_at", false)))
	b := View("u").Root("u").Indexes(Index("deleted_at").Partial(Exists("deleted_at", true)))
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite different partialFilter")
	}
}

func TestArtifactHash_TTLChange(t *testing.T) {
	a := View("u").Root("u").Indexes(Index("expires_at").TTL(time.Hour))
	b := View("u").Root("u").Indexes(Index("expires_at").TTL(2 * time.Hour))
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite different TTL")
	}
}

func TestArtifactHash_IndexOrderInvariant(t *testing.T) {
	// Declaring indexes in different order on Indexes(...) must yield the
	// same ArtifactHash — semantically the index set is unordered.
	a := View("u").Root("u").Indexes(
		Index("email").Unique(),
		Compound("name", "created_at"),
	)
	b := View("u").Root("u").Indexes(
		Compound("name", "created_at"),
		Index("email").Unique(),
	)
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Errorf("ArtifactHash differs across declaration order: %q vs %q", a.ArtifactHash(), b.ArtifactHash())
	}
}

func TestArtifactHash_AccumulatedAcrossCalls(t *testing.T) {
	// Two Indexes(...) calls accumulate (per view_mongo_spec.go) — must
	// hash same as a single call carrying both.
	a := View("u").Root("u").
		Indexes(Index("email").Unique()).
		Indexes(Compound("name", "created_at"))
	b := View("u").Root("u").
		Indexes(Index("email").Unique(), Compound("name", "created_at"))
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Errorf("ArtifactHash differs between accumulated and single-call: %q vs %q", a.ArtifactHash(), b.ArtifactHash())
	}
}

// Combined Hash is sensitive to both partitions.

func TestHash_ChangesWithRebuildShape(t *testing.T) {
	a := View("u").Root("u")
	b := View("u").Root("user_records")
	if a.Hash() == b.Hash() {
		t.Error("Hash same despite rootTable change")
	}
}

func TestHash_ChangesWithArtifactShape(t *testing.T) {
	a := View("u").Root("u")
	b := View("u").Root("u").Indexes(Index("email"))
	if a.Hash() == b.Hash() {
		t.Error("Hash same despite added index")
	}
}

// bson.M map-key order invariance — the canonical writer sorts keys.

func TestBSONValueWriter_MapKeyOrderInvariant(t *testing.T) {
	// Two JSON schemas with the same key set but produced via different
	// insertion order must hash the same. Go map iteration is unordered,
	// so this is essentially testing that the sort in writeSortedMap
	// removes the nondeterminism.
	a := View("u").Root("u").JSONSchema(bson.M{
		"bsonType": "object",
		"required": []string{"_id", "email"},
		"properties": bson.M{
			"email": bson.M{"bsonType": "string"},
			"_id":   bson.M{"bsonType": "string"},
		},
	})
	b := View("u").Root("u").JSONSchema(bson.M{
		"properties": bson.M{
			"_id":   bson.M{"bsonType": "string"},
			"email": bson.M{"bsonType": "string"},
		},
		"required": []string{"_id", "email"},
		"bsonType": "object",
	})
	if a.Hash() != b.Hash() {
		t.Errorf("Hash differs across map-key declaration order: %q vs %q", a.Hash(), b.Hash())
	}
}

// IndexSpec weights map — same order invariance.

func TestArtifactHash_TextWeightsOrderInvariant(t *testing.T) {
	a := View("u").Root("u").Indexes(TextIndex("name", "email").
		Weights(map[string]int{"name": 10, "email": 5}))
	b := View("u").Root("u").Indexes(TextIndex("name", "email").
		Weights(map[string]int{"email": 5, "name": 10}))
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Errorf("ArtifactHash differs across weights map order: %q vs %q", a.ArtifactHash(), b.ArtifactHash())
	}
}

// Numeric normalization — int / int32 / int64 hash equal as bson values.

func TestBSONValueWriter_NumericNormalization(t *testing.T) {
	a := View("u").Root("u").JSONSchema(bson.M{"v": int(42)})
	b := View("u").Root("u").JSONSchema(bson.M{"v": int32(42)})
	c := View("u").Root("u").JSONSchema(bson.M{"v": int64(42)})
	if a.Hash() != b.Hash() || b.Hash() != c.Hash() {
		t.Errorf("numeric normalization broken: int=%q int32=%q int64=%q", a.Hash(), b.Hash(), c.Hash())
	}
}

// Negative — RebuildHash is unaffected by ArtifactHash-only changes.

func TestRebuildHash_StableUnderIndexChanges(t *testing.T) {
	a := View("u").Root("u")
	b := View("u").Root("u").Indexes(Index("email").Unique())
	c := View("u").Root("u").Indexes(Index("email").Unique(), Compound("a", "b"))
	if a.RebuildHash() != b.RebuildHash() {
		t.Errorf("RebuildHash changed when adding an index (should be ArtifactHash-only): %q vs %q", a.RebuildHash(), b.RebuildHash())
	}
	if b.RebuildHash() != c.RebuildHash() {
		t.Errorf("RebuildHash changed when adding more indexes: %q vs %q", b.RebuildHash(), c.RebuildHash())
	}
}

// Nested source.embeds participate in the hash.

func TestRebuildHash_NestedEmbedChange(t *testing.T) {
	a := View("orders").Root("orders").
		EmbedMany("lines", pgEmbed("order_lines", "order_id").
			Embed("product", pgEmbed("products", "").On("product_id")))
	b := View("orders").Root("orders").
		EmbedMany("lines", pgEmbed("order_lines", "order_id"))
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite nested embed change")
	}
}

// Version participates in RebuildHash — bumping the version always produces
// a new hash even if every other declarative field is identical. Closes the
// loop between "developer signals intent via Version(N)" and "framework
// detects the intent via the hash". See tasks/mongo_schema_evolution_2.md §8.3.

func TestRebuildHash_VersionBumpChangesHash(t *testing.T) {
	a := View("users").Version(1).Root("users")
	b := View("users").Version(2).Root("users")
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite Version bump 1 → 2")
	}
}

func TestRebuildHash_VersionParticipates_ArtifactStable(t *testing.T) {
	a := View("users").Version(1).Root("users").Indexes(Index("email").Unique())
	b := View("users").Version(2).Root("users").Indexes(Index("email").Unique())
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Error("ArtifactHash changed across Version bump — version belongs to RebuildHash partition, not ArtifactHash")
	}
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite Version bump")
	}
}

func TestRebuildHash_SameVersionSameSpec(t *testing.T) {
	a := View("users").Version(3).Root("users").
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	b := View("users").Version(3).Root("users").
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	if a.RebuildHash() != b.RebuildHash() {
		t.Errorf("RebuildHash not deterministic across instances with same version + spec: %q vs %q", a.RebuildHash(), b.RebuildHash())
	}
}

// ValidateMongoSpec rejects views without Version(N) — the framework
// guarantees no view reaches the runtime without an explicit version.

func TestValidateMongoSpec_RejectsMissingVersion(t *testing.T) {
	v := View("users").Root("users")
	if err := v.ValidateMongoSpec(); err == nil {
		t.Fatal("expected error: view without Version(N) must be rejected")
	}
}

func TestValidateMongoSpec_RejectsNegativeVersion(t *testing.T) {
	v := View("users").Version(-1).Root("users")
	if err := v.ValidateMongoSpec(); err == nil {
		t.Fatal("expected error: negative Version must be rejected")
	}
}

func TestValidateMongoSpec_AcceptsPositiveVersion(t *testing.T) {
	v := View("users").Version(1).Root("users")
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("unexpected error for valid Version(1): %v", err)
	}
}
