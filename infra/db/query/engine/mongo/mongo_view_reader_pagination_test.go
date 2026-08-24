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
	got := buildStableSortDoc([]queries.OrderByField{{Field: "name"}}, false)
	want := bson.D{
		{Key: "name", Value: 1},
		{Key: "_id", Value: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildStableSortDoc_SingleDesc(t *testing.T) {
	got := buildStableSortDoc([]queries.OrderByField{{Field: "created_at", Desc: true}}, false)
	want := bson.D{
		{Key: "created_at", Value: -1},
		{Key: "_id", Value: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildStableSortDoc_ReverseInvertsAllDirections(t *testing.T) {
	got := buildStableSortDoc([]queries.OrderByField{
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
		[]queries.OrderByField{{Field: "name"}},
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
		[]queries.OrderByField{{Field: "name", Desc: true}},
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
		[]queries.OrderByField{
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
		[]queries.OrderByField{{Field: "name"}},
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
	got := projectionAutoIncluded(nil, []queries.OrderByField{{Field: "name"}}, true)
	if got != nil {
		t.Fatalf("want nil when no user projection, got %#v", got)
	}
}

func TestProjectionAutoIncluded_OrderByFieldAlreadyIncluded_NoAdd(t *testing.T) {
	userProj := map[string]int{"name": 1, "_id": 0}
	got := projectionAutoIncluded(userProj, []queries.OrderByField{{Field: "name"}}, true)
	if len(got) != 0 {
		t.Fatalf("want empty when sort field already in projection, got %#v", got)
	}
}

func TestProjectionAutoIncluded_OrderByFieldMissing_AddedToProjectionAndStripList(t *testing.T) {
	userProj := map[string]int{"email": 1, "_id": 0}
	got := projectionAutoIncluded(userProj, []queries.OrderByField{{Field: "name"}}, true)
	want := []string{"name"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if userProj["name"] != 1 {
		t.Fatalf("the sort field must be folded into the projection as an inclusion: %#v", userProj)
	}
}

// EXCLUSION mode: a sort field the projection does not name is ALREADY served,
// so nothing is added — and nothing may be, because `{phone: 0, name: 1}` is
// the mixed projection Mongo refuses outright (Location31253). This is the
// shape ReadCriteria.Restrict produces, so the regression it guards is a
// field-restricted listing with an ?orderBy= failing the whole read.
func TestProjectionAutoIncluded_ExclusionMode_UntouchedSortFieldIsLeftAlone(t *testing.T) {
	userProj := map[string]int{"phone": 0}
	got := projectionAutoIncluded(userProj, []queries.OrderByField{{Field: "name"}}, false)
	if len(got) != 0 {
		t.Fatalf("want nothing auto-included in exclusion mode, got %#v", got)
	}
	if !reflect.DeepEqual(userProj, map[string]int{"phone": 0}) {
		t.Fatalf("the projection must be untouched, got %#v", userProj)
	}
}

// EXCLUSION mode, the other half: a sort field the projection DOES name is
// un-excluded (the only repair the mode allows) and scheduled for the strip.
func TestProjectionAutoIncluded_ExclusionMode_ExcludedSortFieldIsUnExcluded(t *testing.T) {
	userProj := map[string]int{"name": 0}
	got := projectionAutoIncluded(userProj, []queries.OrderByField{{Field: "name"}}, false)
	if !reflect.DeepEqual(got, []string{"name"}) {
		t.Fatalf("want [name] scheduled for the post-cursor strip, got %#v", got)
	}
	if _, still := userProj["name"]; still {
		t.Fatalf("the exclusion must be lifted so the doc carries the sort value: %#v", userProj)
	}
}

func TestBuildProjection_DeterministicKeyOrder(t *testing.T) {
	userProj := map[string]int{"zeta": 1, "alpha": 1, "_id": 0}
	got := buildProjection(userProj)
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

// buildProjection renders the resolved map and nothing else: the auto-includes
// are already folded in with the flag their mode allows, so an exclusion
// projection emits ONLY exclusions.
func TestBuildProjection_ExclusionProjectionStaysSingleMode(t *testing.T) {
	userProj := map[string]int{"phone": 0, "secret": 0}
	got := buildProjection(userProj)
	if len(got) != 2 {
		t.Fatalf("want 2 keys, got %d (%#v)", len(got), got)
	}
	for _, e := range got {
		if e.Value != 0 {
			t.Fatalf("an inclusion leaked into an exclusion projection: %#v", got)
		}
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
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths, true)
	if !reflect.DeepEqual(auto, []string{"Addresses.deleted_at"}) {
		t.Fatalf("want [Addresses.deleted_at], got %#v", auto)
	}
	if cleanup["Addresses"] != "deleted_at" {
		t.Fatalf("want cleanup Addresses->deleted_at, got %#v", cleanup)
	}
	if colProj["Addresses.deleted_at"] != 1 {
		t.Fatalf("the column must be folded into the projection: %#v", colProj)
	}
}

// EXCLUSION mode: narrowing into a segment by DROPPING one of its subfields
// still serves the segment's DeletedAt column, so the strip can already see it
// and nothing is auto-included — `{addresses.ssn: 0, addresses.deleted_at: 1}`
// would be the mixed projection Mongo refuses.
func TestChildDeletedAtAutoIncludes_ExclusionMode_ColumnAlreadyServed(t *testing.T) {
	colProj := map[string]int{"Addresses.ssn": 0}
	sdPaths := map[string]string{"Addresses": "deleted_at"}
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths, false)
	if len(auto) != 0 || len(cleanup) != 0 {
		t.Fatalf("exclusion mode must add nothing, got auto=%#v cleanup=%#v", auto, cleanup)
	}
	if !reflect.DeepEqual(colProj, map[string]int{"Addresses.ssn": 0}) {
		t.Fatalf("the projection must be untouched, got %#v", colProj)
	}
}

// EXCLUSION mode, the other half: an exclusion that names the DeletedAt column
// itself would blind the strip, so the exclusion is lifted and the column is
// scheduled for removal from the served entries.
func TestChildDeletedAtAutoIncludes_ExclusionMode_ExcludedColumnIsUnExcluded(t *testing.T) {
	colProj := map[string]int{"Addresses.deleted_at": 0}
	sdPaths := map[string]string{"Addresses": "deleted_at"}
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths, false)
	if !reflect.DeepEqual(auto, []string{"Addresses.deleted_at"}) {
		t.Fatalf("want [Addresses.deleted_at], got %#v", auto)
	}
	if cleanup["Addresses"] != "deleted_at" {
		t.Fatalf("want cleanup Addresses->deleted_at, got %#v", cleanup)
	}
	if _, still := colProj["Addresses.deleted_at"]; still {
		t.Fatalf("the exclusion must be lifted so the strip can see the column: %#v", colProj)
	}
}

func TestChildDeletedAtAutoIncludes_WholeField_SkipsToAvoidCollision(t *testing.T) {
	// ?fields=addresses → the whole "Addresses" segment. The stored object
	// already carries deleted_at; re-including "Addresses.deleted_at" would make
	// Mongo reject the projection (Location31249 "Path collision"). Nothing is
	// added, nothing is scheduled for cleanup.
	colProj := map[string]int{"Addresses": 1, "_id": 0}
	sdPaths := map[string]string{"Addresses": "deleted_at"}
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths, true)
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
	auto, cleanup := childDeletedAtAutoIncludes(colProj, sdPaths, true)
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
// core.LimitExceededError — shape of the typed 400 envelope produced when the
// requested page size (?first= / ?last=) exceeds the resolved ceiling.
// ---------------------------------------------------------------------------

func TestLimitExceededError_CarriesSchemaContextAndFieldValue(t *testing.T) {
	err := core.LimitExceededError(250, false)
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
	// FieldName is the directional control the consumer sent (forward here).
	if msgs[0].FieldName != "first" {
		t.Fatalf("FieldName: got %q, want %q", msgs[0].FieldName, "first")
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
	err := error(core.LimitExceededError(100, false))
	if !errors.As(err, &carrier) {
		t.Fatalf("core.LimitExceededError should implement NotificationCarrier")
	}
	if len(carrier.NotificationContexts()) == 0 {
		t.Fatalf("expected non-empty NotificationContexts")
	}
}

// ---------------------------------------------------------------------------
// identityAutoIncluded — `_id` is the cursor's absolute tiebreaker, so a
// projection that would drop it gets it back for the query and stripped after.
// The two narrowing modes need OPPOSITE repairs, because `_id` is the one
// field Mongo lets a projection flag against its own mode.
// ---------------------------------------------------------------------------

func TestIdentityAutoIncluded_InclusionWithoutID_FlipsExclusionToInclusion(t *testing.T) {
	// ?fields=name → {name:1, _id:0}. The auto-exclusion has to become an
	// inclusion, which is the only way the column comes back.
	colProj := map[string]int{"name": 1, "_id": 0}
	identityAutoIncluded(colProj, true)
	if colProj["_id"] != 1 {
		t.Fatalf("want _id:1, got %#v", colProj)
	}
}

func TestIdentityAutoIncluded_InclusionSelectingID_NoOp(t *testing.T) {
	colProj := map[string]int{"name": 1, "_id": 1}
	identityAutoIncluded(colProj, true)
	if colProj["_id"] != 1 {
		t.Fatalf("want _id untouched at 1, got %#v", colProj)
	}
}

func TestIdentityAutoIncluded_ExclusionDroppingID_RemovesTheEntry(t *testing.T) {
	// ReadCriteria.Restrict("ID") → {_id:0}. Writing {_id:1} here would be read
	// by Mongo as an inclusion projection of the key ALONE — the whole document
	// would collapse to its id. Dropping the entry restores the default (an
	// exclusion projection returns `_id` unless it says otherwise).
	colProj := map[string]int{"_id": 0}
	identityAutoIncluded(colProj, false)
	if _, still := colProj["_id"]; still {
		t.Fatalf("want the _id entry removed, got %#v", colProj)
	}
}

func TestIdentityAutoIncluded_NoIDEntry_NoOp(t *testing.T) {
	colProj := map[string]int{"phone": 0}
	identityAutoIncluded(colProj, false)
	if len(colProj) != 1 {
		t.Fatalf("projection must be untouched, got %#v", colProj)
	}
}

// ---------------------------------------------------------------------------
// encodeTupleCursor — the trailing slot is the identity, and a doc without one
// has no valid keyset cursor. Stringifying the absent value produced the
// literal "<nil>", which decodes fine and then compares `_id > "<nil>"` — a
// boundary that sits mid-alphabet ('<' is 0x3C, between the digits and the
// letters of a hex UUID), so the walk silently re-served the same row for every
// id sorting below it. Refusing is the honest answer.
// ---------------------------------------------------------------------------

func TestEncodeTupleCursor_CarriesIDAsTrailingTiebreak(t *testing.T) {
	doc := map[string]any{"_id": "a69341e6-dead-beef", "name": "Alpha"}
	got, err := encodeTupleCursor(doc, []queries.OrderByField{{Field: "name"}}, "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := queries.DecodeCursor(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []any{"Alpha", "a69341e6-dead-beef"}
	if !reflect.DeepEqual(dec.K, want) {
		t.Fatalf("tuple: got %#v, want %#v", dec.K, want)
	}
}

func TestEncodeTupleCursor_MissingID_Refuses(t *testing.T) {
	doc := map[string]any{"name": "Alpha"}
	got, err := encodeTupleCursor(doc, []queries.OrderByField{{Field: "name"}}, "")
	if err == nil {
		t.Fatalf("want an error for a doc with no identity, got cursor %q", got)
	}
	if got != "" {
		t.Fatalf("want no cursor emitted, got %q", got)
	}
}

func TestEncodeTupleCursor_NilID_Refuses(t *testing.T) {
	doc := map[string]any{"_id": nil, "name": "Alpha"}
	if _, err := encodeTupleCursor(doc, []queries.OrderByField{{Field: "name"}}, ""); err == nil {
		t.Fatal("a nil identity must not be stringified into a \"<nil>\" tiebreak")
	}
}

// ---------------------------------------------------------------------------
// The identity is `_id` in this store, and it appears in the sort document and
// the keyset cascade exactly ONCE. A consumer that sorts BY the identity has
// named the tiebreaker itself; the reader must not append its own on top.
// ---------------------------------------------------------------------------

func TestBuildStableSortDoc_ConsumerSortsOnIdentity_NoDuplicateKey(t *testing.T) {
	got := buildStableSortDoc([]queries.OrderByField{{Field: "_id", Desc: true}}, false)
	want := bson.D{{Key: "_id", Value: -1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v — the appended tiebreaker would contradict the consumer's own term", got, want)
	}
}

func TestBuildStableSortDoc_IdentityAfterACustomTerm_StillAppearsOnce(t *testing.T) {
	got := buildStableSortDoc([]queries.OrderByField{{Field: "name"}, {Field: "_id", Desc: true}}, false)
	want := bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: -1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// The cascade stops at the identity slot the consumer declared. Emitting the
// trailing arm too wrote an equality AND an inequality on `_id` into the same
// bson.M — the second overwrote the first, and the arm degenerated into
// "everything on the other side of v", which un-bounded the page.
func TestBuildKeysetFilter_ConsumerSortsOnIdentity_CascadeStopsThere(t *testing.T) {
	got := buildKeysetFilter([]any{"v", "v"}, []queries.OrderByField{{Field: "_id", Desc: true}}, 1)
	arms, ok := got["$or"].(bson.A)
	if !ok {
		t.Fatalf("want an $or cascade, got %#v", got)
	}
	if len(arms) != 1 {
		t.Fatalf("want exactly 1 arm (the identity is unique), got %d: %#v", len(arms), arms)
	}
	want := bson.M{"_id": bson.M{"$lt": "v"}}
	if !reflect.DeepEqual(arms[0], want) {
		t.Fatalf("got %#v, want %#v", arms[0], want)
	}
}

func TestBuildKeysetFilter_IdentitySortBehindACustomTerm_TwoArms(t *testing.T) {
	got := buildKeysetFilter(
		[]any{"n", "v", "v"},
		[]queries.OrderByField{{Field: "name"}, {Field: "_id", Desc: true}},
		1,
	)
	arms := got["$or"].(bson.A)
	if len(arms) != 2 {
		t.Fatalf("want 2 arms, got %d: %#v", len(arms), arms)
	}
	if !reflect.DeepEqual(arms[0], bson.M{"name": bson.M{"$gt": "n"}}) {
		t.Fatalf("arm 1: %#v", arms[0])
	}
	if !reflect.DeepEqual(arms[1], bson.M{"name": "n", "_id": bson.M{"$lt": "v"}}) {
		t.Fatalf("arm 2: %#v", arms[1])
	}
}
