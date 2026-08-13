package query

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// childFixture is a distinct Go type so childDocSegment derives a segment name
// separate from the root's embedFixture.
type childFixture struct{ ID string }

// rootWithChild builds a root schema declaring one native aggregate child — the
// only kind EmbedInChild may enrich.
func rootWithChild(table, childTable string) *core.TableSchema {
	return core.NewTableSchema[embedFixture](table).ID("id").DeletedAt("deleted_at").
		Child(core.NewTableSchema[childFixture](childTable).ID("id").ParentID(table + "_id"))
}

func childSrc() *core.TableSchema {
	return core.NewTableSchema[childFixture]("sale_items").ID("id").ParentID("sales_id")
}

// upstreamLeg is the external enrichment leg (a Mongo collection); the element ParentID
// is named at the call site via .On(...).
func upstreamLeg() *Leg {
	return extLeg("upstream_products", "Product", "product")
}

func TestEmbedInChild_DeclarationPopulatesChildEmbeds(t *testing.T) {
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(childSrc(), upstreamLeg()).On("product_id")
	if len(v.ChildEmbeds()) != 1 {
		t.Fatalf("want 1 child embed, got %d", len(v.ChildEmbeds()))
	}
	ce := v.ChildEmbeds()[0]
	if ce.Field() != "product" {
		t.Errorf("field: got %q want %q", ce.Field(), "product")
	}
	if ce.ParentIDColumn() != "product_id" {
		t.Errorf("fk: got %q want %q", ce.ParentIDColumn(), "product_id")
	}
	if ce.ChildSchema().Table() != "sale_items" {
		t.Errorf("child table: got %q want %q", ce.ChildSchema().Table(), "sale_items")
	}
	if ce.Source().Collection() != "upstream_products" {
		t.Errorf("source: got %q want %q", ce.Source().Collection(), "upstream_products")
	}
}

func TestEmbedInChild_ValidPassesValidation(t *testing.T) {
	child := childSrc()
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, upstreamLeg()).On("product_id").
		Indexes(Index(childDocSegment(child) + ".product_id"))
	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("a valid EmbedInChild must validate, got: %v", err)
	}
}

// The boot guard the design mandates: the schema passed to EmbedInChild MUST be
// a native child of the view root, else boot fails.
func TestEmbedInChild_RejectsNonNativeChild(t *testing.T) {
	notAChild := core.NewTableSchema[childFixture]("random_table").ID("id").ParentID("sales_id")
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(notAChild, upstreamLeg()).On("product_id").
		Indexes(Index(childDocSegment(notAChild) + ".product_id"))
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "NOT a native child") {
		t.Fatalf("a non-native child must be rejected at boot, got: %v", err)
	}
}

// EmbedInChild composes an external mirror OR a local view (JoinView): the view
// leg is accepted when the source view is registered — the SyncEngine signals
// every write to it, so the enrichment is kept fresh by the same ripple.
func TestEmbedInChild_AcceptsRegisteredViewLeg(t *testing.T) {
	child := childSrc()
	legView := View("someview").Version(1).Schema(rootSchema("someview"))
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, JoinView(legView, "Product", "product")).On("product_id").
		Indexes(Index(childDocSegment(child) + ".product_id"))
	if err := ValidateViewSchemas([]*ViewDefinition{v, legView}); err != nil {
		t.Fatalf("a registered JoinView enrichment leg must be accepted, got: %v", err)
	}
}

// An UNregistered source view is rejected: nothing contributes its collection,
// so the enrichment would resolve to null forever.
func TestEmbedInChild_RejectsUnregisteredViewLeg(t *testing.T) {
	child := childSrc()
	legView := View("someview").Version(1).Schema(rootSchema("someview"))
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, JoinView(legView, "Product", "product")).On("product_id").
		Indexes(Index(childDocSegment(child) + ".product_id"))
	err := ValidateViewSchemas([]*ViewDefinition{v}) // legView not contributed
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("an unregistered JoinView enrichment leg must be rejected, got: %v", err)
	}
}

// The retroactive breaking guard: EmbedInChild requires the covering multikey
// index on "<childSegment>.<fk>" for the ripple's reverse scan.
func TestEmbedInChild_RejectsMissingIndex(t *testing.T) {
	child := childSrc()
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, upstreamLeg()).On("product_id")
	// no .Indexes(...)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "multikey index") {
		t.Fatalf("a missing covering index must be rejected, got: %v", err)
	}
}

// EmbedMany is exempt from the index guard (its ripple never reverse-scans the
// view), so a view with only an EmbedMany and no index still validates.
func TestEmbedMany_ExemptFromIndexGuard(t *testing.T) {
	v := View("orders").Version(1).Schema(rootSchema("orders")).
		EmbedMany(extLeg("upstream_items", "Items", "items")).On("order_id")
	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("EmbedMany must not require an index, got: %v", err)
	}
}

// Adding a child-embed changes the RebuildHash so the forgot-to-bump guard fires.
func TestEmbedInChild_ChangesRebuildHash(t *testing.T) {
	child := childSrc()
	base := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items"))
	withCE := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, upstreamLeg()).On("product_id").
		Indexes(Index(childDocSegment(child) + ".product_id"))
	if base.RebuildHash() == withCE.RebuildHash() {
		t.Fatalf("declaring an EmbedInChild must move the RebuildHash")
	}
}

func (childFixture) CollectionName() string { return "ChildFixtures" }
