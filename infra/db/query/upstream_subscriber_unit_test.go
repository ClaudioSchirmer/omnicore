package query

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/segmentio/kafka-go"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// NOTE on the relational failure registry: the subscriber's recordFailure /
// resolveFailures / RetryPendingFailures reach the backend through its own
// relational-engine handle (s.eng). These tests give the subscriber a nil engine
// handle (the best-effort writers take their nil-guard branch) while still
// composing through a fakeEngine-backed composer for the ripple. The underlying
// SQL helpers (RecordUpstreamFailure / ResolveUpstreamFailures /
// ListPendingUpstreamFailuresByTopic) are covered directly in
// upstream_failures_test.go and coverage_seams_test.go via a core.Querier fake.

// This file drives infra/upstream_subscriber.go (previously ~0%) entirely
// in-process: a real UpstreamSubscriber over a fake postgres.Postgres (NewComposerWithMongo
// root rows + omnicore_upstream_failures writes) and a fake MongoDB whose collFn
// dispatches by collection name (the upstream "users" collection plus the
// dependent view "orders" collection). The methods are invoked directly —
// processMessage / upsertAndRipple / deleteAndRipple / dispatchDelete / ripple /
// recordFailure / resolveFailures / RetryPendingFailures / Shutdown — so the
// control flow runs without a live Kafka reader. Start/run are deliberately not
// exercised here: they block on a real kafka.Reader.ReadMessage and have no seam.

// upstreamFakeMongo dispatches collFn by collection name, returning a fresh empty
// fakeColl for any name not in colls.
func upstreamFakeMongo(colls map[string]*fakeColl) ReadModelStore {
	return newFakeMongoFunc(func(name string) *fakeColl {
		if c, ok := colls[name]; ok {
			return c
		}
		return &fakeColl{}
	})
}

// ordersBuyerView is a B view rooted at "orders" embedding the upstream "users"
// collection one-to-one via an external FromSchema joined on the parent FK
// "buyer_id" — exactly the shape UpstreamSubscriber.ripple recomposes.
func ordersBuyerView() *ViewDefinition {
	external := FromSchema(
		core.NewExternalSchema("users").PK("id").Field("Name", "name")).
		On("buyer_id").As("Buyer")
	return View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		Embed("buyer", external)
}

// ordersRootEngine returns a fakeEngine whose composer root fetch yields the
// order root row carrying the buyer_id FK that points at the upstream doc.
func ordersRootEngine() *fakeEngine {
	return composerEngine(func(string, []any) ([]map[string]any, error) {
		return mapsFromColsData([]string{"id", "buyer_id", "name"}, [][]any{{"o1", "u1", "first"}}), nil
	})
}

// happyColls wires both collections so a full ripple succeeds: "orders" returns
// the local doc id for FindIDsByField and accepts the recompose Upsert; "users"
// supplies the embed doc for Compose and accepts the upstream Upsert.
func happyColls() map[string]*fakeColl {
	return map[string]*fakeColl{
		"orders": {docs: []any{map[string]any{"_id": "o1"}}},
		"users":  {docs: []any{map[string]any{"_id": "u1", "name": "alice"}}},
	}
}

func newTestUpstream(t *testing.T, cfg UpstreamSubscriberConfig, mongo ReadModelStore, eng core.RelationalEngine) *UpstreamSubscriber {
	t.Helper()
	if cfg.Topic == "" {
		cfg.Topic = "users.events"
	}
	if cfg.Collection == "" {
		cfg.Collection = "users"
	}
	// The composer drives Compose's root fetch via the engine seam; the
	// subscriber's own engine handle is nil so the best-effort failure writers
	// no-op (their nil-guard branch — see file header note).
	composer := NewComposerWithMongo(eng, mongo)
	s, err := NewUpstreamSubscriber(nil, mongo, composer, cfg,
		[]*ViewDefinition{ordersBuyerView()}, nil, nil)
	if err != nil {
		t.Fatalf("NewUpstreamSubscriber: %v", err)
	}
	return s
}

func upstreamMsg(id, eventType string, value string) kafka.Message {
	return kafka.Message{
		Key: []byte(id),
		Headers: []kafka.Header{
			{Key: "aggregate_type", Value: []byte("User")},
			{Key: "event_type", Value: []byte(eventType)},
		},
		Value: []byte(value),
	}
}

func TestProcessMessage_Inserted_FullRipple(t *testing.T) {
	colls := happyColls()
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())

	before := UpstreamMessagesProcessed()
	s.processMessage(context.Background(), upstreamMsg("u1", "INSERTED", `{"name":"alice"}`), 0)

	if UpstreamMessagesProcessed() != before+1 {
		t.Error("processMessage must bump the global counter")
	}
	if len(colls["users"].updates) == 0 {
		t.Error("INSERTED must upsert the upstream collection")
	}
	if len(colls["orders"].updates) == 0 {
		t.Error("ripple must recompose+upsert the dependent view")
	}
	if got := s.Metrics().Snapshot(); len(got) != 0 {
		t.Errorf("clean ripple must record no failures, got %v", got)
	}
}

func TestProcessMessage_Updated_WithFilter(t *testing.T) {
	colls := happyColls()
	cfg := UpstreamSubscriberConfig{Filter: []string{"name"}}
	s := newTestUpstream(t, cfg, upstreamFakeMongo(colls), ordersRootEngine())

	s.processMessage(context.Background(), upstreamMsg("u1", "UPDATED", `{"name":"alice","secret":"x"}`), 0)

	if len(colls["users"].updates) != 1 {
		t.Fatalf("expected one upstream upsert, got %d", len(colls["users"].updates))
	}
	set, _ := colls["users"].updates[0]["$set"].(map[string]any)
	if _, ok := set["secret"]; ok {
		t.Error("filter allowlist must drop fields outside the list")
	}
	if _, ok := set["name"]; !ok {
		t.Error("filter allowlist must keep listed fields")
	}
}

func TestProcessMessage_Archived_DeleteOnArchive(t *testing.T) {
	colls := happyColls()
	cfg := UpstreamSubscriberConfig{DeleteOnArchive: true}
	s := newTestUpstream(t, cfg, upstreamFakeMongo(colls), ordersRootEngine())

	s.processMessage(context.Background(), upstreamMsg("u1", "ARCHIVED", `{}`), 0)

	if len(colls["users"].deletes) == 0 {
		t.Error("ARCHIVED + DeleteOnArchive must delete the upstream doc")
	}
}

func TestProcessMessage_Archived_SoftKeep(t *testing.T) {
	colls := happyColls()
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())

	s.processMessage(context.Background(), upstreamMsg("u1", "ARCHIVED", `{"deleted_at":"now"}`), 0)

	if len(colls["users"].updates) == 0 {
		t.Error("ARCHIVED without DeleteOnArchive must upsert (soft) the upstream doc")
	}
	if len(colls["users"].deletes) != 0 {
		t.Error("soft archive must not delete")
	}
}

func TestProcessMessage_Deleted_Cascade(t *testing.T) {
	colls := happyColls()
	s := newTestUpstream(t, UpstreamSubscriberConfig{OnUpstreamDelete: upstreamDeletePolicyCascade},
		upstreamFakeMongo(colls), ordersRootEngine())

	s.processMessage(context.Background(), upstreamMsg("u1", "DELETED", `{}`), 0)

	if len(colls["users"].deletes) == 0 {
		t.Error("DELETED + cascade must delete the upstream doc")
	}
}

func TestProcessMessage_Deleted_Anonymize(t *testing.T) {
	colls := happyColls()
	cfg := UpstreamSubscriberConfig{OnUpstreamDelete: upstreamDeletePolicyAnonymize, AnonymizeFields: []string{"name", "email"}}
	s := newTestUpstream(t, cfg, upstreamFakeMongo(colls), ordersRootEngine())

	s.processMessage(context.Background(), upstreamMsg("u1", "DELETED", `{}`), 0)

	if len(colls["users"].updates) == 0 {
		t.Fatal("anonymize must UpdateFields the upstream doc")
	}
	set, _ := colls["users"].updates[0]["$set"].(map[string]any)
	if _, ok := set["name"]; !ok {
		t.Error("anonymize must blank the declared fields")
	}
	// ripple still runs after anonymize.
	if len(colls["orders"].updates) == 0 {
		t.Error("anonymize must still trigger the recompose ripple")
	}
}

func TestProcessMessage_Deleted_Keep(t *testing.T) {
	colls := happyColls()
	cfg := UpstreamSubscriberConfig{OnUpstreamDelete: upstreamDeletePolicyKeep}
	s := newTestUpstream(t, cfg, upstreamFakeMongo(colls), ordersRootEngine())

	s.processMessage(context.Background(), upstreamMsg("u1", "DELETED", `{}`), 0)

	if len(colls["users"].deletes) != 0 || len(colls["orders"].updates) != 0 {
		t.Error("keep policy must be a pure no-op")
	}
}

func TestProcessMessage_UnknownEventType(t *testing.T) {
	colls := happyColls()
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())
	// Unrecognized verb: logged at info, no Mongo writes.
	s.processMessage(context.Background(), upstreamMsg("u1", "FROBNICATED", `{}`), 0)
	if len(colls["users"].updates) != 0 || len(colls["users"].deletes) != 0 {
		t.Error("unknown event type must not touch Mongo")
	}
}

func TestProcessMessage_IncompleteMetadata(t *testing.T) {
	colls := happyColls()
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())

	// Empty aggregate id (empty Key) → early return.
	s.processMessage(context.Background(), upstreamMsg("", "INSERTED", `{}`), 0)
	// Empty event type (no event_type header) → early return.
	noType := kafka.Message{Key: []byte("u1"), Headers: []kafka.Header{{Key: "aggregate_type", Value: []byte("User")}}, Value: []byte(`{}`)}
	s.processMessage(context.Background(), noType, 0)

	if len(colls["users"].updates) != 0 {
		t.Error("incomplete metadata must short-circuit before any write")
	}
}

func TestProcessMessage_DecodeFailure(t *testing.T) {
	colls := happyColls()
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())
	s.processMessage(context.Background(), upstreamMsg("u1", "INSERTED", `{not json`), 0)
	if len(colls["users"].updates) != 0 {
		t.Error("payload decode failure must short-circuit before any write")
	}
}

func TestUpsertAndRipple_UpstreamUpsertError(t *testing.T) {
	colls := happyColls()
	colls["users"].updateErr = errFake
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())

	s.upsertAndRipple(context.Background(), "u1", bson.M{"name": "alice"})
	if len(colls["orders"].updates) != 0 {
		t.Error("upstream upsert failure must skip the ripple")
	}
}

func TestDeleteAndRipple_DeleteError(t *testing.T) {
	colls := happyColls()
	colls["users"].deleteErr = errFake
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())

	s.deleteAndRipple(context.Background(), "u1")
	if len(colls["orders"].updates) != 0 {
		t.Error("upstream delete failure must skip the ripple")
	}
}

func TestDispatchDelete_AnonymizeUpdateError(t *testing.T) {
	colls := happyColls()
	colls["users"].updateErr = errFake
	cfg := UpstreamSubscriberConfig{OnUpstreamDelete: upstreamDeletePolicyAnonymize, AnonymizeFields: []string{"name"}}
	s := newTestUpstream(t, cfg, upstreamFakeMongo(colls), ordersRootEngine())

	s.dispatchDelete(context.Background(), "u1")
	if len(colls["orders"].updates) != 0 {
		t.Error("anonymize UpdateFields failure must skip the ripple")
	}
}

func TestDispatchDelete_UnknownPolicy(t *testing.T) {
	colls := happyColls()
	// Construct then override to an out-of-set policy (the constructor only
	// defaults the empty string to cascade).
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())
	s.cfg.OnUpstreamDelete = "bogus"
	s.dispatchDelete(context.Background(), "u1")
	if len(colls["users"].deletes) != 0 || len(colls["users"].updates) != 0 {
		t.Error("unknown policy must be a defensive no-op")
	}
}

func TestRipple_DiscoverFailure(t *testing.T) {
	colls := happyColls()
	colls["orders"].findErr = errFake // FindIDsByField fails
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())

	s.ripple(context.Background(), "u1")
	snap := s.Metrics().Snapshot()
	if snap["users.events|orders|discover"] != 1 {
		t.Errorf("discover failure must increment the discover metric, got %v", snap)
	}
}

func TestRipple_ComposeFailure(t *testing.T) {
	colls := happyColls()
	eng := composerEngine(func(string, []any) ([]map[string]any, error) {
		return nil, errFake // Compose root fetch fails
	})
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), eng)

	s.ripple(context.Background(), "u1")
	snap := s.Metrics().Snapshot()
	if snap["users.events|orders|compose"] != 1 {
		t.Errorf("compose failure must increment the compose metric, got %v", snap)
	}
	if len(colls["orders"].updates) != 0 {
		t.Error("compose failure must not upsert the recomposed doc")
	}
}

func TestRipple_UpsertFailure(t *testing.T) {
	colls := happyColls()
	colls["orders"].updateErr = errFake // recompose Upsert fails (Find still OK)
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), ordersRootEngine())

	s.ripple(context.Background(), "u1")
	snap := s.Metrics().Snapshot()
	if snap["users.events|orders|upsert"] != 1 {
		t.Errorf("upsert failure must increment the upsert metric, got %v", snap)
	}
}

func TestRipple_ComposeReturnsNilDoc(t *testing.T) {
	// FindIDsByField yields a local id, but the composer's root fetch finds no
	// row (empty result) → Compose returns (nil, nil) → ripple skips the upsert
	// without recording a failure.
	colls := happyColls()
	eng := composerEngine(func(string, []any) ([]map[string]any, error) {
		return nil, nil // root fetch finds no row → Compose returns (nil, nil)
	})
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(colls), eng)

	s.ripple(context.Background(), "u1")
	if len(colls["orders"].updates) != 0 {
		t.Error("a nil composed doc must skip the recompose upsert")
	}
	if got := s.Metrics().Snapshot(); len(got) != 0 {
		t.Errorf("a nil doc is a skip, not a failure, got %v", got)
	}
}

func TestRipple_NoJoinField(t *testing.T) {
	// A view that does NOT embed the upstream collection → joinFieldFor == ""
	// → ripple logs and skips it (defensive branch).
	other := FromSchema(core.NewExternalSchema("partners").PK("id")).On("partner_id").As("Partner")
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).Embed("partner", other)
	mongo := upstreamFakeMongo(happyColls())
	composer := NewComposerWithMongo(ordersRootEngine(), mongo)
	s, err := NewUpstreamSubscriber(nil, mongo, composer,
		UpstreamSubscriberConfig{Topic: "users.events", Collection: "users"},
		[]*ViewDefinition{view}, nil, nil)
	if err != nil {
		t.Fatalf("NewUpstreamSubscriber: %v", err)
	}
	s.ripple(context.Background(), "u1")
	if got := s.Metrics().Snapshot(); len(got) != 0 {
		t.Errorf("no-join-field view is skipped, not failed, got %v", got)
	}
}

func TestRecordFailure_NilPostgres(t *testing.T) {
	mongo := upstreamFakeMongo(happyColls())
	s, err := NewUpstreamSubscriber(nil, mongo, NewComposerWithMongo(nil, mongo),
		UpstreamSubscriberConfig{Topic: "t", Collection: "users"},
		[]*ViewDefinition{ordersBuyerView()}, nil, nil)
	if err != nil {
		t.Fatalf("NewUpstreamSubscriber: %v", err)
	}
	// Both best-effort writers must no-op (and not panic) without a PG handle.
	s.recordFailure(context.Background(), "orders", "u1", "o1", UpstreamFailureStageCompose, errFake)
	s.resolveFailures(context.Background(), "orders", "u1")
}

func TestRetryPendingFailures_NilPostgres(t *testing.T) {
	mongo := upstreamFakeMongo(happyColls())
	s, err := NewUpstreamSubscriber(nil, mongo, NewComposerWithMongo(nil, mongo),
		UpstreamSubscriberConfig{Topic: "t", Collection: "users"},
		[]*ViewDefinition{ordersBuyerView()}, nil, nil)
	if err != nil {
		t.Fatalf("NewUpstreamSubscriber: %v", err)
	}
	if _, err := s.RetryPendingFailures(context.Background()); err == nil {
		t.Error("RetryPendingFailures without a PG handle must error")
	}
}

func TestDecodePayload(t *testing.T) {
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(happyColls()), ordersRootEngine())

	if m, err := s.decodePayload(nil); err != nil || len(m) != 0 {
		t.Errorf("empty payload must decode to empty map, got %v %v", m, err)
	}
	if m, err := s.decodePayload([]byte(`{"name":"x"}`)); err != nil || m["name"] != "x" {
		t.Errorf("top-level object must decode verbatim, got %v %v", m, err)
	}
	if m, err := s.decodePayload([]byte(`{"payload":{"name":"y"}}`)); err != nil || m["name"] != "y" {
		t.Errorf("wrapped {payload:...} envelope must unwrap, got %v %v", m, err)
	}
	if _, err := s.decodePayload([]byte(`{bad`)); err == nil {
		t.Error("invalid JSON must error")
	}
}

func TestApplyFilter(t *testing.T) {
	s := newTestUpstream(t, UpstreamSubscriberConfig{Filter: []string{"name"}},
		upstreamFakeMongo(happyColls()), ordersRootEngine())
	out := s.applyFilter(bson.M{"name": "a", "drop": "b"})
	if _, ok := out["name"]; !ok {
		t.Error("allowed key must survive")
	}
	if _, ok := out["drop"]; ok {
		t.Error("non-allowed key must be dropped")
	}
}

func TestJoinFieldFor(t *testing.T) {
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(happyColls()), ordersRootEngine())
	if jf := s.joinFieldFor(ordersBuyerView()); jf != "buyer_id" {
		t.Errorf("expected join field buyer_id, got %q", jf)
	}
	other := View("x").Version(1).Root("orders").Schema(composerRootSchema()).
		Embed("partner", FromSchema(core.NewExternalSchema("partners").PK("id")).On("partner_id").As("Partner"))
	if jf := s.joinFieldFor(other); jf != "" {
		t.Errorf("view not embedding the upstream collection must yield empty join field, got %q", jf)
	}
}

func TestNewUpstreamSubscriber_Validation(t *testing.T) {
	mongo := upstreamFakeMongo(happyColls())
	pg := ordersRootEngine()
	cmp := NewComposerWithMongo(pg, mongo)
	views := []*ViewDefinition{ordersBuyerView()}

	if _, err := NewUpstreamSubscriber(pg, mongo, cmp, UpstreamSubscriberConfig{Collection: "users"}, views, nil, nil); err == nil {
		t.Error("missing Topic must error")
	}
	if _, err := NewUpstreamSubscriber(pg, mongo, cmp, UpstreamSubscriberConfig{Topic: "t"}, views, nil, nil); err == nil {
		t.Error("missing Collection must error")
	}
	if _, err := NewUpstreamSubscriber(pg, mongo, cmp,
		UpstreamSubscriberConfig{Topic: "t", Collection: "users", StartFrom: "offset:notnum"}, views, nil, nil); err == nil {
		t.Error("malformed offset StartFrom must error")
	}
	// Valid offset seek populates the target without error.
	s, err := NewUpstreamSubscriber(pg, mongo, cmp,
		UpstreamSubscriberConfig{Topic: "t", Collection: "users", StartFrom: "offset:42"}, views, nil, nil)
	if err != nil {
		t.Fatalf("valid offset StartFrom must succeed: %v", err)
	}
	if s.offsetSeekTarget == nil || *s.offsetSeekTarget != 42 {
		t.Errorf("offset seek target must be parsed to 42, got %v", s.offsetSeekTarget)
	}
	// Defaults applied for the symbolic empties.
	if s.cfg.Workers != 1 {
		t.Errorf("Workers must default to 1, got %d", s.cfg.Workers)
	}
}

func TestShutdown(t *testing.T) {
	// nil receiver is safe.
	var nilSub *UpstreamSubscriber
	if err := nilSub.Shutdown(context.Background()); err != nil {
		t.Errorf("nil Shutdown must return nil, got %v", err)
	}

	// No in-flight work → drains immediately.
	s := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(happyColls()), ordersRootEngine())
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("clean Shutdown must return nil, got %v", err)
	}
	// Idempotent (stopOnce guards the double close).
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown must be a no-op, got %v", err)
	}

	// In-flight work + an already-expired drain ctx → timeout error.
	s2 := newTestUpstream(t, UpstreamSubscriberConfig{}, upstreamFakeMongo(happyColls()), ordersRootEngine())
	s2.inflight.Add(1)
	drainCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s2.Shutdown(drainCtx); err == nil {
		t.Error("Shutdown with pending in-flight + expired drain must return the ctx error")
	}
	s2.inflight.Done()
}
