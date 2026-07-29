package query

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The view→view fan-out: a write to a SOURCE view's document refreshes every
// view materializing it (query.JoinView). These tests drive the signal directly
// with fake collections — no broker, no engine.

// salesEmbedsProducts is the dependent: a 1:1 embed of the local "products"
// view, joined on the parent's ParentID column.
func salesEmbedsProducts() *ViewDefinition {
	return View("sales").Version(1).Schema(composerRootSchema()).
		Embed(JoinView(productsSourceView(), "Product", "product")).On("product_id").
		Indexes(Index("product_id"))
}

func productsSourceView() *ViewDefinition {
	return View("products").Version(1).Schema(composerRootSchema())
}

// signalFixture wires a signal over fake collections: the source view's
// documents, and the dependent view's.
func signalFixture(t *testing.T, dependents ...*ViewDefinition) (*viewEmbedSignal, map[string]*fakeColl) {
	t.Helper()
	src := productsSourceView()
	views := append([]*ViewDefinition{src}, dependents...)
	colls := map[string]*fakeColl{}
	for _, v := range views {
		colls[v.Name()] = &fakeColl{}
	}
	mongo := upstreamFakeMongo(colls)
	sig := newViewEmbedSignal(nil, mongo, NewComposerWithMongo(nil, mongo, identityResolver),
		identityResolver, views, "g", nil, newUpstreamMetrics())
	if sig == nil {
		t.Fatal("a view embedded by another must produce a fan-out")
	}
	return sig, colls
}

func TestViewEmbedSignal_NilWhenNoViewEmbedsAView(t *testing.T) {
	// The default shape of every service that exists today: only external legs.
	v := View("orders").Version(1).Schema(composerRootSchema()).
		Embed(extLeg("upstream_users", "Buyer", "buyer")).On("buyer_id")
	sig := newViewEmbedSignal(nil, upstreamFakeMongo(nil), nil, identityResolver,
		[]*ViewDefinition{v}, "g", nil, newUpstreamMetrics())
	if sig != nil {
		t.Fatal("no view embeds a view — the fan-out must not exist at all")
	}
	// Every hook is a no-op on the nil receiver (the zero-cost default path).
	sig.Written(context.Background(), "orders", "o1")
	sig.Deleted(context.Background(), "orders", "o1", nil, 7)
	if sig.Tracks("orders") || sig.Before(context.Background(), "orders", "o1") != nil {
		t.Fatal("nil fan-out must report no tracking and capture nothing")
	}
}

func TestViewEmbedSignal_WrittenRipplesIntoEmbedder(t *testing.T) {
	sig, colls := signalFixture(t, salesEmbedsProducts())
	// The source document as the SyncEngine just wrote it, plus one dependent
	// document referencing it by ParentID (what the 1:1 reverse scan finds).
	colls["products"].docs = []any{map[string]any{"_id": "p1", "name": "Cable", docRevisionField: int64(5)}}
	colls["sales"].docs = []any{map[string]any{"_id": "s1"}}

	sig.Written(context.Background(), "products", "p1")

	ups := colls["sales"].updates
	if len(ups) != 1 {
		t.Fatalf("want one surgical write on the embedder, got %d", len(ups))
	}
	set := ups[0]["$pipeline"].([]Document)[0]["$set"].(Document)
	if _, has := set["product"]; !has {
		t.Fatalf("the embed segment must be edited, got %v", set)
	}
}

// A source document that reads back as ABSENT produces no signal at all: its
// own DELETED event owns the removal, and clearing segments on a lost race
// would erase live data.
func TestViewEmbedSignal_AbsentSourceDocumentDoesNotRipple(t *testing.T) {
	sig, colls := signalFixture(t, salesEmbedsProducts())
	colls["sales"].docs = []any{map[string]any{"_id": "s1"}}
	// colls["products"] stays empty → read-back finds nothing.
	sig.Written(context.Background(), "products", "p1")
	if got := len(colls["sales"].updates); got != 0 {
		t.Fatalf("an absent source document must not ripple, got %d writes", got)
	}
}

func TestViewEmbedSignal_DeletedClearsSegment(t *testing.T) {
	sig, colls := signalFixture(t, salesEmbedsProducts())
	colls["sales"].docs = []any{map[string]any{"_id": "s1"}}
	sig.Deleted(context.Background(), "products", "p1", nil, 9)
	ups := colls["sales"].updates
	if len(ups) != 1 {
		t.Fatalf("want one write clearing the segment, got %d", len(ups))
	}
	set := ups[0]["$pipeline"].([]Document)[0]["$set"].(Document)
	cond := set["product"].(Document)["$cond"].([]any)
	if lit, ok := cond[1].(Document)["$literal"]; !ok || lit != nil {
		t.Fatalf("a deleted source must clear the 1:1 segment to the explicit null, got %v", cond[1])
	}
}

// Before is the pre-write capture, paid ONLY when some dependent embeds the
// source 1:N (the old parent of a moved child is unreachable afterwards).
func TestViewEmbedSignal_BeforeOnlyForManyDependents(t *testing.T) {
	oneToOne, colls := signalFixture(t, salesEmbedsProducts())
	colls["products"].docs = []any{map[string]any{"_id": "p1"}}
	if got := oneToOne.Before(context.Background(), "products", "p1"); got != nil {
		t.Fatalf("a 1:1-only dependent must cost no pre-write read, got %v", got)
	}

	many := View("dashboard").Version(1).Schema(composerRootSchema()).
		EmbedMany(JoinView(productsSourceView(), "Products", "products")).On("owner_id")
	sig, colls2 := signalFixture(t, many)
	colls2["products"].docs = []any{map[string]any{"_id": "p1", "owner_id": "o1"}}
	if got := sig.Before(context.Background(), "products", "p1"); got == nil {
		t.Fatal("a 1:N dependent must capture the pre-write document")
	}
}

// ─── the revision guard (D2) ─────────────────────────────────────────────────

// A WATERMARKED source (a view carries _revision) guards every edit so a late
// older write cannot regress a segment a newer write already landed.
func TestSurgicalStages_WatermarkGuardsOneToOne(t *testing.T) {
	e := embedDef{leg: JoinView(productsSourceView(), "Product", "product"), joinCol: "product_id"}
	guarded := stageSet(t, surgicalEmbedStages([]embedDef{e}, "p1",
		Document{"_id": "p1", docRevisionField: int64(4)}, 4))
	cond := guarded["product"].(Document)["$cond"].([]any)
	and, ok := cond[0].(Document)["$and"].([]any)
	if !ok || len(and) != 2 {
		t.Fatalf("a watermarked 1:1 edit must guard on ParentID match AND the stored revision, got %v", cond[0])
	}

	// srcRev == 0 (an upstream mirror) keeps the unguarded shape — byte-identical
	// to what that path always produced.
	unguarded := stageSet(t, surgicalEmbedStages([]embedDef{e}, "p1",
		Document{"_id": "p1"}, 0))
	plain := unguarded["product"].(Document)["$cond"].([]any)
	if _, guarded := plain[0].(Document)["$and"]; guarded {
		t.Fatalf("an unwatermarked source must emit the plain ParentID condition, got %v", plain[0])
	}
}

func TestSurgicalStages_WatermarkGuardsOneToMany(t *testing.T) {
	e := embedDef{leg: JoinView(productsSourceView(), "Products", "products"), many: true, joinCol: "owner_id"}
	set := stageSet(t, surgicalEmbedStages([]embedDef{e}, "p1",
		Document{"_id": "p1", "owner_id": "o1", docRevisionField: int64(3)}, 3))
	// The append is conditioned on the parent AND on no newer element being
	// stored; the strip keeps everything when a newer element is present.
	cond := set["products"].(Document)["$cond"].([]any)
	and, ok := cond[0].(Document)["$and"].([]any)
	if !ok || len(and) != 2 {
		t.Fatalf("a watermarked 1:N append must be guarded, got %v", cond[0])
	}
	strip := cond[2].(Document)["$filter"].(Document)
	if _, ok := strip["cond"].(Document)["$or"]; !ok {
		t.Fatalf("a watermarked strip must keep a newer stored element, got %v", strip["cond"])
	}
}

// ─── chaining (the second hop) ───────────────────────────────────────────────

// A ripple that refreshes a dependent view must itself signal: that view may be
// materialized into another one, so the chain propagates hop by hop.
func TestViewEmbedSignal_ChainsToNextHop(t *testing.T) {
	products := productsSourceView()
	sales := View("sales").Version(1).Schema(composerRootSchema()).
		Embed(JoinView(products, "Product", "product")).On("product_id").
		Indexes(Index("product_id"))
	dashboard := View("dashboard").Version(1).Schema(composerRootSchema()).
		Embed(JoinView(sales, "Sale", "sale")).On("sale_id").
		Indexes(Index("sale_id"))
	colls := map[string]*fakeColl{
		"products":  {docs: []any{map[string]any{"_id": "p1", docRevisionField: int64(1)}}},
		"sales":     {docs: []any{map[string]any{"_id": "s1", docRevisionField: int64(2)}}},
		"dashboard": {docs: []any{map[string]any{"_id": "d1"}}},
	}
	mongo := upstreamFakeMongo(colls)
	sig := newViewEmbedSignal(nil, mongo, NewComposerWithMongo(nil, mongo, identityResolver),
		identityResolver, []*ViewDefinition{products, sales, dashboard}, "g", nil, newUpstreamMetrics())

	sig.Written(context.Background(), "products", "p1")

	if len(colls["sales"].updates) == 0 {
		t.Fatal("hop 1 (products → sales) did not fire")
	}
	if len(colls["dashboard"].updates) == 0 {
		t.Fatal("hop 2 (sales → dashboard) did not fire — the chain stopped at the first hop")
	}
}

// TestViewLegRippleFailure_LandsInTheUnifiedLedger is the certainty check for
// the JoinView family: a failed refresh of a view-leg segment must be RECORDED
// in omnicore_projection_failures as kind=ripple under the view-source
// coordinate ("view:<name>"), exactly like an upstream-mirror leg — this pins
// that JoinView records rather than failing into the void.
func TestViewLegRippleFailure_LandsInTheUnifiedLedger(t *testing.T) {
	src := viewLegSource("products", "")
	sales := View("sales").Version(1).Schema(rootSchema("sales")).
		Embed(JoinView(src, "Product", "product")).On("product_id").
		Indexes(Index("product_id"))

	var mu sync.Mutex
	var recorded [][]any
	q := &fakeQuerier{execFn: func(sql string, args []any) error {
		if strings.Contains(sql, "omnicore_projection_failures") && strings.Contains(sql, "INSERT") {
			mu.Lock()
			recorded = append(recorded, args)
			mu.Unlock()
		}
		return nil
	}}

	// products: the source document the signal re-reads. sales: the dependent
	// doc discovered by ParentID, whose SEGMENT WRITE FAILS — the leg falling over.
	prodColl := &fakeColl{docs: []any{map[string]any{"_id": "p1", DocRevisionField: int64(2), "name": "Widget"}}}
	salesColl := &fakeColl{docs: []any{map[string]any{"_id": "s1", "product_id": "p1"}}, updateErr: errFake}
	store := newFakeMongoFunc(func(name string) *fakeColl {
		if strings.HasPrefix(name, "sales") {
			return salesColl
		}
		return prodColl
	})

	identityResolver.lease = 0
	s := NewSyncEngine(newFakeEngine(q), store, identityResolver, nil, "g", []*ViewDefinition{sales, src}, 1)
	if s.viewSignal == nil {
		t.Fatal("wiring: sales embeds products, the view signal must exist")
	}

	s.viewSignal.Written(context.Background(), "products", "p1")

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) == 0 {
		t.Fatal("a failed view-leg segment write must be recorded in omnicore_upstream_failures — it failed into the void")
	}
	joined := fmt.Sprint(recorded)
	if !strings.Contains(joined, "view:products") {
		t.Errorf("the failure row must carry the view-source coordinate \"view:products\", got args: %v", joined)
	}
	if !strings.Contains(joined, "sales") {
		t.Errorf("the failure row must name the dependent view \"sales\", got args: %v", joined)
	}
}

// TestParkedRetry_ReplaysViewLegRipple closes the loop the unification exists
// for: a failed view-leg refresh parks a kind=ripple row, and the SAME retry
// driver that replays parked events re-runs the ripple from CURRENT state —
// the segment lands and the rippler resolves the row. One ledger, one clock.
func TestParkedRetry_ReplaysViewLegRipple(t *testing.T) {
	src := viewLegSource("products", "")
	sales := View("sales").Version(1).Schema(rootSchema("sales")).
		Embed(JoinView(src, "Product", "product")).On("product_id").
		Indexes(Index("product_id"))

	var mu sync.Mutex
	var parked [][]any
	var resolves int
	q := &fakeQuerier{}
	q.execFn = func(sql string, args []any) error {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(sql, "omnicore_projection_failures") && strings.Contains(sql, "INSERT") {
			parked = append(parked, args)
		}
		if strings.Contains(sql, "resolved_at = ") {
			resolves++
		}
		return nil
	}
	q.queryFn = func(sql string, _ []any) (core.Rows, error) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(sql, "omnicore_projection_failures") || len(parked) == 0 {
			return &fakeRows{}, nil
		}
		rec := parked[len(parked)-1]
		return &fakeRows{
			rows: 1,
			scan: func(_ int, dest []any) error {
				*(dest[0].(*string)) = "row-1"
				*(dest[1].(*string)) = rec[1].(string) // kind = ripple
				*(dest[2].(*string)) = rec[2].(string) // consumer_group
				*(dest[3].(*string)) = rec[3].(string) // topic = view:products
				*(dest[4].(*string)) = rec[4].(string) // aggregate_type = sales
				*(dest[6].(*string)) = rec[6].(string) // aggregate_id = p1
				*(dest[11].(*string)) = "boom"
				*(dest[12].(*int)) = 1
				return nil
			},
		}, nil
	}

	prodColl := &fakeColl{docs: []any{map[string]any{"_id": "p1", DocRevisionField: int64(2), "name": "Widget"}}}
	salesColl := &fakeColl{docs: []any{map[string]any{"_id": "s1", "product_id": "p1"}}, updateErr: errFake}
	store := newFakeMongoFunc(func(name string) *fakeColl {
		if strings.HasPrefix(name, "sales") {
			return salesColl
		}
		return prodColl
	})

	identityResolver.lease = 0
	s := NewSyncEngine(newFakeEngine(q), store, identityResolver, nil, "g", []*ViewDefinition{sales, src}, 1)

	// The leg fails and parks.
	s.viewSignal.Written(context.Background(), "products", "p1")
	if len(parked) == 0 {
		t.Fatal("precondition: the failed leg must park a ripple row")
	}
	if got := len(salesColl.updates); got != 0 {
		t.Fatalf("precondition: no segment write may have landed, got %d", got)
	}

	// The outage heals; the SAME retry driver sweeps the ledger.
	salesColl.updateErr = nil
	if err := s.RetryPendingProjectionFailures(context.Background()); err != nil {
		t.Fatalf("RetryPendingProjectionFailures: %v", err)
	}
	if len(salesColl.updates) == 0 {
		t.Fatal("the replay must land the embed segment — otherwise the park was a dead letter")
	}
	mu.Lock()
	defer mu.Unlock()
	if resolves == 0 {
		t.Fatal("a clean ripple pass must resolve the pending row (the rippler's own resolve path)")
	}
}

// TestParkedRetry_MootRowsResolve: a ripple row whose SOURCE left the topology
// (embed removed, subscription gone) has no segment left to repair — the
// driver must resolve it as moot instead of pending it forever, and a
// registered upstream replayer must be dispatched to.
func TestParkedRetry_MootRowsResolve(t *testing.T) {
	var mu sync.Mutex
	var resolves int
	pendingRows := [][]any{}
	q := &fakeQuerier{}
	q.execFn = func(sql string, _ []any) error {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(sql, "resolved_at = ") {
			resolves++
		}
		return nil
	}
	q.queryFn = func(sql string, _ []any) (core.Rows, error) {
		if !strings.Contains(sql, "omnicore_projection_failures") {
			return &fakeRows{}, nil
		}
		rows := pendingRows
		return &fakeRows{
			rows: len(rows),
			scan: func(i int, dest []any) error {
				r := rows[i]
				*(dest[0].(*string)) = "row"
				*(dest[1].(*string)) = "ripple"
				*(dest[2].(*string)) = "g"
				*(dest[3].(*string)) = r[0].(string) // topic
				*(dest[4].(*string)) = r[1].(string) // dependent view
				*(dest[6].(*string)) = r[2].(string) // source id
				*(dest[11].(*string)) = "boom"
				*(dest[12].(*int)) = 1
				return nil
			},
		}, nil
	}

	identityResolver.lease = 0
	// No views registered → viewSignal is nil → every view: row is untracked.
	s := NewSyncEngine(newFakeEngine(q), newFakeMongo(&fakeColl{}), identityResolver, nil, "g", nil, 1)

	replayed := 0
	s.registerRippleReplayer("users.events", func(_ context.Context, id string) { replayed++ })

	pendingRows = [][]any{
		{"view:ghosts", "sales", "p1"},   // untracked view source → moot, resolve
		{"orders.events", "sales", "p2"}, // unregistered topic → moot, resolve
		{"users.events", "sales", "p3"},  // registered → dispatched to the replayer
	}
	if err := s.RetryPendingProjectionFailures(context.Background()); err != nil {
		t.Fatalf("RetryPendingProjectionFailures: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if resolves != 2 {
		t.Errorf("the two topology-orphaned rows must resolve as moot, got %d resolves", resolves)
	}
	if replayed != 1 {
		t.Errorf("the registered topic must be dispatched exactly once, got %d", replayed)
	}
}
