package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The Composer reads through the engine's neutral surface — c.eng.Querier().
// QueryMaps — so these tests script a fakeEngine whose QueryMaps returns the
// column-keyed maps each SELECT would yield. The dynamic-shape read replaces the
// old pgx FieldDescriptions()+Values() machinery (that pgx-specific path is now
// tested directly in infra/db/pg).

func composerRootSchema() *core.TableSchema {
	return core.NewTableSchema[*builderTestEntity]("orders").
		PK("id").
		Field("Name", "name").
		SoftDelete("deleted_at")
}

// composerEngine builds a fakeEngine whose QueryMaps is driven by mapsFn.
func composerEngine(mapsFn func(sql string, args []any) ([]map[string]any, error)) *fakeEngine {
	return newFakeEngine(&fakeQuerier{queryMapsFn: mapsFn})
}

// composerBoolEntity backs the bool-coercion tests: MySQL returns a TINYINT(1)
// as int64 on the dynamic QueryMaps path, and the composer must restore it to a
// real bool from the type-anchored schema.
type composerBoolEntity struct {
	domain.BaseEntity
	Active   bool
	Verified *bool
	Name     string
}

func composerBoolSchema() *core.TableSchema {
	return core.NewTableSchema[*composerBoolEntity]("flags").
		PK("id").
		Field("Active", "active").
		Field("Verified", "verified").
		Field("Name", "name")
}

func TestBoolColumns(t *testing.T) {
	got := composerBoolSchema().BoolColumns()
	if len(got) != 2 || !got["active"] || !got["verified"] {
		t.Fatalf("BoolColumns = %v, want {active, verified}", got)
	}
	if got["name"] {
		t.Error("name is a string column, must not be reported as bool")
	}
	// External (type-less) schema has no Go struct to reflect → empty.
	if ext := core.NewExternalSchema("flags").PK("id").Field("Active", "active").BoolColumns(); len(ext) != 0 {
		t.Errorf("external schema BoolColumns = %v, want empty", ext)
	}
}

// A MySQL-shaped read (TINYINT(1) → int64) composes into a real BSON bool, not a
// number; a SQL NULL bool stays nil; a string column is untouched.
func TestCompose_CoercesBoolColumns(t *testing.T) {
	eng := composerEngine(func(string, []any) ([]map[string]any, error) {
		return mapsFromColsData(
			[]string{"id", "active", "verified", "name"},
			[][]any{{"o1", int64(1), nil, "first"}}), nil
	})
	c := NewComposer(eng)
	view := View("flags").Version(1).Root("flags").Schema(composerBoolSchema())

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if v, ok := doc["active"].(bool); !ok || v != true {
		t.Errorf("active = %#v, want bool(true)", doc["active"])
	}
	if doc["verified"] != nil {
		t.Errorf("verified (SQL NULL) = %#v, want nil", doc["verified"])
	}
	if doc["name"] != "first" {
		t.Errorf("name = %#v, want \"first\"", doc["name"])
	}
}

func TestCompose_RootMissing(t *testing.T) {
	eng := composerEngine(func(string, []any) ([]map[string]any, error) {
		return nil, nil // zero rows
	})
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc != nil {
		t.Errorf("missing root must yield nil doc, got %v", doc)
	}
}

func TestCompose_QueryError(t *testing.T) {
	eng := composerEngine(func(string, []any) ([]map[string]any, error) {
		return nil, errFake
	})
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	if _, err := c.Compose(context.Background(), view, "o1"); err == nil {
		t.Fatal("expected query error from Compose")
	}
}

func TestCompose_RootOnly(t *testing.T) {
	eng := composerEngine(func(string, []any) ([]map[string]any, error) {
		return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "first"}}), nil
	})
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc == nil || doc["id"] != "o1" || doc["name"] != "first" {
		t.Errorf("root doc drifted: %v", doc)
	}
}

// The relational embed path (fetchPGEmbed) was removed when embeds were narrowed
// to external sources only (Embed/EmbedMany compose read models / derived
// projections, never a write-anchored schema — a local aggregate's own data
// projects automatically). The former relational EmbedMany / one-to-one Embed
// compose tests are gone with it; the external (Mongo) embed path is covered in
// composer_mongo_test.go, and own-children projection in TestCompose_OwnChildren*.

// composerSiblingRootSchema splits builderTestEntity across an anchor table
// "orders" (name) and a sibling table "orders_ext" (email), sharing the id.
func composerSiblingRootSchema() *core.TableSchema {
	return core.NewTableSchema[*builderTestEntity]("orders").
		PK("id").
		Field("Name", "name").
		SoftDelete("deleted_at").
		Sibling(core.NewSiblingSchema[*builderTestEntity]("orders_ext").Field("Email", "email"))
}

// A sibling's columns merge FLAT into the owner doc (D1): email lands at the
// root level, not nested.
func TestCompose_MergesSiblingFlat(t *testing.T) {
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM orders_ext"):
			return mapsFromColsData([]string{"id", "email"}, [][]any{{"o1", "a@x"}}), nil
		case strings.Contains(sql, "FROM orders"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "first"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerSiblingRootSchema())

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc["name"] != "first" || doc["email"] != "a@x" {
		t.Errorf("expected flat doc {name:first, email:a@x}, got %v", doc)
	}
	if _, nested := doc["orders_ext"]; nested {
		t.Errorf("sibling must merge FLAT, not nest under its table name: %v", doc)
	}
}

// The ViewNode translates a sibling field both ways: Go→column for filter/sort/
// projection pushdown, and column→Go in ToGoDoc so the merged sibling column
// reaches the response instead of being dropped.
func TestViewNode_SiblingFieldTranslatesBothWays(t *testing.T) {
	view := View("orders").Version(1).Root("orders").Schema(composerSiblingRootSchema())
	node := view.BuildViewNode()

	cp, ok := node.ColumnPath([]string{"Email"}) // Email lives in the sibling table
	if !ok || len(cp) != 1 || cp[0] != "email" {
		t.Errorf("ColumnPath([Email]) = %v,%v — want [email],true", cp, ok)
	}
	got := node.ToGoDoc(map[string]any{"id": "o1", "name": "first", "email": "a@x"})
	if got["Email"] != "a@x" {
		t.Errorf("ToGoDoc must keep the merged sibling column as a Go field: %v", got)
	}
}

// An absent sibling row leaves its fields omitted — never forced empty (C3).
func TestCompose_AbsentSiblingOmitsFields(t *testing.T) {
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM orders_ext"):
			return nil, nil // sibling slice absent for this row
		case strings.Contains(sql, "FROM orders"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "first"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerSiblingRootSchema())

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, present := doc["email"]; present {
		t.Errorf("an absent sibling must omit its fields, got email=%v", doc["email"])
	}
}

// composerRoleSchema is a role (aluno) over builderTestEntity: Email is role-own,
// Name lives on the shared base pessoa (natural key on name), linked by pessoa_id.
func composerRoleSchema() *core.TableSchema {
	base := core.NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name")
	return core.NewTableSchema[*builderTestEntity]("aluno").
		PK("id").
		Field("Email", "email").
		SharedBase(base, "pessoa_id")
}

// A shared base's columns merge FLAT into the role doc (M2), fetched by the
// role's FK to the base's deterministic id.
func TestCompose_MergesSharedBaseFlat(t *testing.T) {
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"p1", "Ana"}}), nil
		case strings.Contains(sql, "FROM aluno"):
			return mapsFromColsData([]string{"id", "email", "pessoa_id"}, [][]any{{"a1", "a@x", "p1"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)
	view := View("aluno").Version(1).Root("aluno").Schema(composerRoleSchema())

	doc, err := c.Compose(context.Background(), view, "a1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc["email"] != "a@x" || doc["name"] != "Ana" {
		t.Errorf("expected flat doc with role + base fields {email:a@x, name:Ana}, got %v", doc)
	}
	if _, nested := doc["pessoa"]; nested {
		t.Errorf("shared base must merge FLAT, not nest: %v", doc)
	}
}

// A2b compose coverage: a child that carries a sibling gets the sibling merged
// FLAT into each child row (one level below the root).
type csComposeVO struct {
	ID    string
	Label string
	Note  string
}

func (v csComposeVO) GetID() domain.ID                                 { return domain.NewID(v.ID) }
func (v csComposeVO) BuildRules(string, domain.Service, *domain.Rules) {}

// TestCompose_OwnChildrenAutoNested proves a view whose ROOT schema declares a
// Child(...) projects that collection automatically — NO EmbedMany — under the
// derived segment, mirroring hydrateChildren on the write side. Each child row
// also gets its own sibling merged FLAT (shape #4) on this auto path.
func TestCompose_OwnChildrenAutoNested(t *testing.T) {
	childSchema := core.NewTableSchema[csComposeVO]("lines").PK("id").FK("order_id").Field("Label", "label").
		Sibling(core.NewSiblingSchema[csComposeVO]("lines_ext").Field("Note", "note"))
	rootWithChild := core.NewTableSchema[*builderTestEntity]("orders").
		PK("id").Field("Name", "name").SoftDelete("deleted_at").
		Child(childSchema)
	view := View("orders").Version(1).Root("orders").Schema(rootWithChild) // no EmbedMany

	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM lines_ext"): // check before "FROM lines" (substring)
			return mapsFromColsData([]string{"id", "note"}, [][]any{{"l1", "NOTE"}}), nil
		case strings.Contains(sql, "FROM lines"):
			return mapsFromColsData([]string{"id", "order_id", "label"}, [][]any{{"l1", "o1", "L1"}}), nil
		case strings.Contains(sql, "FROM orders"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "first"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	seg := childDocSegment(childSchema)
	lines, ok := doc[seg].([]Document)
	if !ok {
		t.Fatalf("own child collection %q shape = %T (doc=%v)", seg, doc[seg], doc)
	}
	if len(lines) != 1 {
		t.Fatalf("own children = %d, want 1", len(lines))
	}
	if lines[0]["label"] != "L1" || lines[0]["note"] != "NOTE" {
		t.Errorf("own-child row must carry its columns + FLAT sibling (#4): %v", lines[0])
	}
}

func TestComposeAll(t *testing.T) {
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		if strings.Contains(sql, "FROM orders") {
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "a"}, {"o2", "b"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	docs, err := c.ComposeAll(context.Background(), view)
	if err != nil {
		t.Fatalf("ComposeAll: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("ComposeAll = %d docs, want 2", len(docs))
	}
}

func TestComposeAll_QueryError(t *testing.T) {
	eng := composerEngine(func(string, []any) ([]map[string]any, error) { return nil, errFake })
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())
	if _, err := c.ComposeAll(context.Background(), view); err == nil {
		t.Fatal("expected query error from ComposeAll")
	}
}

// composerRoleSchemaManaged mirrors composerRoleSchema with BOTH sides
// declaring the managed columns (soft-delete + timestamps) — the collision the
// A5 guard resolves in favor of the ROLE.
func composerRoleSchemaManaged() *core.TableSchema {
	base := core.NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").
		SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at")
	return core.NewTableSchema[*builderTestEntity]("aluno").
		PK("id").
		Field("Email", "email").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		SharedBase(base, "pessoa_id")
}

// The base's managed columns must NOT clobber the role's on the composed doc:
// the doc represents the ROLE — its lifecycle (an archived role stays visibly
// archived even while the base is active for a sibling role) and its own
// timestamps. Business fields keep merging flat.
func TestCompose_SharedBase_ManagedColumnsStayRoleScoped(t *testing.T) {
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return mapsFromColsData(
				[]string{"id", "name", "deleted_at", "created_at", "updated_at"},
				[][]any{{"p1", "Ana", nil, "2024-01-10", "2024-01-10"}}), nil
		case strings.Contains(sql, "FROM aluno"):
			return mapsFromColsData(
				[]string{"id", "email", "pessoa_id", "deleted_at", "created_at", "updated_at"},
				[][]any{{"a1", "a@x", "p1", "2026-07-01", "2026-06-30", "2026-07-01"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)
	view := View("aluno").Version(1).Root("aluno").Schema(composerRoleSchemaManaged())

	doc, err := c.Compose(context.Background(), view, "a1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc["name"] != "Ana" {
		t.Errorf("base business field must merge flat, got name=%v", doc["name"])
	}
	if doc["deleted_at"] != "2026-07-01" {
		t.Errorf("role's deleted_at must survive the base merge (role archived, base active), got %v", doc["deleted_at"])
	}
	if doc["created_at"] != "2026-06-30" || doc["updated_at"] != "2026-07-01" {
		t.Errorf("role's timestamps must survive the base merge, got created=%v updated=%v",
			doc["created_at"], doc["updated_at"])
	}
}

// StripArchivedChildren removes archived entries from derived child
// collections (default-read contract, one level down) and leaves everything
// else — active entries, root fields — untouched.
func TestViewNode_StripArchivedChildren(t *testing.T) {
	child := core.NewTableSchema[csComposeVO]("aluno_notas").
		PK("id").
		FK("aluno_id").
		Field("Label", "label").
		SoftDelete("deleted_at")
	root := core.NewTableSchema[*builderTestEntity]("aluno").
		PK("id").
		Field("Name", "name").
		SoftDelete("deleted_at").
		Child(child)
	node := View("aluno").Version(1).Root("aluno").Schema(root).BuildViewNode()

	seg := childDocSegment(child)
	doc := map[string]any{
		"id":   "a1",
		"name": "Ana",
		seg: []any{
			map[string]any{"id": "n1", "label": "active", "deleted_at": nil},
			map[string]any{"id": "n2", "label": "archived", "deleted_at": "2026-07-01"},
		},
	}
	node.StripArchivedChildren(doc)
	items, _ := doc[seg].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 active child after strip, got %d: %v", len(items), doc[seg])
	}
	first, _ := items[0].(map[string]any)
	if first["id"] != "n1" {
		t.Errorf("the ACTIVE entry must survive, got %v", items[0])
	}
	if doc["name"] != "Ana" {
		t.Errorf("root fields must stay untouched, got %v", doc["name"])
	}
}
