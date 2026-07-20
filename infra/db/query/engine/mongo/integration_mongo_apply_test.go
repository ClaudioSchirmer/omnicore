//go:build integration && postgres

package mongo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// --- ApplyMongoSpecs --------------------------------------------------------

func TestApplyMongoSpecs_CreatesCollectionWithIndexes(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	v := query.View("apply_users").Root("apply_users").Version(1).
		Indexes(
			query.Index("email").Unique(),
			query.Index("created_at").Desc(),
		)

	if err := ApplyMongoSpecs(context.Background(), m, []*query.ViewDefinition{v}, testResolver); err != nil {
		t.Fatalf("ApplyMongoSpecs: %v", err)
	}

	// Listing indexes should now include the email_1 index.
	cur, err := m.Collection("apply_users").Indexes().List(context.Background())
	if err != nil {
		t.Fatalf("Indexes().List: %v", err)
	}
	defer cur.Close(context.Background())

	var idxs []bson.M
	if err := cur.All(context.Background(), &idxs); err != nil {
		t.Fatalf("cur.All: %v", err)
	}
	names := map[string]bool{}
	for _, ix := range idxs {
		if name, ok := ix["name"].(string); ok {
			names[name] = true
		}
	}
	if !names["email_1"] {
		t.Errorf("expected email_1 index, got %v", names)
	}
	if !names["created_at_-1"] {
		t.Errorf("expected created_at_-1 index, got %v", names)
	}
}

func TestApplyMongoSpecs_IsIdempotent(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	v := query.View("apply_idem").Root("apply_idem").Version(1).
		Indexes(query.Index("email").Unique())

	for i := 0; i < 3; i++ {
		if err := ApplyMongoSpecs(context.Background(), m, []*query.ViewDefinition{v}, testResolver); err != nil {
			t.Fatalf("ApplyMongoSpecs iter %d: %v", i, err)
		}
	}
}

func TestApplyMongoSpecs_RejectsInvalidView(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	// Missing Version() — ValidateMongoSpec rejects.
	v := query.View("invalid").Root("invalid")
	err := ApplyMongoSpecs(context.Background(), m, []*query.ViewDefinition{v}, testResolver)
	if err == nil {
		t.Fatal("expected ApplyMongoSpecs to reject view without Version()")
	}
}

func TestApplyMongoSpecs_CreatesValidatorOnFreshCollection(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	schema := bson.M{
		"bsonType": "object",
		"required": []string{"_id"},
	}
	v := query.View("apply_validator").Root("apply_validator").Version(1).
		JSONSchema(schema)

	if err := ApplyMongoSpecs(context.Background(), m, []*query.ViewDefinition{v}, testResolver); err != nil {
		t.Fatalf("ApplyMongoSpecs: %v", err)
	}

	// listCollections must echo the validator back.
	cur, err := m.db.ListCollections(context.Background(), bson.M{"name": "apply_validator"})
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	defer cur.Close(context.Background())
	if !cur.Next(context.Background()) {
		t.Fatalf("collection missing")
	}
	var info bson.M
	if err := cur.Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	opts, _ := info["options"].(bson.M)
	if opts == nil || opts["validator"] == nil {
		t.Errorf("expected validator on the created collection, got %+v", info)
	}
}

// --- CheckServiceRegistry -------------------------------------------------

func TestCheckServiceRegistry_HappyPathAndIdempotent(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	v := query.View("my_view").Root("my_view").Version(1)
	if err := ApplyMongoSpecs(context.Background(), m, []*query.ViewDefinition{v}, testResolver); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// First call writes the marker.
	if err := CheckServiceRegistry(context.Background(), m, "svc-a", "prd", []*query.ViewDefinition{v}); err != nil {
		t.Fatalf("CheckServiceRegistry: %v", err)
	}
	// Second call refreshes in place.
	if err := CheckServiceRegistry(context.Background(), m, "svc-a", "prd", []*query.ViewDefinition{v}); err != nil {
		t.Fatalf("CheckServiceRegistry (second call): %v", err)
	}

	// Marker is stored.
	var marker bson.M
	err := m.Collection(RegistryCollectionName).FindOne(context.Background(), bson.M{"_id": "svc-a"}).Decode(&marker)
	if err != nil {
		t.Fatalf("FindOne marker: %v", err)
	}
	if marker["pid"] == nil || marker["host"] == nil {
		t.Errorf("expected pid + host populated: %+v", marker)
	}
}

func TestCheckServiceRegistry_EmptyServiceNameErrors(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	if err := CheckServiceRegistry(context.Background(), m, "", "prd", nil); err == nil {
		t.Error("expected error on empty serviceName")
	}
}

func TestCheckServiceRegistry_DevDowngradesForeignToWarn(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	// Create a collection no view declares.
	if _, err := m.Collection("orphan").InsertOne(context.Background(), bson.M{"a": 1}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	v := query.View("declared").Root("declared").Version(1)
	if err := ApplyMongoSpecs(context.Background(), m, []*query.ViewDefinition{v}, testResolver); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// dev → warn, no error.
	if err := CheckServiceRegistry(context.Background(), m, "svc", "dev", []*query.ViewDefinition{v}); err != nil {
		t.Errorf("dev profile should warn, not error, got %v", err)
	}
}

func TestCheckServiceRegistry_NonDevAbortsOnForeign(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	if _, err := m.Collection("not_declared").InsertOne(context.Background(), bson.M{"a": 1}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	v := query.View("declared").Root("declared").Version(1)
	if err := ApplyMongoSpecs(context.Background(), m, []*query.ViewDefinition{v}, testResolver); err != nil {
		t.Fatalf("apply: %v", err)
	}

	err := CheckServiceRegistry(context.Background(), m, "svc", "prd", []*query.ViewDefinition{v})
	if err == nil {
		t.Fatal("expected non-dev profile to abort on foreign collections")
	}
	if !strings.Contains(err.Error(), "not_declared") {
		t.Errorf("error should mention the foreign collection name, got %v", err)
	}
}

// --- DetectViewDrift (integration end-to-end) ----------------------------

func TestDetectViewDrift_FreshInit(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	v := query.View("drift_users").Root("drift_users").Version(1)
	// The root table exists but is EMPTY — at boot, migrations create it BEFORE
	// drift detection runs (bootstrap: migrations → ApplyMongoSpecs → DetectViewDrift),
	// so DetectViewDrift always probes a present table. No registry row, no Mongo
	// docs, empty SoR → FreshInit.
	if err := pg.Querier().Exec(context.Background(), `CREATE TABLE drift_users (id text)`); err != nil {
		t.Fatalf("create root table: %v", err)
	}
	report, err := query.DetectViewDrift(context.Background(), m, pg, []*query.ViewDefinition{v}, testResolver)
	if err != nil {
		t.Fatalf("DetectViewDrift: %v", err)
	}
	if len(report.Plans) != 1 || report.Plans[0].Decision != query.DriftFreshInit {
		t.Errorf("expected DriftFreshInit, got %+v", report.Plans)
	}
}

func TestDetectViewDrift_NoneAndArtifactOnlyAndAlienData(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	v := query.View("drift_x").Root("drift_x").Version(1).
		Indexes(query.Index("email").Unique())

	// The root table exists but is empty (migrations create it before drift
	// detection at boot); the drift decisions below turn on the registry + Mongo
	// state, not on the SoR having rows.
	if err := pg.Querier().Exec(context.Background(), `CREATE TABLE drift_x (id text)`); err != nil {
		t.Fatalf("create root table: %v", err)
	}

	// First: AlienData — populate Mongo without writing the registry row.
	m.Collection("drift_x").InsertOne(context.Background(), bson.M{"_id": "1"})
	report, err := query.DetectViewDrift(context.Background(), m, pg, []*query.ViewDefinition{v}, testResolver)
	if err != nil {
		t.Fatalf("DetectViewDrift: %v", err)
	}
	if report.Plans[0].Decision != query.DriftAlienData {
		t.Errorf("expected DriftAlienData, got %v", report.Plans[0].Decision)
	}

	// Now write the registry row → DriftNone.
	in := query.InitViewRegistryInput{
		ViewName:     v.Name(),
		Version:      v.VersionNumber(),
		RebuildHash:  v.RebuildHash(),
		ArtifactHash: v.ArtifactHash(),
		CombinedHash: v.Hash(),
		ServiceName:  "svc",
		Now:          time.Now().UTC(),
	}
	if err := query.InitViewRegistry(context.Background(), pg.Querier(), pg.Dialect(), in); err != nil {
		t.Fatalf("Init: %v", err)
	}
	report, _ = query.DetectViewDrift(context.Background(), m, pg, []*query.ViewDefinition{v}, testResolver)
	if report.Plans[0].Decision != query.DriftNone {
		t.Errorf("expected DriftNone after seeding registry, got %v", report.Plans[0].Decision)
	}
}
