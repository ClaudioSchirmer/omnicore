package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Batched-embed (P11) unit coverage: applyEmbedsBatch resolves a WHOLE batch of
// parents' external embeds in one $in per embed source (via FindManyByFieldIn),
// grouping the results by the join key. These tests drive it through ComposeBatch
// with SEVERAL parents — the multi-parent path the QA rebuild suites reach but
// that no per-row test exercises — and pin it to the SAME null semantics the
// per-row fetchMongoEmbed produces (see composer_mongo_test.go for the per-row
// side).

// rootRowsByID echoes a shaped root row per requested id (the batch root fetch is
// WHERE id IN (...)), so ComposeBatch composes several distinct parents in one pass.
func rootRowsByID(cols []string, byID map[string][]any) *fakeEngine {
	return composerEngine(func(_ string, args []any) ([]map[string]any, error) {
		out := make([]map[string]any, 0, len(args))
		for _, a := range args {
			id, _ := a.(string)
			row, ok := byID[id]
			if !ok {
				continue
			}
			m := make(map[string]any, len(cols))
			for i, c := range cols {
				m[c] = row[i]
			}
			out = append(out, m)
		}
		return out, nil
	})
}

func docByID(docs []Document, pk, id string) Document {
	for _, d := range docs {
		if d[pk] == id {
			return d
		}
	}
	return nil
}

// The core P11 proof: a batch of parents each gets ONLY its own 1:N embed docs
// (grouped by the join key); an embed keyed to neither parent never leaks.
func TestApplyEmbedsBatch_EmbedMany_GroupedPerParent(t *testing.T) {
	eng := rootRowsByID([]string{"id"}, map[string][]any{"o1": {"o1"}, "o2": {"o2"}})
	buyers := &fakeColl{docs: []any{
		map[string]any{"_id": "b1", "order_id": "o1"},
		map[string]any{"_id": "b2", "order_id": "o1"},
		map[string]any{"_id": "b3", "order_id": "o2"},
		map[string]any{"_id": "b4", "order_id": "oX"}, // belongs to neither parent
	}}
	c := NewComposerWithMongo(eng, newFakeMongo(buyers), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).EmbedMany(external).On("order_id")

	docs, err := c.ComposeBatch(context.Background(), view, []string{"o1", "o2"})
	if err != nil {
		t.Fatalf("ComposeBatch: %v", err)
	}
	if b, _ := docByID(docs, "id", "o1")["buyers"].([]Document); len(b) != 2 {
		t.Errorf("o1 must get its 2 buyers, got %v", docByID(docs, "id", "o1")["buyers"])
	}
	if b, _ := docByID(docs, "id", "o2")["buyers"].([]Document); len(b) != 1 {
		t.Errorf("o2 must get its 1 buyer, got %v", docByID(docs, "id", "o2")["buyers"])
	}
	for _, d := range docs {
		b, _ := d["buyers"].([]Document)
		for _, x := range b {
			if x["_id"] == "b4" {
				t.Errorf("an embed keyed to neither parent leaked into %v", d["id"])
			}
		}
	}
}

// 1:1 embed is grouped by _id — each parent gets its own matched doc, no crossover.
func TestApplyEmbedsBatch_OneToOne_GroupedPerParent(t *testing.T) {
	eng := rootRowsByID([]string{"id", "buyer_id"}, map[string][]any{"o1": {"o1", "u1"}, "o2": {"o2", "u2"}})
	buyers := &fakeColl{docs: []any{
		map[string]any{"_id": "u1", "name": "alice"},
		map[string]any{"_id": "u2", "name": "bob"},
	}}
	c := NewComposerWithMongo(eng, newFakeMongo(buyers), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyer", "buyer")
	view := View("orders").Version(1).Schema(composerRootSchema()).Embed(external).On("buyer_id")

	docs, err := c.ComposeBatch(context.Background(), view, []string{"o1", "o2"})
	if err != nil {
		t.Fatalf("ComposeBatch: %v", err)
	}
	if r, _ := docByID(docs, "id", "o1")["buyer"].(Document); r["name"] != "alice" {
		t.Errorf("o1 buyer must be alice, got %v", docByID(docs, "id", "o1")["buyer"])
	}
	if r, _ := docByID(docs, "id", "o2")["buyer"].(Document); r["name"] != "bob" {
		t.Errorf("o2 buyer must be bob, got %v", docByID(docs, "id", "o2")["buyer"])
	}
}

// 1:1 with no matching embed doc → the field is OMITTED (per-row parity).
func TestApplyEmbedsBatch_OneToOne_NoMatchOmits(t *testing.T) {
	eng := rootRowsByID([]string{"id", "buyer_id"}, map[string][]any{"o1": {"o1", "u9"}})
	buyers := &fakeColl{docs: []any{map[string]any{"_id": "u1", "name": "alice"}}}
	c := NewComposerWithMongo(eng, newFakeMongo(buyers), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyer", "buyer")
	view := View("orders").Version(1).Schema(composerRootSchema()).Embed(external).On("buyer_id")

	docs, err := c.ComposeBatch(context.Background(), view, []string{"o1"})
	if err != nil {
		t.Fatalf("ComposeBatch: %v", err)
	}
	v, present := docByID(docs, "id", "o1")["buyer"]
	if !present || v != nil {
		t.Errorf("an unresolved 1:1 embed must write the EXPLICIT null ($set-merged docs would otherwise keep a stale sub-document), got present=%v value=%v", present, v)
	}
}

// 1:1 with a nil/absent ParentID → explicit null, same clearing contract.
func TestApplyEmbedsBatch_OneToOne_NilFKSkips(t *testing.T) {
	eng := rootRowsByID([]string{"id"}, map[string][]any{"o1": {"o1"}}) // no buyer_id column
	c := NewComposerWithMongo(eng, newFakeMongo(&fakeColl{}), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyer", "buyer")
	view := View("orders").Version(1).Schema(composerRootSchema()).Embed(external).On("buyer_id")

	docs, err := c.ComposeBatch(context.Background(), view, []string{"o1"})
	if err != nil {
		t.Fatalf("ComposeBatch: %v", err)
	}
	v, present := docByID(docs, "id", "o1")["buyer"]
	if !present || v != nil {
		t.Errorf("a missing ParentID must write the explicit null, got present=%v value=%v", present, v)
	}
}

// 1:N with a present parent key but no match → the field is SET to an empty slice
// (per-row parity: fetchMongoEmbed assigns the — possibly nil — FindManyByField
// result). A nil/absent parent key would instead skip the field entirely.
func TestApplyEmbedsBatch_EmbedMany_NoMatchEmpty(t *testing.T) {
	eng := rootRowsByID([]string{"id"}, map[string][]any{"o1": {"o1"}})
	buyers := &fakeColl{docs: []any{map[string]any{"_id": "b1", "order_id": "oOther"}}}
	c := NewComposerWithMongo(eng, newFakeMongo(buyers), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).EmbedMany(external).On("order_id")

	docs, err := c.ComposeBatch(context.Background(), view, []string{"o1"})
	if err != nil {
		t.Fatalf("ComposeBatch: %v", err)
	}
	v, present := docByID(docs, "id", "o1")["buyers"]
	if !present {
		t.Error("a 1:N embed with a present parent key must set the field (per-row parity)")
	}
	if b, _ := v.([]Document); len(b) != 0 {
		t.Errorf("a no-match 1:N must be empty, got %v", v)
	}
}

// A FindManyByFieldIn error surfaces out of the batched compose.
func TestApplyEmbedsBatch_FindError(t *testing.T) {
	eng := rootRowsByID([]string{"id"}, map[string][]any{"o1": {"o1"}})
	c := NewComposerWithMongo(eng, newFakeMongo(&fakeColl{findErr: context.Canceled}), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).EmbedMany(external).On("order_id")

	if _, err := c.ComposeBatch(context.Background(), view, []string{"o1"}); err == nil {
		t.Fatal("expected the FindManyByFieldIn error to surface from ComposeBatch")
	}
}

// A batch compose of an embed view with NO Mongo handle errors on the guard,
// matching the per-row fetchMongoEmbed contract.
func TestApplyEmbedsBatch_NilHandle(t *testing.T) {
	eng := rootRowsByID([]string{"id"}, map[string][]any{"o1": {"o1"}})
	c := NewComposer(eng) // no Mongo handle
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).EmbedMany(external).On("order_id")

	if _, err := c.ComposeBatch(context.Background(), view, []string{"o1"}); err == nil ||
		!strings.Contains(err.Error(), "requires a MongoDB handle") {
		t.Fatalf("expected missing-handle error, got %v", err)
	}
}
