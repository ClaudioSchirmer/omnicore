package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// This file drives composer.go's fetchMongoEmbed path: a Composer built with
// NewComposerWithMongo over a fakeEngine (root row via QueryMaps) AND a fake
// MongoDB (external embed docs) composes a view whose embed is an external
// FromSchema source, so the Mongo dispatch branch runs for both the EmbedMany
// and one-to-one Embed shapes, plus the nil-handle and find-error branches.

// rootMapsEngine builds a composer engine whose root fetch returns one shaped row.
func rootMapsEngine(cols []string, data [][]any) *fakeEngine {
	return composerEngine(func(string, []any) ([]map[string]any, error) {
		return mapsFromColsData(cols, data), nil
	})
}

func TestFetchMongoEmbed_EmbedMany(t *testing.T) {
	eng := rootMapsEngine([]string{"id", "name"}, [][]any{{"o1", "first"}})
	mongoColl := &fakeColl{docs: []any{
		map[string]any{"_id": "u1", "name": "alice"},
		map[string]any{"_id": "u2", "name": "bob"},
	}}
	c := NewComposerWithMongo(eng, newFakeMongo(mongoColl), identityResolver)

	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		EmbedMany(external).On("order_id")

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	buyers, ok := doc["buyers"].([]Document)
	if !ok || len(buyers) != 2 {
		t.Fatalf("expected 2 embedded mongo docs, got %v", doc["buyers"])
	}
}

func TestFetchMongoEmbed_OneToOne(t *testing.T) {
	eng := rootMapsEngine([]string{"id", "buyer_id", "name"}, [][]any{{"o1", "u1", "first"}})
	mongoColl := &fakeColl{docs: []any{map[string]any{"_id": "u1", "name": "alice"}}}
	c := NewComposerWithMongo(eng, newFakeMongo(mongoColl), identityResolver)

	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyer", "buyer")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		Embed(external).On("buyer_id")

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	row, ok := doc["buyer"].(Document)
	if !ok || row["name"] != "alice" {
		t.Fatalf("expected one-to-one mongo embed, got %v", doc["buyer"])
	}
}

func TestFetchMongoEmbed_OneToOne_NoMatch(t *testing.T) {
	eng := rootMapsEngine([]string{"id", "buyer_id", "name"}, [][]any{{"o1", "u9", "first"}})
	mongoColl := &fakeColl{docs: nil} // FindManyByField returns empty
	c := NewComposerWithMongo(eng, newFakeMongo(mongoColl), identityResolver)

	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyer", "buyer")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		Embed(external).On("buyer_id")

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	v, present := doc["buyer"]
	if !present || v != nil {
		t.Errorf("an unresolved 1:1 embed must write the explicit null (clears a stale sub-document under $set-merge), got present=%v value=%v", present, v)
	}
}

func TestFetchMongoEmbed_OneToOne_MissingFK(t *testing.T) {
	// Root row lacks the buyer_id ParentID column → explicit null, same clearing contract.
	eng := rootMapsEngine([]string{"id", "name"}, [][]any{{"o1", "first"}})
	c := NewComposerWithMongo(eng, newFakeMongo(&fakeColl{}), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyer", "buyer")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		Embed(external).On("buyer_id")

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	v, present := doc["buyer"]
	if !present || v != nil {
		t.Errorf("a missing ParentID must write the explicit null, got present=%v value=%v", present, v)
	}
}

func TestFetchMongoEmbed_OneToOne_FindError(t *testing.T) {
	eng := rootMapsEngine([]string{"id", "buyer_id", "name"}, [][]any{{"o1", "u1", "first"}})
	c := NewComposerWithMongo(eng, newFakeMongo(&fakeColl{findErr: context.Canceled}), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyer", "buyer")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		Embed(external).On("buyer_id")

	if _, err := c.Compose(context.Background(), view, "o1"); err == nil {
		t.Fatal("expected one-to-one FindManyByField error to surface")
	}
}

func TestFetchMongoEmbed_NilHandle(t *testing.T) {
	// NewComposer (no Mongo handle) over a view with a Mongo embed → error.
	eng := rootMapsEngine([]string{"id", "name"}, [][]any{{"o1", "first"}})
	c := NewComposer(eng)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		EmbedMany(external).On("order_id")

	if _, err := c.Compose(context.Background(), view, "o1"); err == nil ||
		!strings.Contains(err.Error(), "requires a MongoDB handle") {
		t.Fatalf("expected missing-handle error, got %v", err)
	}
}

func TestFetchMongoEmbed_FindError(t *testing.T) {
	eng := rootMapsEngine([]string{"id", "name"}, [][]any{{"o1", "first"}})
	c := NewComposerWithMongo(eng, newFakeMongo(&fakeColl{findErr: context.Canceled}), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		EmbedMany(external).On("order_id")

	if _, err := c.Compose(context.Background(), view, "o1"); err == nil {
		t.Fatal("expected FindManyByField error to surface from Compose")
	}
}

func TestFetchMongoEmbed_EmbedMany_MissingParentKey(t *testing.T) {
	// Root row lacks the ID column "id" → EmbedMany skipped.
	eng := rootMapsEngine([]string{"name"}, [][]any{{"first"}})
	c := NewComposerWithMongo(eng, newFakeMongo(&fakeColl{}), identityResolver)
	external := JoinUpstream(core.NewExternalSchema("buyers").ID("id"), "Buyers", "buyers")
	view := View("orders").Version(1).Schema(composerRootSchema()).
		EmbedMany(external).On("order_id")

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, present := doc["buyers"]; present {
		t.Error("missing parent key must skip the mongo embed")
	}
}
