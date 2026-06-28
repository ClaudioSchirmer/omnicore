package mongo

import (
	"testing"
)

func TestFromSchema_ExternalIsMongo(t *testing.T) {
	s := mongoEmbed("users", "").On("buyer_id")
	if !s.IsMongo() {
		t.Error("FromSchema(db.NewExternalSchema) should mark IsMongo=true")
	}
	if s.Collection() != "users" {
		t.Errorf("Collection() = %q", s.Collection())
	}
	if s.JoinKey() != "buyer_id" {
		t.Errorf("JoinKey() = %q", s.JoinKey())
	}
}

func TestFromSchema_AnchoredIsPG(t *testing.T) {
	s := pgEmbed("addresses", "user_id")
	if s.IsMongo() {
		t.Error("FromSchema(db.NewTableSchema) should mark IsMongo=false")
	}
	if s.Table() != "addresses" {
		t.Errorf("Table() = %q", s.Table())
	}
}

func TestSource_CollectionAliasesTable(t *testing.T) {
	pg := pgEmbed("addresses", "")
	mg := mongoEmbed("users", "")
	if pg.Collection() != pg.Table() {
		t.Errorf("Collection should alias Table for PG source")
	}
	if mg.Collection() != mg.Table() {
		t.Errorf("Collection should alias Table for Mongo source")
	}
}

func TestBuildViewIndex_SplitsByKind(t *testing.T) {
	v1 := View("orders").Root("orders").
		EmbedMany("lines", pgEmbed("order_lines", "order_id")).
		Embed("buyer", mongoEmbed("users", "").On("buyer_id")).
		Version(1)
	v2 := View("invoices").Root("invoices").
		Embed("order", mongoEmbed("orders_view", "").On("order_id")).
		Version(1)
	idx := buildViewIndex([]*ViewDefinition{v1, v2})
	// PG side: roots + PG embeds
	if len(idx.byPGTable["orders"]) != 1 {
		t.Errorf("byPGTable[orders] = %d, want 1", len(idx.byPGTable["orders"]))
	}
	if len(idx.byPGTable["order_lines"]) != 1 {
		t.Errorf("byPGTable[order_lines] = %d, want 1", len(idx.byPGTable["order_lines"]))
	}
	if len(idx.byPGTable["invoices"]) != 1 {
		t.Errorf("byPGTable[invoices] = %d, want 1", len(idx.byPGTable["invoices"]))
	}
	// Mongo side: only external FromSchema embeds
	if len(idx.byMongoColl["users"]) != 1 {
		t.Errorf("byMongoColl[users] = %d, want 1", len(idx.byMongoColl["users"]))
	}
	if len(idx.byMongoColl["orders_view"]) != 1 {
		t.Errorf("byMongoColl[orders_view] = %d, want 1", len(idx.byMongoColl["orders_view"]))
	}
	// Negative: PG embed table should NOT leak to Mongo map
	if _, leaked := idx.byMongoColl["order_lines"]; leaked {
		t.Error("byMongoColl should not contain PG embed tables")
	}
}

func TestDependentMongoViews_FindsEmbedders(t *testing.T) {
	v1 := View("orders").Root("orders").
		Embed("buyer", mongoEmbed("users", "").On("buyer_id")).
		Version(1)
	v2 := View("invoices").Root("invoices").Version(1) // no Mongo embed
	v3 := View("audit").Root("audit").
		EmbedMany("perpetrator", mongoEmbed("users", "").On("audit_id")).
		Version(1)
	got := DependentMongoViews([]*ViewDefinition{v1, v2, v3}, "users")
	if len(got) != 2 {
		t.Errorf("DependentMongoViews(users) = %d views, want 2", len(got))
	}
}

func TestDependentMongoViews_EmptyWhenNoMatches(t *testing.T) {
	v := View("orders").Root("orders").
		Embed("buyer", mongoEmbed("users", "").On("buyer_id")).
		Version(1)
	got := DependentMongoViews([]*ViewDefinition{v}, "products")
	if len(got) != 0 {
		t.Errorf("expected no dependents for unrelated collection, got %d", len(got))
	}
}

func TestEmbedDef_AccessorsExposeSourceAndField(t *testing.T) {
	v := View("orders").Root("orders").
		Embed("buyer", mongoEmbed("users", "").On("buyer_id")).
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
