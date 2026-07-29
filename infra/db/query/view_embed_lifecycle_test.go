package query

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Lifecycle battery for a MATERIALIZED view leg. A segment fed by a local view
// must survive the source's whole lifecycle — insert, partial update, archive,
// unarchive, hard delete — and the embedding document's own ParentID change, which no
// event on the source side would ever announce.

type lifeProduct struct {
	ID   string
	Name string
}

type lifeSale struct {
	ID        string
	ProductID string
}

func lifeProductsView() *ViewDefinition {
	return View("products").Version(1).Schema(
		core.NewTableSchema[lifeProduct]("products").ID("id").SoftDelete("deleted_at").
			Field("Name", "name"))
}

func lifeSalesView() *ViewDefinition {
	return View("sales").Version(1).
		Schema(core.NewTableSchema[lifeSale]("sales").ID("id").SoftDelete("deleted_at").
			Field("ProductID", "product_id")).
		Embed(JoinView(lifeProductsView(), "Product", "product")).On("product_id").
		Indexes(Index("product_id"))
}

// lifeFixture wires the signal over fake collections holding one source doc and
// one embedding doc.
func lifeFixture(t *testing.T) (*viewEmbedSignal, map[string]*fakeColl) {
	t.Helper()
	products, sales := lifeProductsView(), lifeSalesView()
	colls := map[string]*fakeColl{"products": {}, "sales": {}}
	mongo := upstreamFakeMongo(colls)
	sig := newViewEmbedSignal(nil, mongo, NewComposerWithMongo(nil, mongo, identityResolver),
		identityResolver, []*ViewDefinition{products, sales}, "g", nil, newUpstreamMetrics())
	if sig == nil {
		t.Fatal("fixture must produce a fan-out")
	}
	return sig, colls
}

// lastSegmentEdit returns the $set expression written on the embedding view's
// last pipeline write.
func lastSegmentEdit(t *testing.T, c *fakeColl, field string) Document {
	t.Helper()
	if len(c.updates) == 0 {
		t.Fatal("no write reached the embedding view")
	}
	last := c.updates[len(c.updates)-1]
	set, ok := last["$pipeline"].([]Document)[0]["$set"].(Document)
	if !ok {
		t.Fatalf("expected a $set pipeline write, got %v", last)
	}
	seg, has := set[field]
	if !has {
		t.Fatalf("the write did not touch segment %q: %v", field, set)
	}
	return seg.(Document)
}

// literalOf returns the $literal payload of a 1:1 segment edit's true branch.
func literalOf(t *testing.T, seg Document) any {
	t.Helper()
	cond, ok := seg["$cond"].([]any)
	if !ok {
		t.Fatalf("expected a guarded $cond edit, got %v", seg)
	}
	val, ok := cond[1].(Document)["$literal"]
	if !ok {
		t.Fatalf("expected a $literal value branch, got %v", cond[1])
	}
	return val
}

// INSERT / partial UPDATE of the source: the segment carries the source's
// CURRENT stored document, read back after the write (the projection pipeline
// writes stages, not a full document — only the read-back knows the result).
func TestLifecycle_SourceUpdateRefreshesSegment(t *testing.T) {
	sig, colls := lifeFixture(t)
	colls["sales"].docs = []any{map[string]any{"_id": "s1", "product_id": "p1"}}
	colls["products"].docs = []any{map[string]any{
		"_id": "p1", "name": "Cable v2", docRevisionField: int64(2)}}

	sig.Written(context.Background(), "products", "p1")

	elem := literalOf(t, lastSegmentEdit(t, colls["sales"], "product")).(Document)
	if elem["name"] != "Cable v2" {
		t.Fatalf("the segment must carry the source's updated state, got %v", elem)
	}
}

// ARCHIVE under the default (keep) policy: the source document survives with
// its soft-delete column populated, and the segment mirrors that state — the
// same contract an external mirror segment has. The reference is NOT nulled.
func TestLifecycle_SourceArchivedKeepsMirroredSegment(t *testing.T) {
	sig, colls := lifeFixture(t)
	colls["sales"].docs = []any{map[string]any{"_id": "s1", "product_id": "p1"}}
	colls["products"].docs = []any{map[string]any{
		"_id": "p1", "name": "Cable", "deleted_at": "2026-01-02T00:00:00Z", docRevisionField: int64(3)}}

	sig.Written(context.Background(), "products", "p1")

	elem := literalOf(t, lastSegmentEdit(t, colls["sales"], "product")).(Document)
	if elem["deleted_at"] == nil {
		t.Fatalf("an archived source must reach the segment carrying its soft-delete stamp, got %v", elem)
	}
}

// UNARCHIVE: the source document is written again with a cleared soft-delete
// column; the segment converges to the live state.
func TestLifecycle_SourceUnarchivedRefreshesSegment(t *testing.T) {
	sig, colls := lifeFixture(t)
	colls["sales"].docs = []any{map[string]any{"_id": "s1", "product_id": "p1"}}
	colls["products"].docs = []any{map[string]any{
		"_id": "p1", "name": "Cable", "deleted_at": nil, docRevisionField: int64(4)}}

	sig.Written(context.Background(), "products", "p1")

	elem := literalOf(t, lastSegmentEdit(t, colls["sales"], "product")).(Document)
	if elem["deleted_at"] != nil {
		t.Fatalf("an unarchived source must clear the stamp in the segment, got %v", elem)
	}
}

// HARD DELETE (and archive under DeleteOnArchive, which routes identically):
// the 1:1 segment becomes the explicit null the unresolved contract requires.
func TestLifecycle_SourceDeletedNullsSegment(t *testing.T) {
	sig, colls := lifeFixture(t)
	colls["sales"].docs = []any{map[string]any{"_id": "s1", "product_id": "p1"}}

	sig.Deleted(context.Background(), "products", "p1", nil, 9)

	val := literalOf(t, lastSegmentEdit(t, colls["sales"], "product"))
	if val != nil {
		t.Fatalf("a deleted source must null the 1:1 segment, got %v", val)
	}
}

// The case NO event on the source announces: the EMBEDDING document changes its
// ParentID. Field ownership keeps the consult write off the segment, and the source
// never changed — so without the post-write repair the segment would point at
// the old product forever.
func TestLifecycle_EmbedderFKChangeRepairsSegment(t *testing.T) {
	colls := map[string]*fakeColl{
		"products": {docs: []any{map[string]any{"_id": "p2", "name": "New"}}},
		"sales":    {},
	}
	mongo := upstreamFakeMongo(colls)
	view := lifeSalesView()

	repairDanglingOneToOne(context.Background(), mongo, identityResolver, nil, view, "s1",
		Document{"id": "s1", "product_id": "p2"}, nil)

	ups := colls["sales"].updates
	if len(ups) != 1 {
		t.Fatalf("an ParentID change must trigger exactly one repair write on a VIEW leg, got %d", len(ups))
	}
	set := ups[0]["$pipeline"].([]Document)[0]["$set"].(Document)
	cond := set["product"].(Document)["$cond"].([]any)
	and, ok := cond[0].(Document)["$and"].([]any)
	if !ok || len(and) != 2 {
		t.Fatalf("the repair must keep its double guard (ParentID match + not-already-this-id), got %v", cond[0])
	}
	elem, ok := cond[1].(Document)["$literal"].(Document)
	if !ok || elem["_id"] != "p2" {
		t.Fatalf("the repair must land the NEWLY referenced source document, got %v", cond[1])
	}
}

// A dead reference (the ParentID points at a source document that no longer exists)
// repairs to the explicit null rather than freezing the old sub-document.
func TestLifecycle_EmbedderFKPointingNowhereClearsSegment(t *testing.T) {
	colls := map[string]*fakeColl{"products": {}, "sales": {}}
	repairDanglingOneToOne(context.Background(), upstreamFakeMongo(colls), identityResolver, nil,
		lifeSalesView(), "s1", Document{"id": "s1", "product_id": "gone"}, nil)

	ups := colls["sales"].updates
	if len(ups) != 1 {
		t.Fatalf("want one repair write, got %d", len(ups))
	}
	set := ups[0]["$pipeline"].([]Document)[0]["$set"].(Document)
	cond := set["product"].(Document)["$cond"].([]any)
	if v, has := cond[1].(Document)["$literal"]; !has || v != nil {
		t.Fatalf("a dead view reference must repair to the explicit null, got %v", cond[1])
	}
}

// 1:N lifecycle: a source document that MOVES between parents must leave the
// old parent's array and enter the new one — which is only possible because the
// pre-write document was captured (Before) and both FKs enter the target set.
func TestLifecycle_SourceMovedBetweenParentsTouchesBothSides(t *testing.T) {
	dashboard := View("dashboard").Version(1).
		Schema(core.NewTableSchema[lifeSale]("customers").ID("id").SoftDelete("deleted_at")).
		EmbedMany(JoinView(lifeProductsView(), "Products", "products")).On("owner_id")
	colls := map[string]*fakeColl{
		"products":  {docs: []any{map[string]any{"_id": "p1", "owner_id": "c2", docRevisionField: int64(5)}}},
		"dashboard": {docs: []any{map[string]any{"_id": "c1"}, map[string]any{"_id": "c2"}}},
	}
	mongo := upstreamFakeMongo(colls)
	sig := newViewEmbedSignal(nil, mongo, NewComposerWithMongo(nil, mongo, identityResolver),
		identityResolver, []*ViewDefinition{lifeProductsView(), dashboard}, "g", nil, newUpstreamMetrics())

	// before: the source belonged to c1; after: it belongs to c2.
	sig.WrittenWithBefore(context.Background(), "products", "p1",
		Document{"_id": "p1", "owner_id": "c1"})

	if got := len(colls["dashboard"].updates); got != 2 {
		t.Fatalf("a moved source must rewrite BOTH the old and the new parent, got %d writes", got)
	}
}
