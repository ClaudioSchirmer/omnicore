//go:build integration && postgres

package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// --- Composer -------------------------------------------------------------

func TestComposer_ComposeRoot(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()

	createTable(t, pg, `CREATE TABLE c_widgets (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		label TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)

	var id string
	if err := pg.Pool().QueryRow(context.Background(),
		`INSERT INTO c_widgets (label) VALUES ('first') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	view := query.View("c_widgets").Root("c_widgets").Schema(rootSchema("c_widgets")).Version(1)
	c := query.NewComposer(pg)
	doc, err := c.Compose(context.Background(), view, id)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc["label"] != "first" {
		t.Errorf("doc.label = %v, want first", doc["label"])
	}
}

func TestComposer_Compose_AbsentRowReturnsNil(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createTable(t, pg, `CREATE TABLE c_empty (
		id UUID PRIMARY KEY,
		deleted_at TIMESTAMP
	)`)
	view := query.View("c_empty").Root("c_empty").Schema(rootSchema("c_empty")).Version(1)
	c := query.NewComposer(pg)
	doc, err := c.Compose(context.Background(), view, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc != nil {
		t.Errorf("expected nil doc on absent row, got %+v", doc)
	}
}

// The relational EmbedMany integration test was removed with the relational embed
// path (embeds are external-only now). A root's own 1:N child projecting against a
// real DB is covered by TestComposer_ComposeWithOwnChild below.

// TestComposer_ComposeWithOwnChild proves the Phase-1 own-child auto path against
// a real backend: the child is declared on the ROOT schema (no EmbedMany) and must
// project automatically, joined root.PK → child.FK, mirroring hydrateChildren.
func TestComposer_ComposeWithOwnChild(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()

	createTable(t, pg, `CREATE TABLE oc_orders (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		ref TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE oc_lines (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		order_id UUID NOT NULL REFERENCES oc_orders(id) ON DELETE CASCADE,
		qty INT NOT NULL,
		deleted_at TIMESTAMP
	)`)

	var orderID string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO oc_orders (ref) VALUES ('R-1') RETURNING id`).Scan(&orderID)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO oc_lines (order_id, qty) VALUES ($1, 3), ($1, 5)`, orderID)

	// Child declared on the ROOT schema — NO EmbedMany. It must auto-project.
	rootWithChild := core.NewTableSchema[embedFixture]("oc_orders").PK("id").SoftDelete("deleted_at").
		Child(core.NewTableSchema[ocLineRow]("oc_lines").PK("id").FK("order_id").
			Field("Qty", "qty").SoftDelete("deleted_at"))
	view := query.View("oc_orders").Root("oc_orders").Schema(rootWithChild).Version(1)

	doc, err := query.NewComposer(pg).Compose(context.Background(), view, orderID)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	seg := domain.PluralizeWord("ocLineRow") // childDocSegment derivation, exported form
	lines, ok := doc[seg].([]query.Document)
	if !ok {
		t.Fatalf("own child %q shape = %T (doc=%v)", seg, doc[seg], doc)
	}
	if len(lines) != 2 {
		t.Errorf("auto-projected own children = %d, want 2", len(lines))
	}
}

type ocLineRow struct {
	ID  string
	Qty int
}

func TestComposer_ComposeAll(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createTable(t, pg, `CREATE TABLE c_items (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO c_items (name) VALUES ('a'), ('b'), ('c')`)

	view := query.View("c_items").Root("c_items").Schema(rootSchema("c_items")).Version(1)
	c := query.NewComposer(pg)
	docs, err := c.ComposeAll(context.Background(), view)
	if err != nil {
		t.Fatalf("ComposeAll: %v", err)
	}
	if len(docs) != 3 {
		t.Errorf("expected 3 docs, got %d", len(docs))
	}
}

func TestComposer_DeleteOnArchive_FiltersDeletedAtFromRoot(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createTable(t, pg, `CREATE TABLE c_keep (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)

	// One active + one archived.
	var activeID, archivedID string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO c_keep (name) VALUES ('active') RETURNING id`).Scan(&activeID)
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO c_keep (name, deleted_at) VALUES ('archived', NOW()) RETURNING id`).Scan(&archivedID)

	// Default view (keep-archived) — should see both.
	defaultView := query.View("c_keep").Root("c_keep").Schema(rootSchema("c_keep")).Version(1)
	c := query.NewComposer(pg)
	if doc, err := c.Compose(context.Background(), defaultView, archivedID); err != nil || doc == nil {
		t.Errorf("default view should return the archived row, got doc=%v err=%v", doc, err)
	}

	// DeleteOnArchive view — should skip the archived row.
	hotView := query.View("c_keep").Root("c_keep").Schema(rootSchema("c_keep")).DeleteOnArchive().Version(1)
	if doc, err := c.Compose(context.Background(), hotView, archivedID); err != nil {
		t.Errorf("DeleteOnArchive Compose returned err = %v", err)
	} else if doc != nil {
		t.Errorf("DeleteOnArchive view should NOT return archived doc, got %+v", doc)
	}
	if doc, err := c.Compose(context.Background(), hotView, activeID); err != nil || doc == nil {
		t.Errorf("active doc should always be returned, got doc=%v err=%v", doc, err)
	}
}

// --- MongoDB --------------------------------------------------------------

func TestMongoDB_UpsertAndDelete(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	ctx := context.Background()
	if err := m.Upsert(ctx, "things", "id-1", bson.M{"name": "alice"}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	doc := mongoDoc(t, m, "things", "id-1")
	if doc == nil || doc["name"] != "alice" {
		t.Errorf("first upsert: %+v", doc)
	}

	// Upsert again replaces fields.
	if err := m.Upsert(ctx, "things", "id-1", bson.M{"name": "alice2"}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	doc = mongoDoc(t, m, "things", "id-1")
	if doc["name"] != "alice2" {
		t.Errorf("second upsert: %+v", doc)
	}

	if err := m.Delete(ctx, "things", "id-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if mongoDoc(t, m, "things", "id-1") != nil {
		t.Error("Delete did not remove the doc")
	}
}

func TestNewMongoDB_BadURIFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()
	if _, err := NewMongoDB(ctx, "mongodb://nobody:1/none", "db"); err == nil {
		t.Error("expected NewMongoDB to fail on bad URI / unreachable server")
	}
}

// --- MongoViewReader -----------------------------------------------------

func TestMongoViewReader_ReadByID_HitMissAndArchivedFilter(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	_, err := col.InsertMany(ctx, []any{
		bson.M{"_id": "u1", "email": "alice@x", "deleted_at": nil},
		bson.M{"_id": "u2", "email": "bob@x", "deleted_at": "2026-01-01"}, // archived
	})
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}

	// The reader needs the view schema to know deleted_at is the soft-delete
	// marker (and to translate columns to Go field names on read); production
	// always registers it via SetViews. Without it the by-id archived filter
	// cannot engage.
	reader := NewMongoViewReader(m).SetViews([]*query.ViewDefinition{
		query.View("users").Root("users").
			Schema(core.NewExternalSchema("users").PK("id").Field("Email", "email").SoftDelete("deleted_at")).
			Version(1),
	})

	// Hit + active. With a schema, the doc is keyed by Go field name (Email).
	doc, ok, err := reader.ReadByID(ctx, "users", "u1", queries.ReadCriteria{})
	if err != nil || !ok || doc["Email"] != "alice@x" {
		t.Errorf("ReadByID active = (%+v, %v, %v)", doc, ok, err)
	}

	// Archived hidden by default.
	doc, ok, err = reader.ReadByID(ctx, "users", "u2", queries.ReadCriteria{})
	if err != nil || ok || doc != nil {
		t.Errorf("ReadByID archived without IncludeArchived = (%+v, %v, %v), want absent", doc, ok, err)
	}

	// Archived visible via IncludeArchived.
	doc, ok, err = reader.ReadByID(ctx, "users", "u2", queries.ReadCriteria{IncludeArchived: true})
	if err != nil || !ok || doc["Email"] != "bob@x" {
		t.Errorf("ReadByID with IncludeArchived = (%+v, %v, %v)", doc, ok, err)
	}

	// Miss.
	doc, ok, err = reader.ReadByID(ctx, "users", "ghost", queries.ReadCriteria{})
	if err != nil || ok || doc != nil {
		t.Errorf("ReadByID miss = (%+v, %v, %v)", doc, ok, err)
	}
}

func TestMongoViewReader_ReadPage_HappyPath(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	for _, e := range []string{"a", "b", "c", "d", "e"} {
		_, err := col.InsertOne(ctx, bson.M{"_id": e, "email": e + "@x", "deleted_at": nil})
		if err != nil {
			t.Fatalf("insert %s: %v", e, err)
		}
	}

	reader := NewMongoViewReader(m)
	page, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{Limit: 3})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 3 {
		t.Errorf("expected 3 items on first page, got %d", len(page.Items))
	}
	if !page.HasNext {
		t.Error("expected HasNext=true on a 3-of-5 page")
	}
	if page.Total != 5 {
		t.Errorf("Total = %d, want 5", page.Total)
	}
	if page.NextCursor == "" {
		t.Error("expected NextCursor populated when HasNext=true")
	}

	// Follow the cursor.
	page2, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{Limit: 3, After: page.NextCursor})
	if err != nil {
		t.Fatalf("ReadPage page2: %v", err)
	}
	if !page2.HasPrev {
		t.Error("expected HasPrev=true on a follow page")
	}
	if page2.HasNext {
		t.Errorf("expected HasNext=false on the last page, got items=%d", len(page2.Items))
	}
}

func TestMongoViewReader_ReadPage_FilterWithMultiClause(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	col.InsertMany(ctx, []any{
		bson.M{"_id": "1", "age": 10, "deleted_at": nil},
		bson.M{"_id": "2", "age": 25, "deleted_at": nil},
		bson.M{"_id": "3", "age": 40, "deleted_at": nil},
		bson.M{"_id": "4", "age": 80, "deleted_at": nil},
	})

	reader := NewMongoViewReader(m)
	page, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{
		Filter: map[string]any{
			"age": queries.MultiClause{Clauses: []any{
				bson.M{"$gte": 20},
				bson.M{"$lte": 50},
			}},
		},
	})
	if err != nil {
		t.Fatalf("ReadPage with MultiClause: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("expected 2 items (ages 25 and 40), got %d", len(page.Items))
	}
}

func TestMongoViewReader_ReadPage_Sort(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	col.InsertMany(ctx, []any{
		bson.M{"_id": "1", "score": 30, "deleted_at": nil},
		bson.M{"_id": "2", "score": 10, "deleted_at": nil},
		bson.M{"_id": "3", "score": 20, "deleted_at": nil},
	})

	reader := NewMongoViewReader(m)
	page, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{
		Sort: []queries.SortField{{Field: "score", Desc: true}},
	})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if got := page.Items[0]["score"]; toInt(got) != 30 {
		t.Errorf("first item score = %v, want 30 (descending)", got)
	}
	if got := page.Items[2]["score"]; toInt(got) != 10 {
		t.Errorf("third item score = %v, want 10", got)
	}
}

func TestMongoViewReader_ReadPage_Projection(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	col.InsertOne(ctx, bson.M{"_id": "1", "email": "a@x", "name": "alice", "deleted_at": nil})

	reader := NewMongoViewReader(m)
	page, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{
		Projection: map[string]int{"email": 1},
	})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(page.Items))
	}
	item := page.Items[0]
	if _, hasEmail := item["email"]; !hasEmail {
		t.Error("expected email projected")
	}
	if _, hasName := item["name"]; hasName {
		t.Error("name should be excluded by projection")
	}
}

func TestMongoViewReader_ReadPage_RegexMatchSentinel(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	col.InsertMany(ctx, []any{
		bson.M{"_id": "1", "name": "Bob Diego", "deleted_at": nil},
		bson.M{"_id": "2", "name": "Carol", "deleted_at": nil},
		bson.M{"_id": "3", "name": "bobby", "deleted_at": nil},
	})

	reader := NewMongoViewReader(m)
	page, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{
		Filter: map[string]any{
			"name": queries.RegexMatch{Pattern: "^bob", CaseInsensitive: true},
		},
	})
	if err != nil {
		t.Fatalf("ReadPage RegexMatch: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("expected 2 matches (Bob Diego + bobby), got %d", len(page.Items))
	}
}

func TestMongoViewReader_ReadPage_RegexMatchListSentinel(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	col.InsertMany(ctx, []any{
		bson.M{"_id": "1", "name": "Bob", "deleted_at": nil},
		bson.M{"_id": "2", "name": "alice", "deleted_at": nil},
		bson.M{"_id": "3", "name": "carlos", "deleted_at": nil},
	})

	reader := NewMongoViewReader(m)
	page, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{
		Filter: map[string]any{
			"name": queries.RegexMatchList{
				Patterns:        []string{"^bob$", "^alice$"},
				CaseInsensitive: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("ReadPage RegexMatchList: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("expected 2 matches, got %d", len(page.Items))
	}
}

func TestMongoViewReader_DefaultLimitWhenZero(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	// Insert more than the framework default ceiling (100) so the "no ?limit="
	// default cap is observable.
	for i := 0; i < 105; i++ {
		col.InsertOne(ctx, bson.M{"_id": i, "deleted_at": nil})
	}

	reader := NewMongoViewReader(m)
	page, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{}) // Limit unset
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	// With no per-view / yaml override, the ceiling is
	// FrameworkDefaultMaxReadLimit (100); an absent limit defers to the ceiling.
	if len(page.Items) != 100 {
		t.Errorf("default page size = %d, want 100 (framework ceiling)", len(page.Items))
	}
	if !page.HasNext {
		t.Error("expected HasNext=true with 105 items under a 100 ceiling")
	}
}

func TestMongoViewReader_BadCursorRejected(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()

	col := m.Collection("users")
	ctx := context.Background()
	col.InsertOne(ctx, bson.M{"_id": "1", "deleted_at": nil})

	reader := NewMongoViewReader(m)
	// A malformed cursor is strictly rejected (keyset contract: an invalid
	// cursor surfaces as an error, mapped to the canonical 400 upstream —
	// never silently ignored).
	if _, err := reader.ReadPage(ctx, "users", queries.ReadCriteria{After: "garbage-cursor"}); err == nil {
		t.Error("expected an error for a malformed cursor, got nil")
	}
}

// Cursor encode/decode round-trip + edge cases live in
// application/queries/cursor_test.go, exercising the keyset cursor API
// (queries.EncodeCursor / DecodeCursor). The old simple-string
// encodeCursor/decodeCursor helpers were removed, so the tests that
// referenced them are gone — coverage moved with the API.

// --- normalizeSQLValue: UUID -----------------------------------------------

func TestComposer_NormalizeUUID_TurnedIntoString(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createTable(t, pg, `CREATE TABLE c_uuid (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		other UUID,
		deleted_at TIMESTAMP
	)`)
	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO c_uuid (other) VALUES (gen_random_uuid()) RETURNING id`).Scan(&id)

	view := query.View("c_uuid").Root("c_uuid").Schema(rootSchema("c_uuid")).Version(1)
	c := query.NewComposer(pg)
	doc, err := c.Compose(context.Background(), view, id)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, ok := doc["other"].(string); !ok {
		t.Errorf("UUID column should be normalized to string, got %T = %v", doc["other"], doc["other"])
	}
}

// --- helpers --------------------------------------------------------------

func toInt(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	}
	return -1
}
