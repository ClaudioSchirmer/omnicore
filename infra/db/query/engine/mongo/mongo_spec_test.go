package mongo

import (
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// The apply step's pure logic (option-building, divergence detection,
// index-name derivation, error classification) is covered here without
// an active Mongo connection. The IO sequence (listCollections →
// CreateCollection / collMod / createIndexes) is exercised by the
// omnicore-example-users E2E suite once Phase E declares a TextIndex
// on UserView and the framework boot drives the apply step end-to-end
// against the docker-compose Mongo.

// ─── derivedName ─────────────────────────────────────────────────────────

func TestDeriveIndexName_SingleAscending(t *testing.T) {
	got := derivedName(query.Index("email"))
	if got != "email_1" {
		t.Errorf("derivedName = %q, want %q", got, "email_1")
	}
}

func TestDeriveIndexName_Descending(t *testing.T) {
	got := derivedName(query.Index("created_at").Desc())
	if got != "created_at_-1" {
		t.Errorf("derivedName = %q, want %q", got, "created_at_-1")
	}
}

func TestDeriveIndexName_Compound(t *testing.T) {
	got := derivedName(query.Compound("email", "created_at").Desc())
	if got != "email_1_created_at_-1" {
		t.Errorf("derivedName = %q, want %q", got, "email_1_created_at_-1")
	}
}

func TestDeriveIndexName_Text(t *testing.T) {
	got := derivedName(query.TextIndex("name", "email"))
	if got != "name_text_email_text" {
		t.Errorf("derivedName = %q, want %q", got, "name_text_email_text")
	}
}

func TestDeriveIndexName_GeoAndHashed(t *testing.T) {
	if got := derivedName(query.GeoIndex("location")); got != "location_2dsphere" {
		t.Errorf("geo derivedName = %q", got)
	}
	if got := derivedName(query.Index("user_id").Hashed()); got != "user_id_hashed" {
		t.Errorf("hashed derivedName = %q", got)
	}
}

func TestDeriveIndexName_ExplicitNameWins(t *testing.T) {
	got := derivedName(query.Index("email").Name("custom_idx"))
	if got != "custom_idx" {
		t.Errorf("explicit name not honored: %q", got)
	}
}

// ─── isIndexConflict ─────────────────────────────────────────────────────────

func TestIsIndexConflict_OptionsConflict(t *testing.T) {
	err := mongo.CommandError{Code: mongoErrIndexOptionsConflict, Message: "options conflict"}
	if !isIndexConflict(err) {
		t.Error("isIndexConflict(IndexOptionsConflict) = false, want true")
	}
}

func TestIsIndexConflict_KeySpecsConflict(t *testing.T) {
	err := mongo.CommandError{Code: mongoErrIndexKeySpecsConflict, Message: "key specs conflict"}
	if !isIndexConflict(err) {
		t.Error("isIndexConflict(IndexKeySpecsConflict) = false, want true")
	}
}

func TestIsIndexConflict_OtherCommandErrorRejected(t *testing.T) {
	err := mongo.CommandError{Code: 11000, Message: "duplicate key"}
	if isIndexConflict(err) {
		t.Error("isIndexConflict(11000 dup-key) = true, want false")
	}
}

func TestIsIndexConflict_NonCommandError(t *testing.T) {
	if isIndexConflict(errors.New("network down")) {
		t.Error("isIndexConflict(plain error) = true, want false")
	}
	if isIndexConflict(nil) {
		t.Error("isIndexConflict(nil) = true, want false")
	}
}

// ─── buildCreateCollectionOptions ────────────────────────────────────────────

// The framework only confirms that each declared piece is forwarded onto
// the driver's builder. The wire-level "is this what Mongo actually
// received?" check lives in the E2E suite; here we verify that absence
// of a declaration produces a no-op builder (cheap idempotency proof).

func TestBuildCreateCollectionOptions_EmptySpec_NonNil(t *testing.T) {
	v := query.View("x").Root("x")
	opts := buildCreateCollectionOptions(v)
	if opts == nil {
		t.Fatal("builder returned nil on empty spec")
	}
}

func TestBuildCreateCollectionOptions_FullSpec_BuildsClean(t *testing.T) {
	v := query.View("x").Root("x").
		Collation(&query.CollationSpec{Locale: "pt", Strength: 1}).
		JSONSchema(bson.M{"bsonType": "object"})
	opts := buildCreateCollectionOptions(v)
	if opts == nil {
		t.Fatal("builder returned nil on full spec")
	}
	// We do not introspect the private setter slice — the driver owns
	// that shape and asserting on it would couple us to its internals.
	// The wire-level guarantee is exercised by E2E.
}

// ─── buildValidatorCommand ───────────────────────────────────────────────────

func TestBuildValidatorCommand_DefaultLevelAction(t *testing.T) {
	v := query.View("users").Root("users").
		JSONSchema(bson.M{"bsonType": "object"})
	cmd := buildValidatorCommand(v, v.Name())
	if len(cmd) < 4 {
		t.Fatalf("cmd length = %d, want 4 (collMod + validator + level + action)", len(cmd))
	}
	if cmd[0].Key != "collMod" || cmd[0].Value != "users" {
		t.Errorf("cmd[0] = %+v, want {collMod, users}", cmd[0])
	}
	if cmd[1].Key != "validator" {
		t.Errorf("cmd[1].Key = %q, want %q", cmd[1].Key, "validator")
	}
	if cmd[2].Key != "validationLevel" || cmd[2].Value != query.ValidationLevelStrict {
		t.Errorf("cmd[2] = %+v, want {validationLevel, strict}", cmd[2])
	}
	if cmd[3].Key != "validationAction" || cmd[3].Value != query.ValidationActionError {
		t.Errorf("cmd[3] = %+v, want {validationAction, error}", cmd[3])
	}
}

func TestBuildValidatorCommand_OverrideLevelAndAction(t *testing.T) {
	v := query.View("users").Root("users").
		JSONSchema(bson.M{"bsonType": "object"}).
		JSONSchemaValidationLevel(query.ValidationLevelModerate).
		JSONSchemaValidationAction(query.ValidationActionWarn)
	cmd := buildValidatorCommand(v, v.Name())
	if cmd[2].Value != query.ValidationLevelModerate {
		t.Errorf("validationLevel = %v, want %q", cmd[2].Value, query.ValidationLevelModerate)
	}
	if cmd[3].Value != query.ValidationActionWarn {
		t.Errorf("validationAction = %v, want %q", cmd[3].Value, query.ValidationActionWarn)
	}
}

func TestBuildValidatorCommand_PayloadWrapsJSONSchema(t *testing.T) {
	schema := bson.M{"required": []string{"_id", "name"}}
	v := query.View("users").Root("users").JSONSchema(schema)
	cmd := buildValidatorCommand(v, v.Name())
	wrapper, ok := cmd[1].Value.(bson.M)
	if !ok {
		t.Fatalf("validator value type = %T, want bson.M", cmd[1].Value)
	}
	inner, ok := wrapper["$jsonSchema"].(bson.M)
	if !ok {
		t.Fatalf("$jsonSchema type = %T, want bson.M", wrapper["$jsonSchema"])
	}
	if got, want := inner["required"], schema["required"]; got == nil {
		t.Errorf("required field stripped: got %v, want %v", got, want)
	}
}

// ─── collationDivergence ─────────────────────────────────────────────────────

func TestCollationDivergence_BothAbsent_NoDivergence(t *testing.T) {
	if diag := collationDivergence(nil, bson.M{}); diag != "" {
		t.Errorf("got %q, want empty", diag)
	}
}

func TestCollationDivergence_DeclaredButAbsent(t *testing.T) {
	diag := collationDivergence(&query.CollationSpec{Locale: "pt"}, bson.M{})
	if !strings.Contains(diag, "declared but absent") {
		t.Errorf("got %q, want \"declared but absent\" diagnostic", diag)
	}
}

func TestCollationDivergence_PresentButUndeclared(t *testing.T) {
	diag := collationDivergence(nil, bson.M{"collation": bson.M{"locale": "en"}})
	if !strings.Contains(diag, "absent from declaration") {
		t.Errorf("got %q, want \"absent from declaration\" diagnostic", diag)
	}
}

func TestCollationDivergence_LocaleMismatch(t *testing.T) {
	diag := collationDivergence(
		&query.CollationSpec{Locale: "pt"},
		bson.M{"collation": bson.M{"locale": "en"}},
	)
	if !strings.Contains(diag, "locale") {
		t.Errorf("got %q, want locale diagnostic", diag)
	}
}

func TestCollationDivergence_StrengthMismatch(t *testing.T) {
	diag := collationDivergence(
		&query.CollationSpec{Locale: "pt", Strength: 1},
		bson.M{"collation": bson.M{"locale": "pt", "strength": int32(3)}},
	)
	if !strings.Contains(diag, "strength") {
		t.Errorf("got %q, want strength diagnostic", diag)
	}
}

func TestCollationDivergence_StrengthInt64Accepted(t *testing.T) {
	// listCollections may return strength as int32 OR int64 depending on
	// BSON encoding. readInt32 normalizes; same observed value must NOT
	// surface as divergence.
	diag := collationDivergence(
		&query.CollationSpec{Locale: "pt", Strength: 1},
		bson.M{"collation": bson.M{"locale": "pt", "strength": int64(1)}},
	)
	if diag != "" {
		t.Errorf("got %q, want empty on matching int64 strength", diag)
	}
}

func TestCollationDivergence_DeclaredFieldsOnly(t *testing.T) {
	// Mongo populates default fields (caseFirst, normalization, etc.)
	// the consumer never declared. Those defaults must NOT register as
	// divergence — declaration is forward-only.
	diag := collationDivergence(
		&query.CollationSpec{Locale: "pt", Strength: 1},
		bson.M{"collation": bson.M{
			"locale": "pt", "strength": int32(1),
			"caseFirst": "off", "normalization": false, "version": "57.1",
		}},
	)
	if diag != "" {
		t.Errorf("got %q, want empty when extra observed fields are defaults", diag)
	}
}

// ─── cappedDivergence ────────────────────────────────────────────────────────

func TestCappedDivergence_BothAbsent(t *testing.T) {
	if diag := cappedDivergence(nil, bson.M{}); diag != "" {
		t.Errorf("got %q", diag)
	}
}

func TestCappedDivergence_DeclaredButUncapped(t *testing.T) {
	diag := cappedDivergence(&query.CappedSpec{SizeBytes: 1024}, bson.M{})
	if !strings.Contains(diag, "Capped declared") {
		t.Errorf("got %q", diag)
	}
}

func TestCappedDivergence_CappedButUndeclared(t *testing.T) {
	diag := cappedDivergence(nil, bson.M{"capped": true})
	if !strings.Contains(diag, "capped but declaration is not") {
		t.Errorf("got %q", diag)
	}
}

func TestCappedDivergence_SizeMismatch(t *testing.T) {
	diag := cappedDivergence(
		&query.CappedSpec{SizeBytes: 1024},
		bson.M{"capped": true, "size": int64(2048)},
	)
	if !strings.Contains(diag, "SizeBytes") {
		t.Errorf("got %q", diag)
	}
}

func TestCappedDivergence_MaxDocsMismatch(t *testing.T) {
	diag := cappedDivergence(
		&query.CappedSpec{SizeBytes: 1024, MaxDocs: 100},
		bson.M{"capped": true, "size": int64(1024), "max": int64(50)},
	)
	if !strings.Contains(diag, "MaxDocs") {
		t.Errorf("got %q", diag)
	}
}

func TestCappedDivergence_MaxDocsOptional(t *testing.T) {
	// Declared MaxDocs == 0 → framework doesn't probe observed.max.
	diag := cappedDivergence(
		&query.CappedSpec{SizeBytes: 1024},
		bson.M{"capped": true, "size": int64(1024), "max": int64(50)},
	)
	if diag != "" {
		t.Errorf("got %q, want empty when MaxDocs not declared", diag)
	}
}

// ─── timeSeriesDivergence ────────────────────────────────────────────────────

func TestTimeSeriesDivergence_BothAbsent(t *testing.T) {
	if diag := timeSeriesDivergence(nil, bson.M{}); diag != "" {
		t.Errorf("got %q", diag)
	}
}

func TestTimeSeriesDivergence_DeclaredButPlainCollection(t *testing.T) {
	diag := timeSeriesDivergence(&query.TimeSeriesSpec{TimeField: "ts"}, bson.M{})
	if !strings.Contains(diag, "TimeSeries declared") {
		t.Errorf("got %q", diag)
	}
}

func TestTimeSeriesDivergence_PresentButUndeclared(t *testing.T) {
	diag := timeSeriesDivergence(nil, bson.M{"timeseries": bson.M{"timeField": "ts"}})
	if !strings.Contains(diag, "time-series but declaration is not") {
		t.Errorf("got %q", diag)
	}
}

func TestTimeSeriesDivergence_TimeFieldMismatch(t *testing.T) {
	diag := timeSeriesDivergence(
		&query.TimeSeriesSpec{TimeField: "ts"},
		bson.M{"timeseries": bson.M{"timeField": "occurred_at"}},
	)
	if !strings.Contains(diag, "TimeField") {
		t.Errorf("got %q", diag)
	}
}

func TestTimeSeriesDivergence_GranularityMismatch(t *testing.T) {
	diag := timeSeriesDivergence(
		&query.TimeSeriesSpec{TimeField: "ts", Granularity: "seconds"},
		bson.M{"timeseries": bson.M{"timeField": "ts", "granularity": "minutes"}},
	)
	if !strings.Contains(diag, "Granularity") {
		t.Errorf("got %q", diag)
	}
}

// ─── assertCollectionMatches composition ─────────────────────────────────────

func TestAssertCollectionMatches_OK(t *testing.T) {
	v := query.View("users").Root("users").
		Collation(&query.CollationSpec{Locale: "pt", Strength: 1})
	observed := bson.M{"options": bson.M{
		"collation": bson.M{"locale": "pt", "strength": int32(1)},
	}}
	if err := assertCollectionMatches(v, observed); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

func TestAssertCollectionMatches_PropagatesCollationDiag(t *testing.T) {
	v := query.View("users").Root("users").
		Collation(&query.CollationSpec{Locale: "pt"})
	observed := bson.M{"options": bson.M{
		"collation": bson.M{"locale": "en"},
	}}
	err := assertCollectionMatches(v, observed)
	if err == nil || !strings.Contains(err.Error(), "users") || !strings.Contains(err.Error(), "locale") {
		t.Errorf("got %v, want diagnostic naming view + locale", err)
	}
}

func TestAssertCollectionMatches_PropagatesCappedDiag(t *testing.T) {
	v := query.View("audit").Root("audit").Capped(&query.CappedSpec{SizeBytes: 1 << 20})
	observed := bson.M{"options": bson.M{
		"capped": true, "size": int64(1 << 30),
	}}
	err := assertCollectionMatches(v, observed)
	if err == nil || !strings.Contains(err.Error(), "SizeBytes") {
		t.Errorf("got %v, want SizeBytes diagnostic", err)
	}
}

func TestAssertCollectionMatches_NoOptionsKey_TreatsAsEmpty(t *testing.T) {
	// Defensive: a listCollections response missing the "options"
	// sub-document (rare, but possible on legacy collections) must NOT
	// be confused with a divergence — empty observed equals no
	// declaration.
	v := query.View("legacy").Root("legacy")
	if err := assertCollectionMatches(v, bson.M{}); err != nil {
		t.Errorf("got %v, want nil when both declaration and observed empty", err)
	}
}

// ─── readInt32 / readInt64 ───────────────────────────────────────────────────

func TestReadInt32_Variants(t *testing.T) {
	cases := []struct {
		in   any
		want int32
	}{
		{int32(7), 7},
		{int64(7), 7},
		{int(7), 7},
		{float64(7), 7},
		{nil, 0},
		{"7", 0}, // not a number → 0
	}
	for _, c := range cases {
		if got := readInt32(c.in); got != c.want {
			t.Errorf("readInt32(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestReadInt64_Variants(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{int64(99), 99},
		{int32(99), 99},
		{int(99), 99},
		{float64(99), 99},
		{nil, 0},
	}
	for _, c := range cases {
		if got := readInt64(c.in); got != c.want {
			t.Errorf("readInt64(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
