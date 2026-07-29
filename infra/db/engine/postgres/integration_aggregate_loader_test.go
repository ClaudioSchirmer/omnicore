//go:build integration && postgres

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// loaderRoot is a root entity used by read.AggregateLoader integration tests.
// It declares two child types (loaderTagVO + loaderNoteVO) so the multi-child
// path is exercised.
type loaderRoot struct {
	domain.AggregateRoot
	Name  string
	Email string
}

func (e *loaderRoot) Modes() []domain.EntityMode                     { return []domain.EntityMode{domain.ModeInsert} }
func (*loaderRoot) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *loaderRoot) GetAggregateRoot() *domain.AggregateRoot        { return &e.AggregateRoot }
func (*loaderRoot) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{loaderTagVO{}, loaderNoteVO{}}
}

type loaderTagVO struct {
	ID    string
	Label string
}

func (v loaderTagVO) GetID() domain.ID                                 { return domain.NewID(v.ID) }
func (v loaderTagVO) BuildRules(string, domain.Service, *domain.Rules) {}

type loaderNoteVO struct {
	ID   string
	Body string
}

func (v loaderNoteVO) GetID() domain.ID                                 { return domain.NewID(v.ID) }
func (v loaderNoteVO) BuildRules(string, domain.Service, *domain.Rules) {}

func createLoaderTables(t *testing.T, pg *Postgres) {
	t.Helper()
	createTable(t, pg, `CREATE TABLE loader_roots (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		email TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE loader_tag_vos (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		loader_root_id UUID NOT NULL REFERENCES loader_roots(id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE loader_note_vos (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		loader_root_id UUID NOT NULL REFERENCES loader_roots(id) ON DELETE CASCADE,
		body TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
}

// loaderRootSchema declares the loaderRoot aggregate with both children
// (loaderTagVO + loaderNoteVO) — the explicit map the loader resolves from.
func loaderRootSchema() *core.TableSchema {
	return core.NewTableSchema[*loaderRoot]("loader_roots").
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Child(loaderTagSchema()).
		Child(core.NewTableSchema[loaderNoteVO]("loader_note_vos").
			ID("id").
			ParentID("loader_root_id").
			Field("Body", "body").
			SoftDelete("deleted_at").
			CreatedAt("created_at").
			UpdatedAt("updated_at"))
}

// loaderRootSchemaTagsOnly declares the loaderRoot aggregate with only the
// loaderTagVO child — for tests that exercise a single child type.
func loaderRootSchemaTagsOnly() *core.TableSchema {
	return core.NewTableSchema[*loaderRoot]("loader_roots").
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Child(loaderTagSchema())
}

// loaderRootSchemaFlat declares the loaderRoot aggregate with no children —
// the root-only path.
func loaderRootSchemaFlat() *core.TableSchema {
	return core.NewTableSchema[*loaderRoot]("loader_roots").
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

func loaderTagSchema() *core.TableSchema {
	return core.NewTableSchema[loaderTagVO]("loader_tag_vos").
		ID("id").
		ParentID("loader_root_id").
		Field("Label", "label").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

// --- Auto-scan (Load happy path) ----------------------------------------

func TestAggregateLoader_Load_AutoScanRootAndChildren(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	// Seed root + 2 tags + 1 note.
	var rootID string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('R', 'r@x') RETURNING id`).Scan(&rootID)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1, 'a'), ($1, 'b')`, rootID)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO loader_note_vos (loader_root_id, body) VALUES ($1, 'note-a')`, rootID)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchema())

	root, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(rootID)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if root.Name != "R" || root.Email != "r@x" {
		t.Errorf("root fields = %+v", root)
	}
	tags := domain.GetCurrentItemsOf[loaderTagVO](&root.AggregateRoot)
	if len(tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(tags))
	}
	notes := domain.GetCurrentItemsOf[loaderNoteVO](&root.AggregateRoot)
	if len(notes) != 1 {
		t.Errorf("expected 1 note, got %d", len(notes))
	}
}

func TestAggregateLoader_Load_NotFoundProducesNotFoundError(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaTagsOnly())
	_, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID("00000000-0000-0000-0000-000000000000")))
	if err == nil {
		t.Fatal("expected NotFound error")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected NotificationCarrier, got %T", err)
	}
	if domain.NotificationKey(carrier.NotificationContexts()[0].Messages()[0].Notification) != "RecordNotFoundNotification" {
		t.Errorf("expected RecordNotFoundNotification, got %v", carrier.NotificationContexts()[0].Messages()[0].Notification)
	}
}

func TestAggregateLoader_LoadIncludingArchived(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email, deleted_at) VALUES ('A', 'a@x', NOW()) RETURNING id`).Scan(&id)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1, 't')`, id)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaTagsOnly())

	// Load (active-only) fails.
	if _, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id))); err == nil {
		t.Error("expected Load to NOT find archived root")
	}
	// LoadIncludingArchived succeeds.
	root, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)).OnlyArchived())
	if err != nil {
		t.Fatalf("LoadIncludingArchived: %v", err)
	}
	if root.Name != "A" {
		t.Errorf("root = %+v", root)
	}
}

func TestAggregateLoader_LoadIncludingArchived_ActiveRootSurfacesAsNotFound(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('Alive', 'l@x') RETURNING id`).Scan(&id)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaFlat())
	if _, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)).OnlyArchived()); err == nil {
		t.Error("expected LoadIncludingArchived to fail on an ACTIVE root (literal 'find archived')")
	}
}

// --- Manual root scanner --------------------------------------------------

func TestAggregateLoader_Load_WithManualRootScanner(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('M', 'm@x') RETURNING id`).Scan(&id)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaFlat()).
		WithRootScanner(func(m map[string]any) (*loaderRoot, error) {
			r := &loaderRoot{}
			// The map is keyed by column name — read BY NAME, order-independent.
			// On the criteria path a manual scanner MUST populate the id (the
			// framework no longer injects it), so read it and SetID.
			idv, _ := m["id"].(string)
			name, _ := m["name"].(string)
			email, _ := m["email"].(string)
			r.SetID(domain.NewID(idv))
			r.Name = name + "_via_manual"
			r.Email = email
			return r, nil
		})

	root, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)))
	if err != nil {
		t.Fatalf("Load with manual scanner: %v", err)
	}
	if root.Name != "M_via_manual" {
		t.Errorf("Name = %q, want M_via_manual", root.Name)
	}
}

func TestAggregateLoader_Load_ManualRootScannerNotFound(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaFlat()).
		WithRootScanner(func(map[string]any) (*loaderRoot, error) {
			return &loaderRoot{}, nil // the by-id query returns no row, so this never runs
		})

	_, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID("00000000-0000-0000-0000-000000000000")))
	if err == nil {
		t.Fatal("expected NotFound")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected NotificationCarrier, got %T", err)
	}
}

// --- Manual child scanner -------------------------------------------------

func TestAggregateLoader_Load_WithManualChildScanner(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('X', 'x@x') RETURNING id`).Scan(&id)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1, 'manual')`, id)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaTagsOnly()).
		WithChildScanner("loaderTagVO", func(m map[string]any) (domain.AggregateValueObject, error) {
			idval, _ := m["id"].(string)
			label, _ := m["label"].(string)
			return loaderTagVO{ID: idval, Label: label + "_manual"}, nil
		})

	root, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)))
	if err != nil {
		t.Fatalf("Load with manual child: %v", err)
	}
	tags := domain.GetCurrentItemsOf[loaderTagVO](&root.AggregateRoot)
	if len(tags) != 1 || tags[0].Label != "manual_manual" {
		t.Errorf("manual child not applied: %+v", tags)
	}
}

// --- WithContextName + schema table/ParentID overrides --------------------------

func TestAggregateLoader_Schema_TableAndFKOverride(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()

	createTable(t, pg, `CREATE TABLE tb_loader_legacy (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		email TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE tb_tags (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		owner_id UUID NOT NULL REFERENCES tb_loader_legacy(id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO tb_loader_legacy (name, email) VALUES ('L', 'l@x') RETURNING id`).Scan(&id)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO tb_tags (owner_id, label) VALUES ($1, 'one')`, id)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithContextName("LegacyLoader").
		WithSchema(core.NewTableSchema[*loaderRoot]("tb_loader_legacy").
			ID("id").
			Field("Name", "name").
			Field("Email", "email").
			SoftDelete("deleted_at").
			CreatedAt("created_at").
			UpdatedAt("updated_at").
			Child(core.NewTableSchema[loaderTagVO]("tb_tags").
				ID("id").
				ParentID("owner_id").
				Field("Label", "label").
				SoftDelete("deleted_at").
				CreatedAt("created_at").
				UpdatedAt("updated_at")))

	root, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)))
	if err != nil {
		t.Fatalf("Load with overrides: %v", err)
	}
	if root.Name != "L" {
		t.Errorf("root.Name = %q, want L", root.Name)
	}
	tags := domain.GetCurrentItemsOf[loaderTagVO](&root.AggregateRoot)
	if len(tags) != 1 || tags[0].Label != "one" {
		t.Errorf("child not loaded via overrides: %+v", tags)
	}
}

// --- No-child path (root-only aggregate) -----------------------------------

func TestAggregateLoader_Load_NoChildRegistered(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('N', 'n@x') RETURNING id`).Scan(&id)

	// Schema declares NO children — root loads, children loop is skipped.
	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaFlat())
	root, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID(id)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if root.Name != "N" {
		t.Errorf("root.Name = %q", root.Name)
	}
}

// --- Auto-scan no-columns error --------------------------------------------

// emptyEntity has no domain fields (BaseEntity only) — auto-scan finds zero
// columns and the loader returns an actionable error message.
type emptyEntity struct {
	domain.BaseEntity
}

func (e *emptyEntity) Modes() []domain.EntityMode                     { return []domain.EntityMode{domain.ModeInsert} }
func (*emptyEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func TestAggregateLoader_Load_AutoScanWithNoFieldsErrors(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createTable(t, pg, `CREATE TABLE empty_entities (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	loader := read.NewAggregateLoader[*emptyEntity](pg, func() *emptyEntity { return &emptyEntity{} }).
		WithSchema(core.NewTableSchema[*emptyEntity]("empty_entities").ID("id").SoftDelete("deleted_at").CreatedAt("created_at"))
	_, err := loader.FindOne(context.Background(), criteria.ByID(domain.NewID("00000000-0000-0000-0000-000000000000")))
	if err == nil {
		t.Fatal("expected error from auto-scan with zero columns")
	}
}

// --- Criteria engine: FindOne / FindAll by arbitrary fields ----------------

func TestAggregateLoader_FindOne_ByNonIDField(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	var id string
	pg.Pool().QueryRow(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('Bob', 'bob@x') RETURNING id`).Scan(&id)
	pg.Pool().Exec(context.Background(),
		`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1, 'one'), ($1, 'two')`, id)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaTagsOnly())

	root, err := loader.FindOne(context.Background(), criteria.Where(criteria.Eq("Email", "bob@x")))
	if err != nil {
		t.Fatalf("FindOne by email: %v", err)
	}
	if root.GetID().Value() != id {
		t.Errorf("id = %q, want %q", root.GetID().Value(), id)
	}
	if root.Name != "Bob" {
		t.Errorf("name = %q", root.Name)
	}
	if tags := domain.GetCurrentItemsOf[loaderTagVO](&root.AggregateRoot); len(tags) != 2 {
		t.Errorf("expected 2 tags hydrated, got %d", len(tags))
	}
}

func TestAggregateLoader_FindOne_MultipleMatchesErrors(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	pg.Pool().Exec(context.Background(),
		`INSERT INTO loader_roots (name, email) VALUES ('Dup', 'a@x'), ('Dup', 'b@x')`)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaFlat())

	_, err := loader.FindOne(context.Background(), criteria.Where(criteria.Eq("Name", "Dup")))
	if err == nil {
		t.Fatal("expected FindOne to error on more than one match")
	}
}

func TestAggregateLoader_FindAll_OperatorsOrderLimitAndChildBatch(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	for _, name := range []string{"Ann", "Bea", "Cyd"} {
		var id string
		pg.Pool().QueryRow(context.Background(),
			`INSERT INTO loader_roots (name, email) VALUES ($1, $2) RETURNING id`,
			name, name+"@x").Scan(&id)
		pg.Pool().Exec(context.Background(),
			`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1, $2)`, id, name+"-tag")
	}

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaTagsOnly())

	// In + ORDER BY DESC + LIMIT; children must arrive grouped per root.
	roots, err := loader.FindAll(context.Background(),
		criteria.Where(criteria.In("Name", "Ann", "Bea", "Cyd")).OrderByDesc("Name").Limit(2))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots (limit), got %d", len(roots))
	}
	if roots[0].Name != "Cyd" || roots[1].Name != "Bea" {
		t.Errorf("order wrong: %q, %q", roots[0].Name, roots[1].Name)
	}
	for _, r := range roots {
		tags := domain.GetCurrentItemsOf[loaderTagVO](&r.AggregateRoot)
		if len(tags) != 1 || tags[0].Label != r.Name+"-tag" {
			t.Errorf("child batch grouping wrong for %q: %+v", r.Name, tags)
		}
	}
}

// TestAggregateLoader_FindAll_OffsetWindow proves offset pagination executes on
// a live Postgres: an ordered FindAll with Limit + Offset returns the correct
// page via the native `LIMIT n OFFSET m`. The window is identical across
// engines; only the rendered clause differs.
func TestAggregateLoader_FindAll_OffsetWindow(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	for _, name := range []string{"Ann", "Bea", "Cyd", "Dan", "Eve"} {
		if _, err := pg.Pool().Exec(context.Background(),
			`INSERT INTO loader_roots (name, email) VALUES ($1, $2)`, name, name+"@x"); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaTagsOnly())

	// Page 2 of an ascending window, 2 per page — skip Ann, Bea; expect Cyd, Dan.
	page, err := loader.FindAll(context.Background(),
		criteria.Where(nil).OrderBy("Name").Limit(2).Offset(2))
	if err != nil {
		t.Fatalf("FindAll offset window: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("offset window: expected 2 rows, got %d", len(page))
	}
	if page[0].Name != "Cyd" || page[1].Name != "Dan" {
		t.Fatalf("offset window order wrong: %q, %q (want Cyd, Dan)", page[0].Name, page[1].Name)
	}

	// The last page is shorter than the limit — skip 4, expect just Eve.
	tail, err := loader.FindAll(context.Background(),
		criteria.Where(nil).OrderBy("Name").Limit(2).Offset(4))
	if err != nil {
		t.Fatalf("FindAll tail window: %v", err)
	}
	if len(tail) != 1 || tail[0].Name != "Eve" {
		t.Fatalf("tail window wrong: got %d rows (want 1 row Eve)", len(tail))
	}

	// Contract: an offset with no ORDER BY is rejected before it can return a
	// non-deterministic page.
	if _, err := loader.FindAll(context.Background(),
		criteria.Where(nil).Limit(2).Offset(2)); err == nil {
		t.Fatal("expected an error: Offset without an OrderBy")
	}
}

func TestAggregateLoader_FindAll_EmptyResultIsNotError(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).
		WithSchema(loaderRootSchemaFlat())
	roots, err := loader.FindAll(context.Background(), criteria.Where(criteria.Eq("Name", "none")))
	if err != nil {
		t.Fatalf("FindAll empty: %v", err)
	}
	if len(roots) != 0 {
		t.Errorf("expected 0 roots, got %d", len(roots))
	}
}
