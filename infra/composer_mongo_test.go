package infra

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// This file drives composer.go's fetchMongoEmbed path (previously 0%): a
// Composer built with NewComposerWithMongo over a fake Postgres (root row) AND
// a fake MongoDB (external embed docs) composes a view whose embed is an
// external FromSchema source, so the Mongo dispatch branch runs for both the
// EmbedMany and one-to-one Embed shapes, plus the nil-handle and find-error
// branches.

func TestFetchMongoEmbed_EmbedMany(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		// Root order row carries the PK the external child FK references.
		return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}}, nil
	}
	mongoColl := &fakeColl{docs: []any{
		map[string]any{"_id": "u1", "name": "alice"},
		map[string]any{"_id": "u2", "name": "bob"},
	}}
	c := NewComposerWithMongo(newFakePostgres(pool), newFakeMongo(mongoColl))

	external := FromSchema(NewExternalSchema("buyers").PK("ID", "id").FK("order_id")).As("Buyers")
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("buyers", external)

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	buyers, ok := doc["buyers"].([]bson.M)
	if !ok || len(buyers) != 2 {
		t.Fatalf("expected 2 embedded mongo docs, got %v", doc["buyers"])
	}
}

func TestFetchMongoEmbed_OneToOne(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		// Root row carries the FK pointing at the external doc's _id.
		return &composerRows{cols: []string{"id", "buyer_id", "name"}, data: [][]any{{"o1", "u1", "first"}}}, nil
	}
	mongoColl := &fakeColl{docs: []any{map[string]any{"_id": "u1", "name": "alice"}}}
	c := NewComposerWithMongo(newFakePostgres(pool), newFakeMongo(mongoColl))

	external := FromSchema(NewExternalSchema("buyers").PK("ID", "id")).On("buyer_id").As("Buyer")
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		Embed("buyer", external)

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	row, ok := doc["buyer"].(bson.M)
	if !ok || row["name"] != "alice" {
		t.Fatalf("expected one-to-one mongo embed, got %v", doc["buyer"])
	}
}

func TestFetchMongoEmbed_OneToOne_NoMatch(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &composerRows{cols: []string{"id", "buyer_id", "name"}, data: [][]any{{"o1", "u9", "first"}}}, nil
	}
	mongoColl := &fakeColl{docs: nil} // FindManyByField returns empty
	c := NewComposerWithMongo(newFakePostgres(pool), newFakeMongo(mongoColl))

	external := FromSchema(NewExternalSchema("buyers").PK("ID", "id")).On("buyer_id").As("Buyer")
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		Embed("buyer", external)

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, present := doc["buyer"]; present {
		t.Error("no matching mongo doc must omit the embed")
	}
}

func TestFetchMongoEmbed_NilHandle(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}}, nil
	}
	// NewComposer (no Mongo handle) over a view with a Mongo embed → error.
	c := NewComposer(newFakePostgres(pool))
	external := FromSchema(NewExternalSchema("buyers").PK("ID", "id").FK("order_id")).As("Buyers")
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("buyers", external)

	if _, err := c.Compose(context.Background(), view, "o1"); err == nil ||
		!strings.Contains(err.Error(), "requires a MongoDB handle") {
		t.Fatalf("expected missing-handle error, got %v", err)
	}
}

func TestFetchMongoEmbed_FindError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}}, nil
	}
	mongoColl := &fakeColl{findErr: context.Canceled}
	c := NewComposerWithMongo(newFakePostgres(pool), newFakeMongo(mongoColl))
	external := FromSchema(NewExternalSchema("buyers").PK("ID", "id").FK("order_id")).As("Buyers")
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("buyers", external)

	if _, err := c.Compose(context.Background(), view, "o1"); err == nil {
		t.Fatal("expected FindManyByField error to surface from Compose")
	}
}

func TestFetchMongoEmbed_EmbedMany_MissingParentKey(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		// Root row lacks the PK column "id" → EmbedMany skipped.
		return &composerRows{cols: []string{"name"}, data: [][]any{{"first"}}}, nil
	}
	c := NewComposerWithMongo(newFakePostgres(pool), newFakeMongo(&fakeColl{}))
	external := FromSchema(NewExternalSchema("buyers").PK("ID", "id").FK("order_id")).As("Buyers")
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("buyers", external)

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, present := doc["buyers"]; present {
		t.Error("missing parent key must skip the mongo embed")
	}
}
