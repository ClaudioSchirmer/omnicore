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
	return core.NewTableSchema[embedFixture](table).PK("id").SoftDelete("deleted_at").
		Child(core.NewTableSchema[childFixture](childTable).PK("id").FK(table + "_id"))
}

func childSrc() *core.TableSchema {
	return core.NewTableSchema[childFixture]("sale_items").PK("id").FK("sales_id")
}

// upstreamSrc is the external enrichment source (a Mongo collection).
func upstreamSrc(fk string) *Source {
	return mongoEmbed("upstream_products", "").FK(fk)
}

func TestEmbedInChild_DeclarationPopulatesChildEmbeds(t *testing.T) {
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(childSrc(), "product", upstreamSrc("product_id").As("Product"))
	if len(v.ChildEmbeds()) != 1 {
		t.Fatalf("want 1 child embed, got %d", len(v.ChildEmbeds()))
	}
	ce := v.ChildEmbeds()[0]
	if ce.Field() != "product" {
		t.Errorf("field: got %q want %q", ce.Field(), "product")
	}
	if ce.FKColumn() != "product_id" {
		t.Errorf("fk: got %q want %q", ce.FKColumn(), "product_id")
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
		EmbedInChild(child, "product", upstreamSrc("product_id").As("Product")).
		Indexes(Index(childDocSegment(child) + ".product_id"))
	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("a valid EmbedInChild must validate, got: %v", err)
	}
}

// The boot guard the design mandates: the schema passed to EmbedInChild MUST be
// a native child of the view root, else boot fails.
func TestEmbedInChild_RejectsNonNativeChild(t *testing.T) {
	notAChild := core.NewTableSchema[childFixture]("random_table").PK("id").FK("sales_id")
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(notAChild, "product", upstreamSrc("product_id").As("Product")).
		Indexes(Index(childDocSegment(notAChild) + ".product_id"))
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "NOT a native child") {
		t.Fatalf("a non-native child must be rejected at boot, got: %v", err)
	}
}

func TestEmbedInChild_RejectsWriteAnchoredSource(t *testing.T) {
	child := childSrc()
	// A type-anchored (write-anchored) source is not allowed — must be external.
	anchored := FromSchema(core.NewTableSchema[embedFixture]("products").PK("id")).FK("product_id")
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, "product", anchored).
		Indexes(Index(childDocSegment(child) + ".product_id"))
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "write-anchored") {
		t.Fatalf("a write-anchored source must be rejected, got: %v", err)
	}
}

// The retroactive breaking guard: EmbedInChild requires the covering multikey
// index on "<childSegment>.<fk>" for the ripple's reverse scan.
func TestEmbedInChild_RejectsMissingIndex(t *testing.T) {
	child := childSrc()
	v := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, "product", upstreamSrc("product_id").As("Product"))
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
		EmbedMany("items", mongoEmbed("upstream_items", "order_id").As("Items"))
	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("EmbedMany must not require an index, got: %v", err)
	}
}

// Adding a child-embed changes the RebuildHash so the forgot-to-bump guard fires.
func TestEmbedInChild_ChangesRebuildHash(t *testing.T) {
	child := childSrc()
	base := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items"))
	withCE := View("sales").Version(1).Schema(rootWithChild("sales", "sale_items")).
		EmbedInChild(child, "product", upstreamSrc("product_id").As("Product")).
		Indexes(Index(childDocSegment(child) + ".product_id"))
	if base.RebuildHash() == withCE.RebuildHash() {
		t.Fatalf("declaring an EmbedInChild must move the RebuildHash")
	}
}
