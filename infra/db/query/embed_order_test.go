package query

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// A declared 1:N element order is MATERIALIZED, so the guarantee that matters is
// not "the array comes out sorted" — it is that EVERY writer of the segment
// emits the identical sort. The three writers are the surgical ripple
// (surgicalManyExpr), the ripple's full-recompose fallback
// (fieldOwnershipStages) and the consult/rebuild path (embedCreateStage, which
// the blue-green backfill also drives). This file pins all three to the same
// expression, because a divergence between them is exactly what would surface
// as an intermittent rebuild-verify failure.

type orderPart struct {
	ID   string
	Slot int
}

func orderedManyEmbed(t *testing.T, desc bool) embedDef {
	t.Helper()
	src := View("parts").Version(1).Schema(
		core.NewTableSchema[orderPart]("parts").PK("id").Field("Slot", "slot")).
		Indexes(Index("kit_id"))
	b := View("kits").Version(1).Schema(composerRootSchema()).
		EmbedMany(JoinView(src, "Parts", "parts")).OrderBy("slot")
	if desc {
		b = b.Desc()
	}
	v := b.On("kit_id")
	return v.embeds[0]
}

// sortSpec extracts the $sortArray sortBy of a segment expression ("" when the
// expression is not sorted).
func sortSpec(t *testing.T, expr any) bson.D {
	t.Helper()
	d, ok := expr.(Document)
	if !ok {
		return nil
	}
	sa, ok := d["$sortArray"].(Document)
	if !ok {
		return nil
	}
	spec, _ := sa["sortBy"].(bson.D)
	return spec
}

func TestEmbedOrder_TotalOrderWithIDTiebreaker(t *testing.T) {
	e := orderedManyEmbed(t, false)
	spec := sortSpec(t, surgicalManyExpr(e, "p1", Document{"_id": "p1", "kit_id": "k1"}, 0))
	if len(spec) != 2 {
		t.Fatalf("the sort must be TOTAL (declared column + _id), got %v", spec)
	}
	if spec[0].Key != "slot" || spec[0].Value != 1 {
		t.Errorf("primary sort key: got %v want slot:1", spec[0])
	}
	if spec[1].Key != "_id" || spec[1].Value != 1 {
		t.Errorf("tiebreaker: got %v want _id:1 — without it two writers can store different arrays for identical state", spec[1])
	}
}

func TestEmbedOrder_DescInvertsOnlyThePrimaryKey(t *testing.T) {
	spec := sortSpec(t, surgicalManyExpr(orderedManyEmbed(t, true), "p1", Document{"_id": "p1", "kit_id": "k1"}, 0))
	if len(spec) != 2 || spec[0].Value != -1 || spec[1].Value != 1 {
		t.Fatalf("Desc must invert the declared column and keep the tiebreaker ascending, got %v", spec)
	}
}

// The three writers must emit the SAME sortBy — the single reason the sort lives
// in the pipeline instead of in Go.
func TestEmbedOrder_EveryWriterEmitsTheSameSort(t *testing.T) {
	e := orderedManyEmbed(t, false)
	orders := embedOrders([]embedDef{e})
	doc := Document{"id": "k1", "parts": []Document{{"_id": "p1", "slot": 2}}}

	fromRipple := sortSpec(t, surgicalManyExpr(e, "p1", Document{"_id": "p1", "kit_id": "k1"}, 0))
	fromFallback := sortSpec(t, fieldOwnershipStages(doc, "id", embedFieldSet([]embedDef{e}), orders)[0]["$set"].(Document)["parts"])
	consult := embedCreateStage(Document{"parts": doc["parts"]}, "id", orders)["$set"].(Document)["parts"].(Document)
	fromConsult := sortSpec(t, consult["$cond"].([]any)[2])

	for name, got := range map[string]bson.D{"fallback": fromFallback, "consult/rebuild": fromConsult} {
		if len(got) != len(fromRipple) {
			t.Fatalf("%s writer sorts differently from the surgical ripple: %v vs %v", name, got, fromRipple)
		}
		for i := range got {
			if got[i].Key != fromRipple[i].Key || got[i].Value != fromRipple[i].Value {
				t.Fatalf("%s writer sorts differently from the surgical ripple: %v vs %v", name, got, fromRipple)
			}
		}
	}
}

// An UNDECLARED order keeps the historical expression untouched — no $sortArray,
// so no deployed view changes shape (or needs MongoDB 5.2).
func TestEmbedOrder_UnorderedSegmentIsUntouched(t *testing.T) {
	src := View("parts").Version(1).Schema(composerRootSchema()).Indexes(Index("kit_id"))
	v := View("kits").Version(1).Schema(composerRootSchema()).
		EmbedMany(JoinView(src, "Parts", "parts")).On("kit_id")
	e := v.embeds[0]
	if sortSpec(t, surgicalManyExpr(e, "p1", Document{"_id": "p1", "kit_id": "k1"}, 0)) != nil {
		t.Fatal("an unordered EmbedMany must emit no $sortArray")
	}
	orders := embedOrders([]embedDef{e})
	if len(orders) != 0 {
		t.Fatalf("an unordered embed contributes no order spec, got %v", orders)
	}
}

// Ordering is materialized INTO the document, so it is projection shape.
func TestEmbedOrder_MovesTheRebuildHash(t *testing.T) {
	build := func(order string, desc bool) string {
		src := View("parts").Version(1).Schema(
			core.NewTableSchema[orderPart]("parts").PK("id").Field("Slot", "slot")).
			Indexes(Index("kit_id"))
		b := View("kits").Version(1).Schema(composerRootSchema()).EmbedMany(JoinView(src, "Parts", "parts"))
		if order != "" {
			b = b.OrderBy(order)
		}
		if desc {
			b = b.Desc()
		}
		return b.On("kit_id").RebuildHash()
	}
	unordered, asc, desc := build("", false), build("slot", false), build("slot", true)
	if unordered == asc {
		t.Error("declaring an order must move the rebuild hash")
	}
	if asc == desc {
		t.Error("flipping the direction must move the rebuild hash")
	}
}

// Boot guards: the order column must exist on the SOURCE, and .Desc() alone is
// a declaration mistake.
func TestEmbedOrder_BootGuards(t *testing.T) {
	src := View("parts").Version(1).Schema(
		core.NewTableSchema[orderPart]("parts").PK("id").Field("Slot", "slot")).
		Indexes(Index("kit_id"))

	bad := View("kits").Version(1).Schema(composerRootSchema()).
		EmbedMany(JoinView(src, "Parts", "parts")).OrderBy("nope").On("kit_id")
	err := ValidateViewSchemas([]*ViewDefinition{bad, src})
	if err == nil || !strings.Contains(err.Error(), "not a column of the embedded source") {
		t.Fatalf("an unknown order column must be rejected at boot, got: %v", err)
	}

	descOnly := View("kits2").Version(1).Schema(composerRootSchema()).
		EmbedMany(JoinView(src, "Parts", "parts")).Desc().On("kit_id")
	err = ValidateViewSchemas([]*ViewDefinition{descOnly, src})
	if err == nil || !strings.Contains(err.Error(), "without .OrderBy") {
		t.Fatalf(".Desc() without .OrderBy must be rejected at boot, got: %v", err)
	}

	good := View("kits3").Version(1).Schema(composerRootSchema()).
		EmbedMany(JoinView(src, "Parts", "parts")).OrderBy("slot").Desc().On("kit_id")
	if err := ValidateViewSchemas([]*ViewDefinition{good, src}); err != nil {
		t.Fatalf("a valid ordered EmbedMany must boot, got: %v", err)
	}
}
