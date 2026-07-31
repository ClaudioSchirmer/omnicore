//go:build integration && postgres

package mongo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/engine/postgres"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/relational"
)

// TestRelationalDocParity_RootChildrenManaged is the guard that the relational
// read path's document (engine/relational.BuildDocument, mapped from the loaded
// typed aggregate) is byte-for-byte the SAME document the Composer builds by
// fetching relationally — the two ways a view can be read must return the same
// shape. It compares the two column-keyed documents as canonical JSON (Go's
// json.Marshal sorts map keys), so ANY divergence — a managed/magic column one
// path emits and the other misses, a different value form, a nesting difference
// — fails here and names the gap. That is what removes the "remember to change
// two places" burden: add a managed column to the schema and the Composer picks
// it up automatically; if BuildDocument does not, this test goes red.
//
// Covers: root scalars, the physical id column, the managed timestamps +
// revision watermark (_revision), and two own-child collections under their
// pluralized segments with their own id + managed columns. Sibling flatten and
// the archived-child strip are follow-up cases over a richer fixture.
func TestRelationalDocParity_RootChildrenManaged(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	ctx := context.Background()
	var rootID string
	if err := pg.Pool().QueryRow(ctx,
		`INSERT INTO loader_roots (name, email) VALUES ('Ada', 'ada@x.com') RETURNING id`).Scan(&rootID); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1,'red'), ($1,'blue')`, rootID); err != nil {
		t.Fatalf("seed tags: %v", err)
	}
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO loader_note_vos (loader_root_id, body) VALUES ($1,'hello')`, rootID); err != nil {
		t.Fatalf("seed note: %v", err)
	}

	schema := loaderRootSchema()
	view := query.View("loader_roots").Schema(schema).Version(1)

	// Path A — the Composer: relational fetch -> column-keyed document.
	composerDoc, err := query.NewComposer(pg).Compose(ctx, view, rootID)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if composerDoc == nil {
		t.Fatal("compose returned nil doc for a live root")
	}

	// Path B — the loader (the SAME typed loader a repo builds) -> BuildDocument.
	// IncludeArchived matches the Composer's keep-archived default so the two
	// paths load the identical set of children.
	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).WithSchema(schema)
	ent, err := loader.FindOne(ctx, criteria.ByID(domain.NewID(rootID)).IncludeArchived())
	if err != nil {
		t.Fatalf("findone: %v", err)
	}
	relationalDoc := relational.BuildDocument(schema, ent)

	assertDocParity(t, composerDoc, relationalDoc)
}

// assertDocParity fails unless the two documents marshal to identical canonical
// JSON. json.Marshal sorts map keys and normalizes value types (time.Time ->
// RFC3339, integers -> numbers), so the compare is order- and Go-type-agnostic
// and reports the two full documents on divergence.
func assertDocParity(t *testing.T, composerDoc, relationalDoc query.Document) {
	t.Helper()
	ja, err := json.Marshal(composerDoc)
	if err != nil {
		t.Fatalf("marshal composer doc: %v", err)
	}
	jr, err := json.Marshal(relationalDoc)
	if err != nil {
		t.Fatalf("marshal relational doc: %v", err)
	}
	if string(ja) == string(jr) {
		return
	}
	pa, _ := json.MarshalIndent(composerDoc, "", "  ")
	pr, _ := json.MarshalIndent(relationalDoc, "", "  ")
	t.Fatalf("relational document diverges from composer document:\n--- composer ---\n%s\n--- relational ---\n%s", pa, pr)
}

// --- sibling flatten parity ------------------------------------------------

// parityRoot exercises a ROOT SIBLING (a secondary table sharing the root PK,
// merged flat into the doc) alongside an own child — the two shape rules the
// loaderRoot fixture does not carry.
type parityRoot struct {
	domain.AggregateRoot
	Name     string
	Nickname string // sibling field: parity_roots_ext.nickname
}

func (e *parityRoot) Modes() []domain.EntityMode                     { return []domain.EntityMode{domain.ModeInsert} }
func (*parityRoot) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *parityRoot) GetAggregateRoot() *domain.AggregateRoot        { return &e.AggregateRoot }
func (*parityRoot) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{parityLineVO{}}
}

type parityLineVO struct {
	domain.Managed
	Amount int64
}

func (v parityLineVO) BuildRules(string, domain.Service, *domain.Rules) {}

func (v parityLineVO) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(v, o)
}

func parityRootSchema() *core.TableSchema {
	return core.NewTableSchema[*parityRoot]("parity_roots").
		ID("id").
		Revision("revision").
		Field("Name", "name").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Sibling(core.NewSiblingSchema[*parityRoot]("parity_roots_ext").Field("Nickname", "nickname")).
		Child(core.NewTableSchema[parityLineVO]("parity_lines").
			ID("id").
			ParentID("parity_root_id").
			Field("Amount", "amount").
			DeletedAt("deleted_at").
			CreatedAt("created_at").
			UpdatedAt("updated_at"))
}

func createParityTables(t *testing.T, p *postgres.Postgres) {
	t.Helper()
	createTable(t, p, `CREATE TABLE parity_roots (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		revision BIGINT NOT NULL DEFAULT 0,
		name TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, p, `CREATE TABLE parity_roots_ext (
		id UUID PRIMARY KEY REFERENCES parity_roots(id) ON DELETE CASCADE,
		nickname TEXT NOT NULL
	)`)
	createTable(t, p, `CREATE TABLE parity_lines (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		parity_root_id UUID NOT NULL REFERENCES parity_roots(id) ON DELETE CASCADE,
		amount BIGINT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
}

// TestRelationalDocParity_WithSibling adds a root sibling (merged flat) to the
// root + own child shape, closing the mergeSiblings gap.
func TestRelationalDocParity_WithSibling(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createParityTables(t, pg)

	ctx := context.Background()
	var rootID string
	if err := pg.Pool().QueryRow(ctx,
		`INSERT INTO parity_roots (name) VALUES ('Grace') RETURNING id`).Scan(&rootID); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO parity_roots_ext (id, nickname) VALUES ($1, 'Amazing')`, rootID); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO parity_lines (parity_root_id, amount) VALUES ($1, 100), ($1, 250)`, rootID); err != nil {
		t.Fatalf("seed lines: %v", err)
	}

	schema := parityRootSchema()
	view := query.View("parity_roots").Schema(schema).Version(1)

	composerDoc, err := query.NewComposer(pg).Compose(ctx, view, rootID)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	loader := read.NewAggregateLoader[*parityRoot](pg, func() *parityRoot { return &parityRoot{} }).WithSchema(schema)
	ent, err := loader.FindOne(ctx, criteria.ByID(domain.NewID(rootID)).IncludeArchived())
	if err != nil {
		t.Fatalf("findone: %v", err)
	}
	assertDocParity(t, composerDoc, relational.BuildDocument(schema, ent))
}

// TestRelationalDocParity_ArchivedChild seeds one live and one archived child and
// loads with IncludeArchived (matching the Composer's keep-archived default), so
// both paths carry the archived element WITH its deleted_at — the read-time strip
// is a later ViewNode pass, not part of the composed/built document.
func TestRelationalDocParity_ArchivedChild(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	ctx := context.Background()
	var rootID string
	if err := pg.Pool().QueryRow(ctx,
		`INSERT INTO loader_roots (name, email) VALUES ('Lin', 'lin@x.com') RETURNING id`).Scan(&rootID); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1,'live')`, rootID); err != nil {
		t.Fatalf("seed live tag: %v", err)
	}
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO loader_tag_vos (loader_root_id, label, deleted_at) VALUES ($1,'gone', NOW())`, rootID); err != nil {
		t.Fatalf("seed archived tag: %v", err)
	}

	schema := loaderRootSchema()
	view := query.View("loader_roots").Schema(schema).Version(1)

	composerDoc, err := query.NewComposer(pg).Compose(ctx, view, rootID)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).WithSchema(schema)
	ent, err := loader.FindOne(ctx, criteria.ByID(domain.NewID(rootID)).IncludeArchived())
	if err != nil {
		t.Fatalf("findone: %v", err)
	}
	assertDocParity(t, composerDoc, relational.BuildDocument(schema, ent))
}
