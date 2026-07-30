package mongo

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Tests in this file cover the pure-logic helpers of mongo_view_reader.go —
// stable sort assembly, keyset filter cascade, cursor encode/decode round-trip
// against doc paths, projection auto-include, doc-path delete. No Mongo
// container is needed; the integration coverage against a real driver lives
// in integration_mongo_view_reader_test.go (//go:build integration).

// ---------------------------------------------------------------------------
// buildStableSortDoc — _id is always the final tiebreaker; reverse flips
// every direction so the backward path can query Mongo with inverted order
// and the reader restores canonical order by reversing the slice in Go.
// ---------------------------------------------------------------------------

func TestBuildStableSortDoc_NoCustomSort_AppendsIDAsc(t *testing.T) {
	got := buildStableSortDoc(nil, false)
	want := bson.D{{Key: "_id", Value: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stable sort doc mismatch: got %#v, want %#v", got, want)
	}
}

func TestBuildStableSortDoc_SingleAsc(t *testing.T) {
	got := buildStableSortDoc([]queries.SortField{{Field: "name"}}, false)
	want := bson.D{
		{Key: "name", Value: 1},
		{Key: "_id", Value: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildStableSortDoc_SingleDesc(t *testing.T) {
	got := buildStableSortDoc([]queries.SortField{{Field: "created_at", Desc: true}}, false)
	want := bson.D{
		{Key: "created_at", Value: -1},
		{Key: "_id", Value: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildStableSortDoc_ReverseInvertsAllDirections(t *testing.T) {
	got := buildStableSortDoc([]queries.SortField{
		{Field: "name"},
		{Field: "created_at", Desc: true},
	}, true)
	want := bson.D{
		{Key: "name", Value: -1},
		{Key: "created_at", Value: 1},
		{Key: "_id", Value: -1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// ---------------------------------------------------------------------------
// buildKeysetFilter — forward / backward; ASC / DESC mix; single + multi
// sort. The trailing _id slot is treated as ASC regardless of sort
// directions; the global direction (+1 forward / -1 backward) governs the
// outer flip.
// ---------------------------------------------------------------------------

func TestBuildKeysetFilter_NoSort_OnlyID_Forward(t *testing.T) {
	got := buildKeysetFilter([]any{"id-1"}, nil, +1)
	// 1 arm: _id > id-1
	want := bson.M{"$or": bson.A{
		bson.M{"_id": bson.M{"$gt": "id-1"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildKeysetFilter_NoSort_OnlyID_Backward(t *testing.T) {
	got := buildKeysetFilter([]any{"id-1"}, nil, -1)
	want := bson.M{"$or": bson.A{
		bson.M{"_id": bson.M{"$lt": "id-1"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildKeysetFilter_SingleSortAsc_Forward(t *testing.T) {
	got := buildKeysetFilter(
		[]any{"Bob", "id-7"},
		[]queries.SortField{{Field: "name"}},
		+1,
	)
	// Arms: (name > Bob) OR (name = Bob AND _id > id-7)
	want := bson.M{"$or": bson.A{
		bson.M{"name": bson.M{"$gt": "Bob"}},
		bson.M{"name": "Bob", "_id": bson.M{"$gt": "id-7"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildKeysetFilter_SingleSortDesc_Forward(t *testing.T) {
	// Forward direction on a DESC sort means "give me docs with name LESS
	// than Bob" (continuing the descending walk).
	got := buildKeysetFilter(
		[]any{"Bob", "id-7"},
		[]queries.SortField{{Field: "name", Desc: true}},
		+1,
	)
	want := bson.M{"$or": bson.A{
		bson.M{"name": bson.M{"$lt": "Bob"}},
		bson.M{"name": "Bob", "_id": bson.M{"$gt": "id-7"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildKeysetFilter_TwoSortsMixedDirection_Forward(t *testing.T) {
	got := buildKeysetFilter(
		[]any{"Bob", "2024-01-01", "id-7"},
		[]queries.SortField{
			{Field: "name"},
			{Field: "created_at", Desc: true},
		},
		+1,
	)
	want := bson.M{"$or": bson.A{
		bson.M{"name": bson.M{"$gt": "Bob"}},
		bson.M{"name": "Bob", "created_at": bson.M{"$lt": "2024-01-01"}},
		bson.M{"name": "Bob", "created_at": "2024-01-01", "_id": bson.M{"$gt": "id-7"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildKeysetFilter_SingleSortAsc_Backward(t *testing.T) {
	got := buildKeysetFilter(
		[]any{"Bob", "id-7"},
		[]queries.SortField{{Field: "name"}},
		-1,
	)
	// Backward on ASC → name < Bob OR (name = Bob AND _id < id-7).
	want := bson.M{"$or": bson.A{
		bson.M{"name": bson.M{"$lt": "Bob"}},
		bson.M{"name": "Bob", "_id": bson.M{"$lt": "id-7"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// ---------------------------------------------------------------------------
// appendKeysetClause — merges the $or cascade with whatever pre-existing
// filter state the wrapper already produced.
// ---------------------------------------------------------------------------

func TestAppendKeysetClause_EmptyFilter_LiftsToTop(t *testing.T) {
	filter := bson.M{}
	keyset := bson.M{"$or": bson.A{bson.M{"_id": bson.M{"$gt": "x"}}}}
	appendKeysetClause(filter, keyset)
	if _, ok := filter["$or"]; !ok {
		t.Fatalf("expected $or to land at top level: %#v", filter)
	}
}

func TestAppendKeysetClause_PreservesExistingAnd(t *testing.T) {
	existing := bson.A{bson.M{"name": "Alice"}}
	filter := bson.M{"$and": existing}
	keyset := bson.M{"$or": bson.A{bson.M{"_id": bson.M{"$gt": "x"}}}}
	appendKeysetClause(filter, keyset)
	arr, ok := filter["$and"].(bson.A)
	if !ok {
		t.Fatalf("expected filter[$and] to be bson.A, got %T", filter["$and"])
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 $and clauses, got %d", len(arr))
	}
}

// ---------------------------------------------------------------------------
// projectionAutoIncluded + buildProjection — sort fields not in the user's
// ?fields= projection get transparently re-included so the cursor builder
// can read their values; the reader strips them from the returned doc.
// ---------------------------------------------------------------------------

func TestProjectionAutoIncluded_NoUserProjection_ReturnsNil(t *testing.T) {
	got := projectionAutoIncluded(nil, []queries.SortField{{Field: "name"}})
	if got != nil {
		t.Fatalf("want nil when no user projection, got %#v", got)
	}
}

func TestProjectionAutoIncluded_SortFieldAlreadyIncluded_NoAdd(t *testing.T) {
	userProj := map[string]int{"name": 1, "_id": 0}
	got := projectionAutoIncluded(userProj, []queries.SortField{{Field: "name"}})
	if len(got) != 0 {
		t.Fatalf("want empty when sort field already in projection, got %#v", got)
	}
}

func TestProjectionAutoIncluded_SortFieldMissing_AddedToList(t *testing.T) {
	userProj := map[string]int{"email": 1, "_id": 0}
	got := projectionAutoIncluded(userProj, []queries.SortField{{Field: "name"}})
	want := []string{"name"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildProjection_DeterministicKeyOrder(t *testing.T) {
	userProj := map[string]int{"zeta": 1, "alpha": 1, "_id": 0}
	got := buildProjection(userProj, nil)
	wantKeys := []string{"_id", "alpha", "zeta"}
	if len(got) != len(wantKeys) {
		t.Fatalf("len mismatch: %d vs %d", len(got), len(wantKeys))
	}
	for i, k := range wantKeys {
		if got[i].Key != k {
			t.Fatalf("ordering broken at [%d]: got %q, want %q (full=%#v)",
				i, got[i].Key, k, got)
		}
	}
}

func TestBuildProjection_AutoIncludeAppendsAfterUserKeys(t *testing.T) {
	userProj := map[string]int{"name": 1, "_id": 0}
	got := buildProjection(userProj, []string{"created_at"})
	// User keys come first (sorted): _id, name. Auto-included after.
	if len(got) != 3 {
		t.Fatalf("want 3 keys, got %d (%#v)", len(got), got)
	}
	if got[2].Key != "created_at" || got[2].Value != 1 {
		t.Fatalf("auto-included key missing or wrong value: %#v", got[2])
	}
}

// ---------------------------------------------------------------------------
// childDeletedAtAutoIncludes — a STRICT-SUBFIELD child projection re-includes
// the child DeletedAt column so the archived strip can see it; a WHOLE-field
// child projection must NOT (the object already carries the column, and adding
// its subpath collides at Mongo, Location31249). Regression guard for the
// ?fields=<whole child segment> 500.
// ---------------------------------------------------------------------------

func TestChildDeletedAtAutoIncludes_StrictSubfield_ReIncludesColumn(t *testing.T) {
	colProj := map[string]int{"Addresses.city": 1, "_id": 0}
	sdPaths := map[string]string{"Addresses": "deleted_at"}
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths)
	if !reflect.DeepEqual(auto, []string{"Addresses.deleted_at"}) {
		t.Fatalf("want [Addresses.deleted_at], got %#v", auto)
	}
	if cleanup["Addresses"] != "deleted_at" {
		t.Fatalf("want cleanup Addresses->deleted_at, got %#v", cleanup)
	}
}

func TestChildDeletedAtAutoIncludes_WholeField_SkipsToAvoidCollision(t *testing.T) {
	// ?fields=addresses → the whole "Addresses" segment. The stored object
	// already carries deleted_at; re-including "Addresses.deleted_at" would make
	// Mongo reject the projection (Location31249 "Path collision"). Nothing is
	// added, nothing is scheduled for cleanup.
	colProj := map[string]int{"Addresses": 1, "_id": 0}
	sdPaths := map[string]string{"Addresses": "deleted_at"}
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths)
	if len(auto) != 0 {
		t.Fatalf("whole-field projection must add nothing, got %#v", auto)
	}
	if len(cleanup) != 0 {
		t.Fatalf("whole-field projection must schedule no cleanup, got %#v", cleanup)
	}
}

func TestChildDeletedAtAutoIncludes_UntouchedChild_Ignored(t *testing.T) {
	colProj := map[string]int{"name": 1, "_id": 0}
	sdPaths := map[string]string{"Addresses": "deleted_at"}
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths)
	if len(auto) != 0 || len(cleanup) != 0 {
		t.Fatalf("a child the projection does not touch must be ignored, got auto=%#v cleanup=%#v", auto, cleanup)
	}
}

// ---------------------------------------------------------------------------
// lookupDocPath + deleteDocPath — flat and nested doc support.
// ---------------------------------------------------------------------------

func TestLookupDocPath_TopLevel(t *testing.T) {
	doc := map[string]any{"name": "Alice"}
	if got := lookupDocPath(doc, "name"); got != "Alice" {
		t.Fatalf("got %v, want Alice", got)
	}
}

func TestLookupDocPath_Nested(t *testing.T) {
	doc := map[string]any{
		"meta": map[string]any{"version": int64(7)},
	}
	if got := lookupDocPath(doc, "meta.version"); got != int64(7) {
		t.Fatalf("got %v, want 7", got)
	}
}

func TestLookupDocPath_MissingReturnsNil(t *testing.T) {
	doc := map[string]any{"a": 1}
	if got := lookupDocPath(doc, "b"); got != nil {
		t.Fatalf("want nil for missing key, got %v", got)
	}
}

func TestDeleteDocPath_TopLevel(t *testing.T) {
	doc := map[string]any{"a": 1, "b": 2}
	deleteDocPath(doc, "a")
	if _, ok := doc["a"]; ok {
		t.Fatalf("a should be deleted: %#v", doc)
	}
	if doc["b"] != 2 {
		t.Fatalf("b should survive: %#v", doc)
	}
}

func TestDeleteDocPath_Nested(t *testing.T) {
	doc := map[string]any{
		"meta": map[string]any{"version": 7, "kind": "x"},
	}
	deleteDocPath(doc, "meta.version")
	inner := doc["meta"].(map[string]any)
	if _, ok := inner["version"]; ok {
		t.Fatalf("meta.version should be deleted: %#v", inner)
	}
	if inner["kind"] != "x" {
		t.Fatalf("meta.kind should survive: %#v", inner)
	}
}

// ---------------------------------------------------------------------------
// resolveMaxLimit — per-view > yaml > framework default 100.
// ---------------------------------------------------------------------------

func TestResolveMaxLimit_FrameworkDefault_WhenNoResolver(t *testing.T) {
	r := &MongoViewReader{}
	if got := r.resolveMaxLimit("users"); got != FrameworkDefaultMaxReadLimit {
		t.Fatalf("got %d, want %d", got, FrameworkDefaultMaxReadLimit)
	}
}

func TestResolveMaxLimit_ResolverReturnsZero_DefaultsToFramework(t *testing.T) {
	r := &MongoViewReader{maxLimitFn: func(string) int64 { return 0 }}
	if got := r.resolveMaxLimit("users"); got != FrameworkDefaultMaxReadLimit {
		t.Fatalf("got %d, want %d", got, FrameworkDefaultMaxReadLimit)
	}
}

func TestResolveMaxLimit_ResolverWins(t *testing.T) {
	r := &MongoViewReader{maxLimitFn: func(v string) int64 {
		if v == "users" {
			return 250
		}
		return 0
	}}
	if got := r.resolveMaxLimit("users"); got != 250 {
		t.Fatalf("got %d, want 250", got)
	}
	if got := r.resolveMaxLimit("other"); got != FrameworkDefaultMaxReadLimit {
		t.Fatalf("got %d, want framework default", got)
	}
}

// ---------------------------------------------------------------------------
// core.LimitExceededError — shape of the typed 400 envelope produced when ?limit=
// exceeds the resolved ceiling.
// ---------------------------------------------------------------------------

func TestLimitExceededError_CarriesSchemaContextAndFieldValue(t *testing.T) {
	err := core.LimitExceededError(250)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	contexts := err.NotificationContexts()
	if len(contexts) != 1 {
		t.Fatalf("want 1 context, got %d", len(contexts))
	}
	msgs := contexts[0].Messages()
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if _, ok := msgs[0].Notification.(domain.LimitExceededNotification); !ok {
		t.Fatalf("want LimitExceededNotification, got %T", msgs[0].Notification)
	}
	if msgs[0].FieldName != "limit" {
		t.Fatalf("FieldName: got %q, want %q", msgs[0].FieldName, "limit")
	}
	if msgs[0].FieldValue != "250" {
		t.Fatalf("FieldValue: got %q, want %q", msgs[0].FieldValue, "250")
	}
}

// ---------------------------------------------------------------------------
// ViewDefinition.MaxLimit — does not participate in RebuildHash / ArtifactHash.
// ---------------------------------------------------------------------------

func TestViewDefinition_MaxLimit_DoesNotChangeRebuildHash(t *testing.T) {
	base := query.View("users").Version(1)
	bumped := query.View("users").Version(1).MaxLimit(500)
	if base.RebuildHash() != bumped.RebuildHash() {
		t.Fatalf("MaxLimit must not affect RebuildHash; base=%s bumped=%s",
			base.RebuildHash(), bumped.RebuildHash())
	}
}

func TestViewDefinition_MaxLimit_DoesNotChangeArtifactHash(t *testing.T) {
	base := query.View("users").Version(1)
	bumped := query.View("users").Version(1).MaxLimit(500)
	if base.ArtifactHash() != bumped.ArtifactHash() {
		t.Fatalf("MaxLimit must not affect ArtifactHash; base=%s bumped=%s",
			base.ArtifactHash(), bumped.ArtifactHash())
	}
}

func TestViewDefinition_MaxLimit_PreservesValue(t *testing.T) {
	v := query.View("users").MaxLimit(500)
	if v.MaxLimitValue() != 500 {
		t.Fatalf("want 500, got %d", v.MaxLimitValue())
	}
}

// Ensure the core.LimitExceededError implements NotificationCarrier (via the
// embedded *core.InfrastructureError). Defense against accidental breakage of the
// Pipeline contract.
func TestLimitExceededError_IsNotificationCarrier(t *testing.T) {
	var carrier domain.NotificationCarrier
	err := error(core.LimitExceededError(100))
	if !errors.As(err, &carrier) {
		t.Fatalf("core.LimitExceededError should implement NotificationCarrier")
	}
	if len(carrier.NotificationContexts()) == 0 {
		t.Fatalf("expected non-empty NotificationContexts")
	}
}
