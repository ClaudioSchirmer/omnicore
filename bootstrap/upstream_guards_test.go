package bootstrap

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra"
)

func TestFindSchemaLessMongoEmbeds_FlagsEmbedWithoutSchema(t *testing.T) {
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id")). // no .Schema(...)
		Version(1)
	got := findSchemaLessMongoEmbeds([]*infra.ViewDefinition{v})
	if len(got) != 1 || got[0].View != "orders" || got[0].Collection != "users" {
		t.Errorf("expected one finding {orders,users}, got %+v", got)
	}
}

func TestFindSchemaLessMongoEmbeds_SilentWithSchema(t *testing.T) {
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id").
			Schema(infra.NewExternalSchema("users").PK("ID", "id").Field("Name", "name"))).
		Version(1)
	if got := findSchemaLessMongoEmbeds([]*infra.ViewDefinition{v}); len(got) != 0 {
		t.Errorf("expected no findings when the embed declares a schema, got %+v", got)
	}
}

func TestFindSchemaLessMongoEmbeds_IgnoresLocalFrom(t *testing.T) {
	// A local From source without a schema is NOT flagged — only FromMongo.
	v := infra.View("users").Root("users").
		EmbedMany("addresses", infra.From("addresses").On("user_id")).
		Version(1)
	if got := findSchemaLessMongoEmbeds([]*infra.ViewDefinition{v}); len(got) != 0 {
		t.Errorf("local From embed must not be flagged, got %+v", got)
	}
}

func TestFindSchemaLessMongoEmbeds_RecursesNested(t *testing.T) {
	// FromMongo nested inside a local embed — the detector must descend.
	v := infra.View("orders").Root("orders").
		EmbedMany("lines", infra.From("order_lines").On("order_id").
			Embed("product", infra.FromMongo("products").On("product_id"))). // no .Schema
		Version(1)
	got := findSchemaLessMongoEmbeds([]*infra.ViewDefinition{v})
	if len(got) != 1 || got[0].Collection != "products" {
		t.Errorf("expected one nested finding for products, got %+v", got)
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
	views := []*infra.ViewDefinition{infra.View("users").Root("users").Version(1)}
	errs := guardCollectionCollision(subs, views)
	if len(errs) != 1 || !strings.Contains(errs[0], "local view") {
		t.Errorf("expected local-view collision diagnostic, got %v", errs)
	}
}

func TestGuardMaterializingSource_RejectsUnknownCollection(t *testing.T) {
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id")).
		Version(1)
	errs := guardMaterializingSource(nil, []*infra.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.3") ||
		!strings.Contains(errs[0], `"users"`) {
		t.Errorf("expected §8.3 diagnostic for unknown collection, got %v", errs)
	}
}

func TestGuardMaterializingSource_AcceptsSubscriptionCollection(t *testing.T) {
	subs := []UpstreamSubscription{{Topic: "users.events", Collection: "users"}}
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id")).
		Version(1)
	errs := guardMaterializingSource(subs, []*infra.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with materializing subscription, got %v", errs)
	}
}

func TestGuardMaterializingSource_RejectsLocalView(t *testing.T) {
	// View-on-view via FromMongo (FromMongo targeting another local
	// ViewDefinition.Name()) is rejected at boot: the recompose ripple is
	// one-hop, so a change upstream of derivative_view would recompose
	// derivative_view but never re-ripple to orders. Drift would silently
	// accumulate. The guard catches the trap before any subscriber starts.
	views := []*infra.ViewDefinition{
		infra.View("orders").Root("orders").
			Embed("derivative", infra.FromMongo("derivative_view").On("orders_id")).
			Version(1),
		infra.View("derivative_view").Root("derivative_table").Version(1),
	}
	errs := guardMaterializingSource(nil, views)
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.3") ||
		!strings.Contains(errs[0], "view-on-view") ||
		!strings.Contains(errs[0], "NOT supported") {
		t.Errorf("expected §8.3 view-on-view diagnostic, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_RejectsMissingIndex(t *testing.T) {
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id")).
		Version(1)
	errs := guardJoinFieldIndex([]*infra.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.1") ||
		!strings.Contains(errs[0], "buyer_id") {
		t.Errorf("expected §8.1 diagnostic, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_AcceptsSingleFieldIndex(t *testing.T) {
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id")).
		Indexes(infra.Index("buyer_id")).
		Version(1)
	errs := guardJoinFieldIndex([]*infra.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with covering single-field index, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_AcceptsCompoundIndexJoinFieldFirst(t *testing.T) {
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id")).
		Indexes(infra.Compound("buyer_id", "created_at")).
		Version(1)
	errs := guardJoinFieldIndex([]*infra.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with covering compound index, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_RejectsCompoundIndexJoinFieldNotFirst(t *testing.T) {
	v := infra.View("orders").Root("orders").
		Embed("buyer", infra.FromMongo("users").On("buyer_id")).
		Indexes(infra.Compound("created_at", "buyer_id")).
		Version(1)
	errs := guardJoinFieldIndex([]*infra.ViewDefinition{v})
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
	views := []*infra.ViewDefinition{
		// §8.1 — FromMongo without covering index
		infra.View("orders").Root("orders").
			Embed("buyer", infra.FromMongo("users").On("buyer_id")).
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
