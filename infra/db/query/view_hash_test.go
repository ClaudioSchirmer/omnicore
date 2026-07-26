package query

import (
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Determinism — same spec hashes to the same value across distinct
// ViewDefinition instances. Guards against accidental dependence on
// pointer identity or map iteration order.

func TestHash_DeterministicAcrossInstances(t *testing.T) {
	build := func() *ViewDefinition {
		return View("users").
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
	v := View("users")
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
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("RebuildHash contains non-hex char %q", c)
		}
	}
}

// RebuildHash partition — fields that should change the rebuild hash.

func TestRebuildHash_RootTableChange(t *testing.T) {
	// The root table is now derived from the schema, so it changes by changing
	// the schema's table — both the RootTable() field and the root-schema shape
	// move the rebuild hash.
	a := View("users").Schema(rootSchema("users"))
	b := View("users").Schema(rootSchema("user_records"))
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different root schema table")
	}
}

func TestRebuildHash_EmbedShapeChange(t *testing.T) {
	a := View("users").
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	b := View("users").
		EmbedMany("addresses", pgEmbed("addresses", "uid")) // different joinKey
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different embed joinKey")
	}
}

func TestRebuildHash_EmbedAddition(t *testing.T) {
	a := View("orders")
	b := View("orders").
		EmbedMany("lines", pgEmbed("order_lines", "order_id"))
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite added embed")
	}
}

func TestRebuildHash_DeleteOnArchiveFlag(t *testing.T) {
	a := View("users")
	b := View("users").DeleteOnArchive()
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite DeleteOnArchive flag change")
	}
}

func TestRebuildHash_JSONSchemaChange(t *testing.T) {
	a := View("users").
		JSONSchema(bson.M{"bsonType": "object", "required": []string{"_id", "email"}})
	b := View("users").
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
	a := View("u").JSONSchema(schema)
	b := View("u").JSONSchema(schema).
		JSONSchemaValidationLevel(ValidationLevelStrict).
		JSONSchemaValidationAction(ValidationActionError)
	if a.RebuildHash() != b.RebuildHash() {
		t.Errorf("RebuildHash differs between implicit defaults and explicit defaults: %q vs %q", a.RebuildHash(), b.RebuildHash())
	}
}

func TestRebuildHash_CollationChange(t *testing.T) {
	a := View("u").Collation(&CollationSpec{Locale: "pt", Strength: 1})
	b := View("u").Collation(&CollationSpec{Locale: "pt", Strength: 2})
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different Collation strength")
	}
}

func TestRebuildHash_CappedChange(t *testing.T) {
	a := View("u").Capped(&CappedSpec{SizeBytes: 1 << 20})
	b := View("u").Capped(&CappedSpec{SizeBytes: 1 << 30})
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different Capped size")
	}
}

func TestRebuildHash_TimeSeriesChange(t *testing.T) {
	a := View("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "seconds"})
	b := View("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "minutes"})
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite different TimeSeries granularity")
	}
}

func TestRebuildHash_TimeSeriesGranularityCaseInsensitive(t *testing.T) {
	// "Seconds" and "seconds" must hash equal — same Mongo wire value.
	a := View("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "Seconds"})
	b := View("u").TimeSeries(&TimeSeriesSpec{TimeField: "ts", Granularity: "seconds"})
	if a.RebuildHash() != b.RebuildHash() {
		t.Error("RebuildHash differs between 'Seconds' and 'seconds' — normalization broken")
	}
}

// ArtifactHash partition — fields that should NOT change RebuildHash but
// should change ArtifactHash.

func TestArtifactHash_IndexAddition(t *testing.T) {
	a := View("u")
	b := View("u").Indexes(Index("email").Unique())
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite added index")
	}
	if a.RebuildHash() != b.RebuildHash() {
		t.Error("RebuildHash should NOT change for index addition")
	}
}

func TestArtifactHash_IndexNameOverride(t *testing.T) {
	a := View("u").Indexes(Index("email").Unique())
	b := View("u").Indexes(Index("email").Unique().Name("email_unique_custom"))
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite Name override on index")
	}
}

func TestArtifactHash_PartialFilterChange(t *testing.T) {
	a := View("u").Indexes(Index("deleted_at").Partial(Exists("deleted_at", false)))
	b := View("u").Indexes(Index("deleted_at").Partial(Exists("deleted_at", true)))
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite different partialFilter")
	}
}

func TestArtifactHash_TTLChange(t *testing.T) {
	a := View("u").Indexes(Index("expires_at").TTL(time.Hour))
	b := View("u").Indexes(Index("expires_at").TTL(2 * time.Hour))
	if a.ArtifactHash() == b.ArtifactHash() {
		t.Error("ArtifactHash same despite different TTL")
	}
}

func TestArtifactHash_IndexOrderInvariant(t *testing.T) {
	// Declaring indexes in different order on Indexes(...) must yield the
	// same ArtifactHash — semantically the index set is unordered.
	a := View("u").Indexes(
		Index("email").Unique(),
		Compound("name", "created_at"),
	)
	b := View("u").Indexes(
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
	a := View("u").
		Indexes(Index("email").Unique()).
		Indexes(Compound("name", "created_at"))
	b := View("u").
		Indexes(Index("email").Unique(), Compound("name", "created_at"))
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Errorf("ArtifactHash differs between accumulated and single-call: %q vs %q", a.ArtifactHash(), b.ArtifactHash())
	}
}

// Combined Hash is sensitive to both partitions.

func TestHash_ChangesWithRebuildShape(t *testing.T) {
	a := View("u").Schema(rootSchema("u"))
	b := View("u").Schema(rootSchema("user_records"))
	if a.Hash() == b.Hash() {
		t.Error("Hash same despite root schema table change")
	}
}

func TestHash_ChangesWithArtifactShape(t *testing.T) {
	a := View("u")
	b := View("u").Indexes(Index("email"))
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
	a := View("u").JSONSchema(bson.M{
		"bsonType": "object",
		"required": []string{"_id", "email"},
		"properties": bson.M{
			"email": bson.M{"bsonType": "string"},
			"_id":   bson.M{"bsonType": "string"},
		},
	})
	b := View("u").JSONSchema(bson.M{
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
	a := View("u").Indexes(TextIndex("name", "email").
		Weights(map[string]int{"name": 10, "email": 5}))
	b := View("u").Indexes(TextIndex("name", "email").
		Weights(map[string]int{"email": 5, "name": 10}))
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Errorf("ArtifactHash differs across weights map order: %q vs %q", a.ArtifactHash(), b.ArtifactHash())
	}
}

// Numeric normalization — int / int32 / int64 hash equal as bson values.

func TestBSONValueWriter_NumericNormalization(t *testing.T) {
	a := View("u").JSONSchema(bson.M{"v": int(42)})
	b := View("u").JSONSchema(bson.M{"v": int32(42)})
	c := View("u").JSONSchema(bson.M{"v": int64(42)})
	if a.Hash() != b.Hash() || b.Hash() != c.Hash() {
		t.Errorf("numeric normalization broken: int=%q int32=%q int64=%q", a.Hash(), b.Hash(), c.Hash())
	}
}

// Negative — RebuildHash is unaffected by ArtifactHash-only changes.

func TestRebuildHash_StableUnderIndexChanges(t *testing.T) {
	a := View("u")
	b := View("u").Indexes(Index("email").Unique())
	c := View("u").Indexes(Index("email").Unique(), Compound("a", "b"))
	if a.RebuildHash() != b.RebuildHash() {
		t.Errorf("RebuildHash changed when adding an index (should be ArtifactHash-only): %q vs %q", a.RebuildHash(), b.RebuildHash())
	}
	if b.RebuildHash() != c.RebuildHash() {
		t.Errorf("RebuildHash changed when adding more indexes: %q vs %q", b.RebuildHash(), c.RebuildHash())
	}
}

// Version participates in RebuildHash — bumping the version always produces
// a new hash even if every other declarative field is identical. Closes the
// loop between "developer signals intent via Version(N)" and "framework
// detects the intent via the hash". See tasks/mongo_schema_evolution_2.md §8.3.

func TestRebuildHash_VersionBumpChangesHash(t *testing.T) {
	a := View("users").Version(1)
	b := View("users").Version(2)
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite Version bump 1 → 2")
	}
}

func TestRebuildHash_VersionParticipates_ArtifactStable(t *testing.T) {
	a := View("users").Version(1).Indexes(Index("email").Unique())
	b := View("users").Version(2).Indexes(Index("email").Unique())
	if a.ArtifactHash() != b.ArtifactHash() {
		t.Error("ArtifactHash changed across Version bump — version belongs to RebuildHash partition, not ArtifactHash")
	}
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite Version bump")
	}
}

func TestRebuildHash_SameVersionSameSpec(t *testing.T) {
	a := View("users").Version(3).
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	b := View("users").Version(3).
		EmbedMany("addresses", pgEmbed("addresses", "user_id"))
	if a.RebuildHash() != b.RebuildHash() {
		t.Errorf("RebuildHash not deterministic across instances with same version + spec: %q vs %q", a.RebuildHash(), b.RebuildHash())
	}
}

// ValidateMongoSpec rejects views without Version(N) — the framework
// guarantees no view reaches the runtime without an explicit version.

func TestValidateMongoSpec_RejectsMissingVersion(t *testing.T) {
	v := View("users")
	if err := v.ValidateMongoSpec(); err == nil {
		t.Fatal("expected error: view without Version(N) must be rejected")
	}
}

func TestValidateMongoSpec_RejectsNegativeVersion(t *testing.T) {
	v := View("users").Version(-1)
	if err := v.ValidateMongoSpec(); err == nil {
		t.Fatal("expected error: negative Version must be rejected")
	}
}

func TestValidateMongoSpec_AcceptsPositiveVersion(t *testing.T) {
	v := View("users").Version(1)
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("unexpected error for valid Version(1): %v", err)
	}
}

// ─── projected schema shape participates in RebuildHash ──────────────────────
//
// Internal data (root columns / siblings / SharedBase / children) auto-projects
// from the write TableSchema with no embed, so a change to that closure must move
// the RebuildHash — otherwise the forgot-to-bump guard never fires and the Mongo
// projection silently goes stale on a schema change with no version bump.

type renameA struct{ Title string }
type renameB struct{ Heading string }

func TestRebuildHash_SchemaColumnAddition(t *testing.T) {
	a := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Name", "name"))
	b := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Name", "name").Field("Email", "email"))
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite an added schema column")
	}
}

func TestRebuildHash_SchemaChildAddition(t *testing.T) {
	a := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Name", "name"))
	b := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Name", "name").
			Child(core.NewTableSchema[expAddr]("addresses").PK("id").FK("user_id").Field("ZipCode", "zip")))
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite an added .Child (own children auto-project, no embed)")
	}
}

func TestRebuildHash_SchemaSiblingAddition(t *testing.T) {
	a := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Name", "name"))
	b := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Name", "name").
			Sibling(core.NewSiblingSchema[*expUser]("users_ext").Field("Phone", "phone")))
	if a.RebuildHash() == b.RebuildHash() {
		t.Error("RebuildHash same despite an added sibling")
	}
}

func TestRebuildHash_SchemaFieldReorderStable(t *testing.T) {
	a := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Name", "name").Field("Email", "email"))
	b := View("users").Schema(
		core.NewTableSchema[*expUser]("users").PK("id").Field("Email", "email").Field("Name", "name"))
	if a.RebuildHash() != b.RebuildHash() {
		t.Error("RebuildHash must be stable across field declaration order (columns are sorted)")
	}
}

func TestRebuildHash_GoRenameSameColumnStable(t *testing.T) {
	// Two entities whose different Go field names map to the SAME column: the
	// projected document is identical, so the hash must not move (column-granular).
	a := View("users").Schema(
		core.NewTableSchema[renameA]("users").PK("id").Field("Title", "label"))
	b := View("users").Schema(
		core.NewTableSchema[renameB]("users").PK("id").Field("Heading", "label"))
	if a.RebuildHash() != b.RebuildHash() {
		t.Error("a Go-only rename that keeps the same column must not change RebuildHash")
	}
}
