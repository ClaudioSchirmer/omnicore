package query

import (
	"testing"
	"time"
)

// Field-ownership coverage: the recompose-ripple's write shape — embeds
// unconditionally (the ripple owns them), everything else only on document
// creation — so the ripple can never regress the SyncEngine's freshly-read
// relational fields (the lost-update the oracle lane's CDC skew exposed on
// EmbedMany segments). The SyncEngine's side of the split lives in
// consultGuardedStages and is covered by its own tests.

func ownershipDoc() Document {
	return Document{
		"_id":          "p1",
		"id":           "p1",
		"display_name": "Primary",
		"Items":        []Document{{"label": "A1"}},
		"FeaturedItem": Document{"label": "FA"},
		"updated_at":   time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
	}
}

func ownershipEmbeds() map[string]struct{} {
	return map[string]struct{}{"Items": {}, "FeaturedItem": {}}
}

func assertLiteral(t *testing.T, set Document, key string) {
	t.Helper()
	v, ok := set[key].(Document)
	if !ok {
		t.Fatalf("%q: want a Document value, got %T", key, set[key])
	}
	if _, isLit := v["$literal"]; !isLit {
		t.Errorf("%q: owned field must be written as $literal, got %v", key, v)
	}
}

func assertKeptUnlessCreating(t *testing.T, set Document, key string) {
	t.Helper()
	v, ok := set[key].(Document)
	if !ok {
		t.Fatalf("%q: want a Document value, got %T", key, set[key])
	}
	cond, isCond := v["$cond"].([]any)
	if !isCond || len(cond) != 3 {
		t.Fatalf("%q: unowned field must be a 3-arm $cond, got %v", key, v)
	}
	if cond[1] != "$"+key {
		t.Errorf("%q: existing doc must keep its stored value, got %v", key, cond[1])
	}
	if lit, ok := cond[2].(Document); !ok {
		t.Errorf("%q: creation arm must carry the composed value, got %T", key, cond[2])
	} else if _, isLit := lit["$literal"]; !isLit {
		t.Errorf("%q: creation arm must be $literal-wrapped, got %v", key, lit)
	}
}

func TestFieldOwnershipStages_RippleOwnsEmbeds(t *testing.T) {
	stages := fieldOwnershipStages(ownershipDoc(), "id", ownershipEmbeds())
	if len(stages) != 1 {
		t.Fatalf("want one atomic $set stage, got %d", len(stages))
	}
	set := stages[0]["$set"].(Document)
	if _, has := set["_id"]; has {
		t.Error("_id must never be written by the pipeline")
	}
	assertLiteral(t, set, "Items")
	assertLiteral(t, set, "FeaturedItem")
	assertKeptUnlessCreating(t, set, "display_name")
	assertKeptUnlessCreating(t, set, "updated_at")
	assertKeptUnlessCreating(t, set, "id")
}

func TestEmbedFieldSet(t *testing.T) {
	if embedFieldSet(nil) != nil {
		t.Error("no embeds → nil set")
	}
	got := embedFieldSet([]embedDef{{field: "Items"}, {field: "FeaturedItem", many: false}})
	if len(got) != 2 {
		t.Fatalf("want both segments, got %v", got)
	}
	if _, ok := got["Items"]; !ok {
		t.Errorf("missing Items in %v", got)
	}
}
