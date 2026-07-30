package query

import (
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Phase 4 — the READ side over a materialized view leg. A JoinView segment
// carries the source view's FULL document (its own children included), so every
// reader-side walk must descend into it exactly as a direct read of that view
// would: Go↔column translation, filter/sort/?fields= path resolution, the
// archived-entry strip, and the tabular export.

type nestLine struct {
	ID    string
	Label string
}

type nestProduct struct {
	ID   string
	Name string
}

type nestSale struct {
	ID        string
	ProductID string
}

type nestCustomer struct{ ID string }

// productWithLines is the SOURCE view's schema: a root with one native child
// collection that carries its own DeletedAt lifecycle.
func productWithLines() *core.TableSchema {
	return core.NewTableSchema[nestProduct]("products").ID("id").DeletedAt("deleted_at").
		Field("Name", "name").
		Child(core.NewTableSchema[nestLine]("product_lines").ID("id").ParentID("products_id").
			DeletedAt("deleted_at").Field("Label", "label"))
}

// lineSeg is the derived doc segment of the source view's child collection —
// computed, never hardcoded (the derivation is from the Go TYPE name).
var lineSeg = childDocSegment(core.NewTableSchema[nestLine]("product_lines").ID("id").ParentID("products_id"))

func productsWithLinesView() *ViewDefinition {
	return View("products").Version(1).Schema(productWithLines())
}

// salesEmbeddingProducts materializes that source 1:1 inside "sales".
func salesEmbeddingProducts() *ViewDefinition {
	return View("sales").Version(1).
		Schema(core.NewTableSchema[nestSale]("sales").ID("id").DeletedAt("deleted_at").
			Field("ProductID", "product_id")).
		Embed(JoinView(productsWithLinesView(), "Product", "product")).On("product_id").
		Indexes(Index("product_id"))
}

// ─── translation at depth ────────────────────────────────────────────────────

// ToGoDoc must translate INSIDE the materialized segment — its root fields and
// its own child collection — or the consumer would receive raw column names for
// everything below the first hop.
func TestNestedViewLeg_ToGoDocTranslatesInsideSegment(t *testing.T) {
	node := salesEmbeddingProducts().BuildViewNode()
	got := node.ToGoDoc(map[string]any{
		"_id":        "s1",
		"product_id": "p1",
		"product": map[string]any{
			"_id":  "p1",
			"name": "Cable",
			lineSeg: []any{
				map[string]any{"_id": "l1", "label": "1m"},
			},
		},
	})
	if got["ProductID"] != "p1" {
		t.Fatalf("root translation lost: %v", got)
	}
	seg, ok := got["Product"].(map[string]any)
	if !ok {
		t.Fatalf("the segment must land under its Go name, got %v", got)
	}
	if seg["Name"] != "Cable" {
		t.Errorf("segment root field not translated: %v", seg)
	}
	lines, ok := seg[lineSeg].([]any)
	if !ok || len(lines) != 1 {
		t.Fatalf("the segment's own child collection must survive translation: %v", seg)
	}
	if lines[0].(map[string]any)["Label"] != "1m" {
		t.Errorf("child inside the segment not translated: %v", lines[0])
	}
}

// The watermark the ripple guards on (_revision, stored inside the segment) is
// framework bookkeeping, never consumer data: the Go translation keeps only
// _id and declared columns, so it cannot reach the wire.
func TestNestedViewLeg_WatermarkNeverReachesTheWire(t *testing.T) {
	got := salesEmbeddingProducts().BuildViewNode().ToGoDoc(map[string]any{
		"_id":        "s1",
		"product_id": "p1",
		"product":    map[string]any{"_id": "p1", "name": "Cable", docRevisionField: int64(7)},
	})
	seg := got["Product"].(map[string]any)
	if _, leaked := seg[docRevisionField]; leaked {
		t.Fatalf("the embedded watermark must not survive translation, got %v", seg)
	}
	if seg["Name"] != "Cable" {
		t.Errorf("declared fields must survive alongside the dropped watermark: %v", seg)
	}
}

// A filter / sort / ?fields= path must resolve THROUGH the segment into the
// source view's own child — the second hop of the path.
func TestNestedViewLeg_ColumnPathResolvesThroughSegment(t *testing.T) {
	node := salesEmbeddingProducts().BuildViewNode()
	got, ok := node.ColumnPath([]string{"Product", lineSeg, "Label"})
	if !ok {
		t.Fatal("a path into the segment's child must resolve")
	}
	if want := []string{"product", lineSeg, "label"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ColumnPath = %v, want %v", got, want)
	}
}

// ─── the archived-entry strip inside the segment ─────────────────────────────

// A direct read of the source view hides its ARCHIVED children. The
// materialized copy must hide them too, or the same data would read
// differently depending on which view you asked for.
func TestNestedViewLeg_StripsArchivedChildrenInsideSegment(t *testing.T) {
	node := salesEmbeddingProducts().BuildViewNode()
	doc := map[string]any{
		"_id":        "s1",
		"product_id": "p1",
		"product": map[string]any{
			"_id": "p1",
			lineSeg: []any{
				map[string]any{"_id": "l1", "label": "live", "deleted_at": nil},
				map[string]any{"_id": "l2", "label": "gone", "deleted_at": "2026-01-01T00:00:00Z"},
			},
		},
	}
	node.StripArchivedChildren(doc)
	lines := doc["product"].(map[string]any)[lineSeg].([]any)
	if len(lines) != 1 || lines[0].(map[string]any)["_id"] != "l1" {
		t.Fatalf("archived children inside a materialized segment must be stripped, got %v", lines)
	}
}

// The same, one multiplicity up: EmbedMany of a view whose documents carry
// children — the array-in-array shape. Every element's child collection strips.
func TestNestedViewLeg_StripsInsideEmbedManyElements(t *testing.T) {
	dashboard := View("dashboard").Version(1).
		Schema(core.NewTableSchema[nestCustomer]("customers").ID("id").DeletedAt("deleted_at")).
		EmbedMany(JoinView(productsWithLinesView(), "Products", "products")).On("owner_id")
	doc := map[string]any{
		"_id": "c1",
		"products": []any{
			map[string]any{
				"_id": "p1",
				lineSeg: []any{
					map[string]any{"_id": "l1", "deleted_at": nil},
					map[string]any{"_id": "l2", "deleted_at": "2026-01-01T00:00:00Z"},
				},
			},
		},
	}
	dashboard.BuildViewNode().StripArchivedChildren(doc)
	el := doc["products"].([]any)[0].(map[string]any)
	lines := el[lineSeg].([]any)
	if len(lines) != 1 {
		t.Fatalf("archived children must strip inside EVERY element of a 1:N view segment, got %v", lines)
	}
}

// A narrowed projection (?fields=) can only strip what the projected entries
// still carry, so the reader auto-includes each lifecycle segment's DeletedAt
// column — including the ones nested inside a materialized view segment.
func TestNestedViewLeg_ChildDeletedAtPathsReachIntoSegment(t *testing.T) {
	paths := salesEmbeddingProducts().BuildViewNode().ChildDeletedAtPaths()
	if got, ok := paths["product."+lineSeg]; !ok || got != "deleted_at" {
		t.Fatalf("the nested child's DeletedAt path must be auto-included, got %v", paths)
	}
}

// An EXTERNAL leg keeps the untouched contract: a mirror's lifecycle belongs to
// its upstream, so nothing inside it is stripped or auto-included.
func TestExternalLeg_SegmentContentsStayUntouched(t *testing.T) {
	v := View("orders").Version(1).Schema(composerRootSchema()).
		Embed(extLeg("upstream_users", "Buyer", "buyer")).On("buyer_id")
	node := v.BuildViewNode()
	if len(node.ChildDeletedAtPaths()) != 0 {
		t.Fatalf("an external mirror segment must contribute no strip paths, got %v", node.ChildDeletedAtPaths())
	}
	doc := map[string]any{"_id": "o1", "buyer": map[string]any{"_id": "u1", "deleted_at": "2026-01-01T00:00:00Z"}}
	node.StripArchivedChildren(doc)
	if doc["buyer"] == nil {
		t.Fatal("an archived mirror document must stay embedded — its lifecycle is the upstream's")
	}
}

// ─── tabular export at depth ─────────────────────────────────────────────────

// The export plan of a view leg is the SOURCE view's full tree re-rooted under
// the embed's segments — children included, exactly like a composed view's
// internal leg.
func TestNestedViewLeg_ExportPlanCarriesSourceTree(t *testing.T) {
	plan := salesEmbeddingProducts().ExportPlan()
	var seg *exportBranch
	for _, c := range plan.Root.Children {
		if c.GoSegment == "Product" {
			seg = &exportBranch{goSegment: c.GoSegment, wireSegment: c.WireSegment, children: len(c.Children)}
			break
		}
	}
	if seg == nil {
		t.Fatalf("the materialized segment must appear in the export plan, got %+v", plan.Root.Children)
	}
	if seg.wireSegment != "product" {
		t.Errorf("wire segment: got %q want %q", seg.wireSegment, "product")
	}
	if seg.children != 1 {
		t.Errorf("the source view's own child collection must export under the segment, got %d branches", seg.children)
	}
}

type exportBranch struct {
	goSegment   string
	wireSegment string
	children    int
}
