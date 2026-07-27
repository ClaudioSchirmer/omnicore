package query

import (
	"testing"
)

// Leg accessor / kind coverage lives in TestLeg_Accessors (view_test.go); the old
// Source-constructor kind tests were removed with the type.

func TestBuildViewIndex_RootsAndEmbeds(t *testing.T) {
	// Every embed is external now, so it indexes by Mongo collection; roots index
	// by their PG table.
	v1 := View("orders").
		Schema(rootSchema("orders")).
		EmbedMany(extLeg("order_lines", "Lines", "lines")).On("order_id").
		Embed(extLeg("users", "Buyer", "buyer")).On("buyer_id").
		Version(1)
	v2 := View("invoices").
		Schema(rootSchema("invoices")).
		Embed(extLeg("orders_view", "Order", "order")).On("order_id").
		Version(1)
	idx := buildViewIndex([]*ViewDefinition{v1, v2})
	// PG side: roots only.
	if len(idx.byPGTable["orders"]) != 1 {
		t.Errorf("byPGTable[orders] = %d, want 1", len(idx.byPGTable["orders"]))
	}
	if len(idx.byPGTable["invoices"]) != 1 {
		t.Errorf("byPGTable[invoices] = %d, want 1", len(idx.byPGTable["invoices"]))
	}
	// Mongo side: every external embed collection.
	if len(idx.byMongoColl["users"]) != 1 {
		t.Errorf("byMongoColl[users] = %d, want 1", len(idx.byMongoColl["users"]))
	}
	if len(idx.byMongoColl["orders_view"]) != 1 {
		t.Errorf("byMongoColl[orders_view] = %d, want 1", len(idx.byMongoColl["orders_view"]))
	}
	if len(idx.byMongoColl["order_lines"]) != 1 {
		t.Errorf("byMongoColl[order_lines] = %d, want 1", len(idx.byMongoColl["order_lines"]))
	}
}

func TestDependentMongoViews_FindsEmbedders(t *testing.T) {
	v1 := View("orders").
		Embed(extLeg("users", "Buyer", "buyer")).On("buyer_id").
		Version(1)
	v2 := View("invoices").Version(1) // no Mongo embed
	v3 := View("audit").
		EmbedMany(extLeg("users", "Perpetrator", "perpetrator")).On("audit_id").
		Version(1)
	got := DependentMongoViews([]*ViewDefinition{v1, v2, v3}, "users")
	if len(got) != 2 {
		t.Errorf("DependentMongoViews(users) = %d views, want 2", len(got))
	}
}

func TestDependentMongoViews_EmptyWhenNoMatches(t *testing.T) {
	v := View("orders").
		Embed(extLeg("users", "Buyer", "buyer")).On("buyer_id").
		Version(1)
	got := DependentMongoViews([]*ViewDefinition{v}, "products")
	if len(got) != 0 {
		t.Errorf("expected no dependents for unrelated collection, got %d", len(got))
	}
}

func TestEmbedDef_AccessorsExposeSourceAndField(t *testing.T) {
	v := View("orders").
		Embed(extLeg("users", "Buyer", "buyer")).On("buyer_id").
		Version(1)
	embeds := v.Embeds()
	if len(embeds) != 1 {
		t.Fatalf("Embeds() len = %d, want 1", len(embeds))
	}
	e := embeds[0]
	if e.Field() != "buyer" {
		t.Errorf("Field() = %q", e.Field())
	}
	if e.Many() {
		t.Error("Embed should produce many=false")
	}
	if e.Source() == nil || e.Source().Collection() != "users" {
		t.Errorf("Source().Collection() = %v", e.Source())
	}
	if e.JoinColumn() != "buyer_id" {
		t.Errorf("JoinColumn() = %q, want buyer_id", e.JoinColumn())
	}
}

func TestIndexSpec_KeyNames(t *testing.T) {
	if got := Index("email").KeyNames(); len(got) != 1 || got[0] != "email" {
		t.Errorf("single-field KeyNames = %v", got)
	}
	if got := Compound("email", "created_at").KeyNames(); len(got) != 2 ||
		got[0] != "email" || got[1] != "created_at" {
		t.Errorf("compound KeyNames = %v", got)
	}
}
