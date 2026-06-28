package bootstrap

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/read/mongo"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// extEmbed builds an external (Mongo) embed source from a type-less schema for
// the guard tests — table + FK from the schema, .As supplies the Go segment.
func extEmbed(collection, fk, as string) *mongo.Source {
	return mongo.FromSchema(db.NewExternalSchema(collection).PK("id").FK(fk)).On(fk).As(as)
}

func TestValidateViewSchemas_RejectsMissingRootSchema(t *testing.T) {
	v := mongo.View("orders").Root("orders").Version(1) // no .Schema(...)
	err := mongo.ValidateViewSchemas([]*mongo.ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no root") {
		t.Errorf("expected missing-root-schema error, got %v", err)
	}
}

func TestValidateViewSchemas_RejectsExternalEmbedWithoutAs(t *testing.T) {
	v := mongo.View("orders").Root("orders").
		Schema(db.NewExternalSchema("orders").PK("id")).
		Embed("buyer", mongo.FromSchema(db.NewExternalSchema("users").PK("id")).On("buyer_id")). // no .As
		Version(1)
	err := mongo.ValidateViewSchemas([]*mongo.ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), ".As(") {
		t.Errorf("expected external-embed-missing-.As error, got %v", err)
	}
}

func TestValidateViewSchemas_PassesWhenComplete(t *testing.T) {
	v := mongo.View("orders").Root("orders").
		Schema(db.NewExternalSchema("orders").PK("id")).
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	if err := mongo.ValidateViewSchemas([]*mongo.ViewDefinition{v}); err != nil {
		t.Errorf("expected no error for a complete view, got %v", err)
	}
}

func TestResolveUpstreamSubscriptions_MergesCfgAndWiring(t *testing.T) {
	cfg := &Config{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
	}}
	w := Wiring{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "products.events", Collection: "products"},
	}}
	merged, err := resolveUpstreamSubscriptions(cfg, w)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(merged) != 2 {
		t.Errorf("len(merged) = %d, want 2", len(merged))
	}
}

func TestResolveUpstreamSubscriptions_RejectsTopicCollision(t *testing.T) {
	cfg := &Config{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
	}}
	w := Wiring{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "users.events", Collection: "users_alt"},
	}}
	_, err := resolveUpstreamSubscriptions(cfg, w)
	if err == nil || !strings.Contains(err.Error(), "users.events") {
		t.Errorf("expected collision error, got %v", err)
	}
}

func TestGuardCollectionCollision_SubSub(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
		{Topic: "peer-users.events", Collection: "users"},
	}
	errs := guardCollectionCollision(subs, nil)
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.2") ||
		!strings.Contains(errs[0], "users.events") ||
		!strings.Contains(errs[0], "peer-users.events") {
		t.Errorf("expected sub-sub collision diagnostic, got %v", errs)
	}
}

func TestGuardCollectionCollision_SubLocalView(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
	}
	views := []*mongo.ViewDefinition{mongo.View("users").Root("users").Version(1)}
	errs := guardCollectionCollision(subs, views)
	if len(errs) != 1 || !strings.Contains(errs[0], "local view") {
		t.Errorf("expected local-view collision diagnostic, got %v", errs)
	}
}

func TestGuardMaterializingSource_RejectsUnknownCollection(t *testing.T) {
	v := mongo.View("orders").Root("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	errs := guardMaterializingSource(nil, []*mongo.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.3") ||
		!strings.Contains(errs[0], `"users"`) {
		t.Errorf("expected §8.3 diagnostic for unknown collection, got %v", errs)
	}
}

func TestGuardMaterializingSource_AcceptsSubscriptionCollection(t *testing.T) {
	subs := []UpstreamSubscription{{Topic: "users.events", Collection: "users"}}
	v := mongo.View("orders").Root("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	errs := guardMaterializingSource(subs, []*mongo.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with materializing subscription, got %v", errs)
	}
}

func TestGuardMaterializingSource_RejectsLocalView(t *testing.T) {
	// View-on-view via an external FromSchema (targeting another local
	// ViewDefinition.Name()) is rejected at boot: the recompose ripple is
	// one-hop, so a change upstream of derivative_view would recompose
	// derivative_view but never re-ripple to orders. Drift would silently
	// accumulate. The guard catches the trap before any subscriber starts.
	views := []*mongo.ViewDefinition{
		mongo.View("orders").Root("orders").
			Embed("derivative", extEmbed("derivative_view", "orders_id", "Derivative")).
			Version(1),
		mongo.View("derivative_view").Root("derivative_table").Version(1),
	}
	errs := guardMaterializingSource(nil, views)
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.3") ||
		!strings.Contains(errs[0], "view-on-view") ||
		!strings.Contains(errs[0], "NOT supported") {
		t.Errorf("expected §8.3 view-on-view diagnostic, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_RejectsMissingIndex(t *testing.T) {
	v := mongo.View("orders").Root("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	errs := guardJoinFieldIndex([]*mongo.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.1") ||
		!strings.Contains(errs[0], "buyer_id") {
		t.Errorf("expected §8.1 diagnostic, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_AcceptsSingleFieldIndex(t *testing.T) {
	v := mongo.View("orders").Root("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Indexes(mongo.Index("buyer_id")).
		Version(1)
	errs := guardJoinFieldIndex([]*mongo.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with covering single-field index, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_AcceptsCompoundIndexJoinFieldFirst(t *testing.T) {
	v := mongo.View("orders").Root("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Indexes(mongo.Compound("buyer_id", "created_at")).
		Version(1)
	errs := guardJoinFieldIndex([]*mongo.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with covering compound index, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_RejectsCompoundIndexJoinFieldNotFirst(t *testing.T) {
	v := mongo.View("orders").Root("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Indexes(mongo.Compound("created_at", "buyer_id")).
		Version(1)
	errs := guardJoinFieldIndex([]*mongo.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.1") {
		t.Errorf("expected §8.1 diagnostic (suffix-only coverage), got %v", errs)
	}
}

func TestGuardAnonymizePolicy_RejectsEmptyFields(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", OnUpstreamDelete: UpstreamDeleteAnonymize},
	}
	errs := guardAnonymizePolicy(subs)
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.4") ||
		!strings.Contains(errs[0], "anonymizeFields") {
		t.Errorf("expected §8.4 diagnostic, got %v", errs)
	}
}

func TestGuardAnonymizePolicy_AcceptsCascadeWithEmptyFields(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", OnUpstreamDelete: UpstreamDeleteCascade},
	}
	errs := guardAnonymizePolicy(subs)
	if len(errs) != 0 {
		t.Errorf("cascade does not require AnonymizeFields, got %v", errs)
	}
}

func TestGuardAnonymizePolicy_AcceptsAnonymizeWithFields(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users",
			OnUpstreamDelete: UpstreamDeleteAnonymize,
			AnonymizeFields:  []string{"name", "email"}},
	}
	errs := guardAnonymizePolicy(subs)
	if len(errs) != 0 {
		t.Errorf("anonymize with fields should pass, got %v", errs)
	}
}

func TestValidateUpstreamSubscriptions_AccumulatesAllViolations(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users",
			OnUpstreamDelete: UpstreamDeleteAnonymize, // §8.4 — missing AnonymizeFields
			StartFrom:        StartFromLatest},
		{Topic: "users.events", Collection: "users", // §5 — duplicate sub-sub
			OnUpstreamDelete: UpstreamDeleteCascade,
			StartFrom:        StartFromLatest},
	}
	views := []*mongo.ViewDefinition{
		// §8.1 — external FromSchema without covering index
		mongo.View("orders").Root("orders").
			Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
			Version(1),
	}
	err := validateUpstreamSubscriptions(subs, views, profileDev)
	if err == nil {
		t.Fatal("expected violations")
	}
	msg := err.Error()
	// Should list multiple violation lines (§8.1 + §8.2 + §8.4)
	if !strings.Contains(msg, "§8.1") || !strings.Contains(msg, "§8.2") ||
		!strings.Contains(msg, "§8.4") {
		t.Errorf("expected diagnostic to surface all three section codes, got: %s", msg)
	}
}
