package query

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Rebuild ordering over the embed graph.
//
// A rebuild composes its embed segments by reading the SOURCE view's ACTIVE
// collection, and that pointer flips only when the source's own rebuild
// finishes. Rebuilding an embedder first therefore materializes the source's
// pre-flip content and finishes stale — with nothing left to repair it, since
// rebuild writes deliberately raise no embed signal. The order is not operator
// advice: bumping a source's Version moves every embedder's hash too, so both
// land in one rebuild run, and the framework must sequence them itself.

type orderRoot struct{ ID string }

func orderView(name string) *ViewDefinition {
	return View(name).Version(1).Schema(
		core.NewTableSchema[orderRoot](name).ID("id").SoftDelete("deleted_at"))
}

func names(views []*ViewDefinition) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Name()
	}
	return out
}

func indexOf(list []string, name string) int {
	for i, n := range list {
		if n == name {
			return i
		}
	}
	return -1
}

// The declaration order is the WORST case: the embedder is declared first, so
// an unordered run would rebuild it before its source.
func TestRebuildOrder_SourceBeforeEmbedder(t *testing.T) {
	products := orderView("products")
	sales := orderView("sales").
		Embed(JoinView(products, "Product", "product")).On("product_id").
		Indexes(Index("product_id"))

	got := names(OrderViewsByEmbedDependency([]*ViewDefinition{sales, products}))
	if indexOf(got, "products") > indexOf(got, "sales") {
		t.Fatalf("the source must be rebuilt before the view materializing it, got %v", got)
	}
}

// A chain orders transitively: products → sales → dashboard.
func TestRebuildOrder_ChainIsTransitive(t *testing.T) {
	products := orderView("products")
	sales := orderView("sales").
		Embed(JoinView(products, "Product", "product")).On("product_id").
		Indexes(Index("product_id"))
	dashboard := orderView("dashboard").
		Embed(JoinView(sales, "Sale", "sale")).On("sale_id").
		Indexes(Index("sale_id"))

	got := names(OrderViewsByEmbedDependency([]*ViewDefinition{dashboard, sales, products}))
	if !(indexOf(got, "products") < indexOf(got, "sales") && indexOf(got, "sales") < indexOf(got, "dashboard")) {
		t.Fatalf("a chain must rebuild source-first at every hop, got %v", got)
	}
}

// An EmbedInChild leg is an edge too — the enrichment reads the source view.
func TestRebuildOrder_ChildEmbedIsAnEdge(t *testing.T) {
	products := orderView("products")
	child := childSrc()
	kits := View("kits").Version(1).Schema(rootWithChild("kits", "sale_items")).
		EmbedInChild(child, JoinView(products, "Product", "product")).On("product_id").
		Indexes(Index(childDocSegment(child) + ".product_id"))

	got := names(OrderViewsByEmbedDependency([]*ViewDefinition{kits, products}))
	if indexOf(got, "products") > indexOf(got, "kits") {
		t.Fatalf("an EmbedInChild source must be rebuilt first too, got %v", got)
	}
}

// Views with no dependency between them keep their declaration order, so a
// service that embeds no view gets its input back untouched.
func TestRebuildOrder_StableWithoutViewLegs(t *testing.T) {
	in := []*ViewDefinition{orderView("c"), orderView("a"), orderView("b")}
	got := names(OrderViewsByEmbedDependency(in))
	want := []string{"c", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("independent views must keep declaration order, got %v want %v", got, want)
		}
	}
}

// EmbedSourceViews reports the direct edges — the input both the ordering and
// the boot's defer-the-dependent guard read.
func TestEmbedSourceViews_ReportsBothEmbedKinds(t *testing.T) {
	products, carriers := orderView("products"), orderView("carriers")
	child := childSrc()
	v := View("kits").Version(1).Schema(rootWithChild("kits", "sale_items")).
		Embed(JoinView(carriers, "Carrier", "carrier")).On("carrier_id").
		Embed(extLeg("upstream_users", "Buyer", "buyer")).On("buyer_id").
		EmbedInChild(child, JoinView(products, "Product", "product")).On("product_id").
		Indexes(Index("carrier_id"), Index("buyer_id"), Index(childDocSegment(child)+".product_id"))

	got := EmbedSourceViews(v)
	if len(got) != 2 || indexOf(got, "carriers") < 0 || indexOf(got, "products") < 0 {
		t.Fatalf("EmbedSourceViews must report every VIEW leg and no mirror leg, got %v", got)
	}
}
