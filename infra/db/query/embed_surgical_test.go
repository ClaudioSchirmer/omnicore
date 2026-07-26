package query

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Surgical embed-edit coverage: the per-element forms that make concurrent
// ripples commute (different upstream ids touch disjoint elements) and the
// delete/unresolved contract (1:1 → explicit null; 1:N → element stripped).

func surgicalManyDef(t *testing.T) embedDef {
	t.Helper()
	src := FromSchema(core.NewExternalSchema("upstream_items").
		PK("id").
		Field("Label", "label").
		Field("AccountID", "account_id").
		FK("account_id")).
		As("Items")
	return embedDef{field: "Items", source: src, many: true}
}

func surgicalOneDef(t *testing.T) embedDef {
	t.Helper()
	src := FromSchema(core.NewExternalSchema("upstream_items").
		PK("id").
		Field("Label", "label")).
		FK("featured_item_id").
		As("FeaturedItem")
	return embedDef{field: "FeaturedItem", source: src, many: false}
}

func stageSet(t *testing.T, stages []Document) Document {
	t.Helper()
	if len(stages) != 1 {
		t.Fatalf("want one $set stage, got %d", len(stages))
	}
	set, ok := stages[0]["$set"].(Document)
	if !ok {
		t.Fatalf("want a $set stage, got %v", stages[0])
	}
	return set
}

func TestSurgicalEmbedStages_ManyUpsert(t *testing.T) {
	after := Document{"label": "A2", "account_id": "acc-1"}
	stages := surgicalEmbedStages([]embedDef{surgicalManyDef(t)}, "i2", after)
	set := stageSet(t, stages)
	cond, ok := set["Items"].(Document)["$cond"].([]any)
	if !ok || len(cond) != 3 {
		t.Fatalf("FK present → a parent-conditional edit, got %v", set["Items"])
	}
	// The condition keys on the DOCUMENT's own _id, so one stage set serves
	// every target parent (old and new side of a move alike).
	eq := cond[0].(Document)["$eq"].([]any)
	if eq[0] != "$_id" {
		t.Errorf("the upsert arm must key on the doc's _id, got %v", eq[0])
	}
	concat, ok := cond[1].(Document)["$concatArrays"].([]any)
	if !ok || len(concat) != 2 {
		t.Fatalf("upsert arm must strip+append, got %v", cond[1])
	}
	elem := concat[1].([]any)[0].(Document)["$literal"].(Document)
	if elem["_id"] != "i2" || elem["label"] != "A2" {
		t.Errorf("element must be the mirror doc plus _id, got %v", elem)
	}
	if _, isFilter := cond[2].(Document)["$filter"]; !isFilter {
		t.Errorf("the other parents strip the element, got %v", cond[2])
	}
}

func TestSurgicalEmbedStages_ManyDelete(t *testing.T) {
	stages := surgicalEmbedStages([]embedDef{surgicalManyDef(t)}, "i2", nil)
	set := stageSet(t, stages)
	filter, ok := set["Items"].(Document)["$filter"].(Document)
	if !ok {
		t.Fatalf("delete → strip-only, got %v", set["Items"])
	}
	if _, hasIfNull := filter["input"].(Document)["$ifNull"]; !hasIfNull {
		t.Errorf("strip must tolerate a null/absent array, got %v", filter["input"])
	}
}

func TestSurgicalEmbedStages_OneUpsertAndDelete(t *testing.T) {
	after := Document{"label": "FA"}
	set := stageSet(t, surgicalEmbedStages([]embedDef{surgicalOneDef(t)}, "i9", after))
	cond := set["FeaturedItem"].(Document)["$cond"].([]any)
	eq := cond[0].(Document)["$eq"].([]any)
	if eq[0] != "$featured_item_id" {
		t.Errorf("1:1 keys on the parent's FK column, got %v", eq[0])
	}
	if cond[2] != "$FeaturedItem" {
		t.Errorf("non-referencing parents keep their stored value, got %v", cond[2])
	}

	setDel := stageSet(t, surgicalEmbedStages([]embedDef{surgicalOneDef(t)}, "i9", nil))
	condDel := setDel["FeaturedItem"].(Document)["$cond"].([]any)
	null, ok := condDel[1].(Document)
	if !ok {
		t.Fatalf("delete arm must be a $literal, got %T", condDel[1])
	}
	if v, has := null["$literal"]; !has || v != nil {
		t.Errorf("a deleted 1:1 source must write the explicit null, got %v", null)
	}
}

func TestRepairDanglingOneToOne_HealsAndGuards(t *testing.T) {
	// The written doc references i9, mirror has it → repair writes a guarded
	// $cond: FK still matches AND stored segment _id ≠ FK (so the element's own
	// fresher ripple is never regressed), non-upsert.
	view := View("qa_accounts_view").Version(1).Schema(composerRootSchema()).
		Embed("featuredItem", FromSchema(
			core.NewExternalSchema("upstream_items").PK("id").Field("Label", "label")).
			FK("featured_item_id").As("FeaturedItem"))
	colls := map[string]*fakeColl{
		"upstream_items":   {docs: []any{map[string]any{"_id": "i9", "label": "FA"}}},
		"qa_accounts_view": {},
	}
	mongo := upstreamFakeMongo(colls)
	repairDanglingOneToOne(context.Background(), mongo, identityResolver, nil, view, "acc1",
		Document{"id": "acc1", "featured_item_id": "i9"})

	ups := colls["qa_accounts_view"].updates
	if len(ups) != 1 {
		t.Fatalf("want one repair write, got %d", len(ups))
	}
	if ups[0]["$upsert"] != false {
		t.Errorf("repair must never create documents, got %v", ups[0]["$upsert"])
	}
	set := ups[0]["$pipeline"].([]Document)[0]["$set"].(Document)
	cond := set["featuredItem"].(Document)["$cond"].([]any)
	and := cond[0].(Document)["$and"].([]any)
	if len(and) != 2 {
		t.Fatalf("repair must double-guard (FK match + not-already-this-id), got %v", cond[0])
	}
	elem := cond[1].(Document)["$literal"].(map[string]any)
	if elem["_id"] != "i9" {
		t.Errorf("repair must write the FRESH mirror doc, got %v", elem)
	}
}

func TestRepairDanglingOneToOne_MissingMirrorClearsToNull(t *testing.T) {
	view := View("qa_accounts_view").Version(1).Schema(composerRootSchema()).
		Embed("featuredItem", FromSchema(
			core.NewExternalSchema("upstream_items").PK("id").Field("Label", "label")).
			FK("featured_item_id").As("FeaturedItem"))
	colls := map[string]*fakeColl{
		"upstream_items":   {}, // referenced doc does not exist (yet, or anymore)
		"qa_accounts_view": {},
	}
	mongo := upstreamFakeMongo(colls)
	repairDanglingOneToOne(context.Background(), mongo, identityResolver, nil, view, "acc1",
		Document{"id": "acc1", "featured_item_id": "i9"})

	ups := colls["qa_accounts_view"].updates
	if len(ups) != 1 {
		t.Fatalf("want one repair write, got %d", len(ups))
	}
	set := ups[0]["$pipeline"].([]Document)[0]["$set"].(Document)
	cond := set["featuredItem"].(Document)["$cond"].([]any)
	null := cond[1].(Document)
	if v, has := null["$literal"]; !has || v != nil {
		t.Errorf("a dead reference must repair to the explicit null, got %v", cond[1])
	}
}

func TestRepairDanglingOneToOne_NoFKNoWrite(t *testing.T) {
	view := View("qa_accounts_view").Version(1).Schema(composerRootSchema()).
		Embed("featuredItem", FromSchema(
			core.NewExternalSchema("upstream_items").PK("id").Field("Label", "label")).
			FK("featured_item_id").As("FeaturedItem"))
	colls := map[string]*fakeColl{"qa_accounts_view": {}}
	repairDanglingOneToOne(context.Background(), upstreamFakeMongo(colls), identityResolver, nil, view, "acc1",
		Document{"id": "acc1", "featured_item_id": nil})
	if len(colls["qa_accounts_view"].updates) != 0 {
		t.Errorf("a null FK needs no repair (the composed null was written by the create), got %v", colls["qa_accounts_view"].updates)
	}
}
