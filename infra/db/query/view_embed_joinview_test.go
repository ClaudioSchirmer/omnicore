package query

import (
	"strings"
	"testing"
)

// ─── query.JoinView as an embed source (materialized view-on-view) ───────────
//
// The declaration contract: Embed/EmbedMany/EmbedInChild accept a JoinView leg
// over a REGISTERED local view. The freshness signal is the SyncEngine's write
// to the source view (byViewSource), which is why the shape that used to be
// rejected ("the ripple is one-hop") is now valid.

// Digests captured from the framework BEFORE the leg_view tag existed — the
// byte-identity anchors of TestRebuildHash_ExternalLegStreamUnchanged.
const (
	externalRootEmbedRebuildHash  = "f3c854c2e3269e1bdedaea507823bddd820c9f9e08dfa7d0a0e580525eabf52b"
	externalChildEmbedRebuildHash = "75c232ae830f9b17159d7c4c590fe3e7fe963a8e7dd7492300f17664c68c770f"
)

// viewLegSource is a minimal registered source view for the leg tests. fkCol
// non-empty declares the covering index a 1:N EmbedMany needs on the leg view.
func viewLegSource(name, fkCol string) *ViewDefinition {
	v := View(name).Version(1).Schema(rootSchema(name))
	if fkCol != "" {
		v = v.Indexes(Index(fkCol))
	}
	return v
}

func TestEmbed_AcceptsRegisteredViewLeg(t *testing.T) {
	src := viewLegSource("products", "")
	v := View("sales").Version(1).Schema(rootSchema("sales")).
		Embed(JoinView(src, "Product", "product")).On("product_id").
		Indexes(Index("product_id")) // the 1:1 reverse-scan index (unchanged rule)
	if err := ValidateViewSchemas([]*ViewDefinition{v, src}); err != nil {
		t.Fatalf("a registered JoinView embed leg must be accepted, got: %v", err)
	}
}

func TestEmbed_RejectsUnregisteredViewLeg(t *testing.T) {
	src := viewLegSource("products", "")
	v := View("sales").Version(1).Schema(rootSchema("sales")).
		Embed(JoinView(src, "Product", "product")).On("product_id").
		Indexes(Index("product_id"))
	err := ValidateViewSchemas([]*ViewDefinition{v}) // src not contributed
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("an unregistered JoinView embed leg must be rejected, got: %v", err)
	}
}

// A 1:N EmbedMany runs one find({fk: parent}) against the LEG VIEW per parent
// document, so the leg view must declare a covering index whose first key is
// the join column — the same rule LinkMany applies to an internal leg.
func TestEmbedMany_RejectsLegViewWithoutCoveringIndex(t *testing.T) {
	src := viewLegSource("sales", "") // no Index("customer_id")
	v := View("customer_dashboard").Version(1).Schema(rootSchema("customers")).
		EmbedMany(JoinView(src, "Sales", "sales")).On("customer_id")
	err := ValidateViewSchemas([]*ViewDefinition{v, src})
	if err == nil || !strings.Contains(err.Error(), "NO covering index") {
		t.Fatalf("EmbedMany over a leg view without a covering index must be rejected, got: %v", err)
	}
}

func TestEmbedMany_AcceptsLegViewWithCoveringIndex(t *testing.T) {
	src := viewLegSource("sales", "customer_id")
	v := View("customer_dashboard").Version(1).Schema(rootSchema("customers")).
		EmbedMany(JoinView(src, "Sales", "sales")).On("customer_id")
	if err := ValidateViewSchemas([]*ViewDefinition{v, src}); err != nil {
		t.Fatalf("EmbedMany over an indexed leg view must be accepted, got: %v", err)
	}
}

// ─── the cycle guard ─────────────────────────────────────────────────────────

// Each hop's ripple writes the embedding view, which fires the next hop's
// signal — a loop would recompose forever, so the graph must be acyclic.
func TestEmbedCycle_RejectsTwoViewLoop(t *testing.T) {
	a := View("a").Version(1).Schema(rootSchema("a")).Indexes(Index("b_id"))
	b := View("b").Version(1).Schema(rootSchema("b")).Indexes(Index("a_id"))
	a.Embed(JoinView(b, "B", "b")).On("b_id")
	b.Embed(JoinView(a, "A", "a")).On("a_id")
	err := ValidateViewSchemas([]*ViewDefinition{a, b})
	if err == nil || !strings.Contains(err.Error(), "view embed cycle") {
		t.Fatalf("a two-view embed loop must be rejected, got: %v", err)
	}
}

func TestEmbedCycle_RejectsSelfEmbed(t *testing.T) {
	a := View("a").Version(1).Schema(rootSchema("a")).Indexes(Index("self_id"))
	a.Embed(JoinView(a, "Self", "self")).On("self_id")
	err := ValidateViewSchemas([]*ViewDefinition{a})
	if err == nil || !strings.Contains(err.Error(), "view embed cycle") {
		t.Fatalf("a self-embed must be rejected, got: %v", err)
	}
}

// An ACYCLIC chain of any depth is valid: every path ends at a view with no
// view-sourced embed, so the ripple terminates (D5a).
func TestEmbedChain_AcceptsAcyclicDepth(t *testing.T) {
	products := viewLegSource("products", "")
	sales := View("sales").Version(1).Schema(rootSchema("sales")).
		Embed(JoinView(products, "Product", "product")).On("product_id").
		Indexes(Index("product_id"), Index("customer_id"))
	dashboard := View("customer_dashboard").Version(1).Schema(rootSchema("customers")).
		EmbedMany(JoinView(sales, "Sales", "sales")).On("customer_id")
	if err := ValidateViewSchemas([]*ViewDefinition{products, sales, dashboard}); err != nil {
		t.Fatalf("an acyclic products→sales→dashboard chain must be accepted, got: %v", err)
	}
}

// ─── the index the SyncEngine signals through ────────────────────────────────

func TestViewIndex_ByViewSourceRoutesViewLegs(t *testing.T) {
	products := viewLegSource("products", "")
	sales := View("sales").Version(1).Schema(rootSchema("sales")).
		Embed(JoinView(products, "Product", "product")).On("product_id").
		Embed(extLeg("upstream_carriers", "Carrier", "carrier")).On("carrier_id")
	idx := buildViewIndex([]*ViewDefinition{products, sales})
	deps := idx.byViewSource["products"]
	if len(deps) != 1 || deps[0].Name() != "sales" {
		t.Fatalf("byViewSource[products] = %v, want [sales]", deps)
	}
	// The external leg still routes to byMongoColl — the two source kinds keep
	// their own namespaces and their own signalling writer.
	if got := idx.byMongoColl["upstream_carriers"]; len(got) != 1 || got[0].Name() != "sales" {
		t.Fatalf("byMongoColl[upstream_carriers] = %v, want [sales]", got)
	}
	// A view name must never leak into the PG-table namespace.
	if got := idx.byPGTable["products"]; len(got) != 1 || got[0].Name() != "products" {
		t.Fatalf("byPGTable[products] = %v, want only the products view itself (its root table)", got)
	}
}

func TestDependentViewViews_FindsRootAndChildEmbeds(t *testing.T) {
	products := viewLegSource("products", "")
	rootEmbedder := View("sales").Version(1).Schema(rootSchema("sales")).
		Embed(JoinView(products, "Product", "product")).On("product_id")
	child := childSrc()
	childEmbedder := View("kits").Version(1).Schema(rootWithChild("kits", "sale_items")).
		EmbedInChild(child, JoinView(products, "Product", "product")).On("product_id")
	unrelated := View("others").Version(1).Schema(rootSchema("others"))
	got := DependentViewViews([]*ViewDefinition{rootEmbedder, childEmbedder, unrelated, products}, "products")
	if len(got) != 2 || got[0].Name() != "sales" || got[1].Name() != "kits" {
		t.Fatalf("DependentViewViews(products) = %v, want [sales kits]", got)
	}
}

// ─── version coupling in the rebuild hash ────────────────────────────────────

// Bumping the SOURCE view's Version moves the EMBEDDING view's RebuildHash, so
// the forgot-to-bump guard fires on the embedder too and its documents are
// rebuilt against the new shape instead of keeping stale copies.
func TestRebuildHash_CouplesToLegViewVersion(t *testing.T) {
	build := func(srcVersion int) string {
		src := View("products").Version(srcVersion).Schema(rootSchema("products"))
		return View("sales").Version(1).Schema(rootSchema("sales")).
			Embed(JoinView(src, "Product", "product")).On("product_id").
			RebuildHash()
	}
	if build(1) == build(2) {
		t.Fatal("bumping the embedded view's Version must move the embedding view's RebuildHash")
	}
}

func TestRebuildHash_ChildEmbedCouplesToLegViewVersion(t *testing.T) {
	child := childSrc()
	build := func(srcVersion int) string {
		src := View("products").Version(srcVersion).Schema(rootSchema("products"))
		return View("kits").Version(1).Schema(rootWithChild("kits", "sale_items")).
			EmbedInChild(child, JoinView(src, "Product", "product")).On("product_id").
			RebuildHash()
	}
	if build(1) == build(2) {
		t.Fatal("bumping the embedded view's Version must move the EmbedInChild embedder's RebuildHash")
	}
}

// The coupling is emitted ONLY for a view leg: an external-sourced view's
// canonical byte stream stays byte-identical, so upgrading the framework moves
// no existing view's hash — no spurious drift/rebuild on any deployed service.
// The pinned digests were captured from the code BEFORE the leg_view tag
// existed; they must never move again.
func TestRebuildHash_ExternalLegStreamUnchanged(t *testing.T) {
	cases := []struct {
		name string
		view func() *ViewDefinition
		want string
	}{
		{
			name: "root external embed",
			view: func() *ViewDefinition {
				return View("orders").Version(1).Schema(rootSchema("orders")).
					Embed(extLeg("upstream_users", "Buyer", "buyer")).On("buyer_id")
			},
			want: externalRootEmbedRebuildHash,
		},
		{
			name: "external EmbedInChild",
			view: func() *ViewDefinition {
				return View("kits").Version(1).Schema(rootWithChild("kits", "sale_items")).
					EmbedInChild(childSrc(), upstreamLeg()).On("product_id")
			},
			want: externalChildEmbedRebuildHash,
		},
	}
	for _, tc := range cases {
		if got := tc.view().RebuildHash(); got != tc.want {
			t.Errorf("%s: RebuildHash moved — every deployed view with this shape would "+
				"drift/rebuild on upgrade.\n got: %s\nwant: %s", tc.name, got, tc.want)
		}
	}
}
