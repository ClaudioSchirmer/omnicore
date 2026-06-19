//go:build integration

package infra

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// schemaPerson exercises the TableSchema end-to-end: a renamed PK
// (person_pk), renamed domain columns (full_name, mail), a renamed soft-delete
// column (removed_at), and managed created_at/updated_at columns the framework
// must stamp (the table declares them NOT NULL with NO default, so a missing
// stamp would fail the INSERT). The aggregate child diverges on its own PK,
// FK, column, and soft-delete name.
type schemaPerson struct {
	domain.AggregateRoot
	FullName string
	Email    string
}

func (e *schemaPerson) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeArchive, domain.ModeUnarchive}
}
func (*schemaPerson) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *schemaPerson) GetAggregateRoot() *domain.AggregateRoot { return &e.AggregateRoot }
func (*schemaPerson) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{schemaTag{}}
}

type schemaTag struct {
	ID    string
	Label string
}

func (v schemaTag) GetID() string                                    { return v.ID }
func (v schemaTag) BuildRules(string, domain.Service, *domain.Rules) {}

func schemaPersonSchema() *TableSchema {
	return NewTableSchema[*schemaPerson]("tb_people").
		PK("ID", "person_pk").
		Field("FullName", "full_name").
		Field("Email", "mail").
		SoftDelete("removed_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Child(
			NewTableSchema[schemaTag]("tb_tags").
				PK("ID", "tag_pk").
				FK("person_ref").
				Field("Label", "caption").
				SoftDelete("removed_at").
				CreatedAt("created_at").
				UpdatedAt("updated_at"),
		)
}

func createSchemaMapTables(t *testing.T, pg *Postgres) {
	t.Helper()
	// created_at / updated_at carry NO default — the framework MUST stamp them.
	createTable(t, pg, `CREATE TABLE tb_people (
		person_pk  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		full_name  TEXT NOT NULL,
		mail       TEXT NOT NULL,
		removed_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`)
	createTable(t, pg, `CREATE TABLE tb_tags (
		tag_pk     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		person_ref UUID NOT NULL REFERENCES tb_people(person_pk) ON DELETE CASCADE,
		caption    TEXT NOT NULL,
		removed_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`)
}

func newSchemaPersonRepo(pg *Postgres) *BaseAggregateRepository[*schemaPerson] {
	r := NewBaseAggregateRepository[*schemaPerson](pg, func() *schemaPerson { return &schemaPerson{} })
	r.WithSchema(schemaPersonSchema())
	return &r
}

// TestSchemaMap_WriteCriteriaReadRoundTrip locks the latent-bug fix: a value
// written to a renamed column must be filterable by criteria AND scannable back
// through the loader. Before the fix the read path used convention columns and
// the row written to `mail` could not be read back.
func TestSchemaMap_WriteCriteriaReadRoundTrip(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createSchemaMapTables(t, pg)
	repo := newSchemaPersonRepo(pg)
	ctx := testCtx()

	person := &schemaPerson{FullName: "Ada Lovelace", Email: "ada@x.test"}
	domain.AddAggregateChild(person, schemaTag{Label: "pioneer"})
	ins, err := domain.GetInsertable(person, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	id, err := repo.Scope(ctx).Insert(ins)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The framework stamped created_at / updated_at (no DB default existed) —
	// the INSERT above succeeding already proves it; assert non-null anyway.
	var createdNotNull, updatedNotNull bool
	if err := pg.Pool().QueryRow(context.Background(),
		`SELECT created_at IS NOT NULL, updated_at IS NOT NULL FROM tb_people WHERE person_pk = $1`, id.Value()).
		Scan(&createdNotNull, &updatedNotNull); err != nil {
		t.Fatalf("stamp probe: %v", err)
	}
	if !createdNotNull || !updatedNotNull {
		t.Errorf("framework must stamp created_at/updated_at (created=%v updated=%v)", createdNotNull, updatedNotNull)
	}

	// Criteria on the renamed column + scan-back through the renamed columns.
	got, err := repo.Loader.FindOne(ctx, criteria.Where(criteria.Eq("Email", "ada@x.test")))
	if err != nil {
		t.Fatalf("FindOne by renamed Email column: %v", err)
	}
	if got.FullName != "Ada Lovelace" || got.Email != "ada@x.test" {
		t.Errorf("scan-back wrong: %+v", got)
	}
	tags := domain.GetCurrentItemsOf[schemaTag](&got.AggregateRoot)
	if len(tags) != 1 || tags[0].Label != "pioneer" {
		t.Errorf("child round-trip wrong: %+v", tags)
	}
}

// TestSchemaMap_ArchiveUsesRenamedSoftDeleteColumn proves the soft-delete
// membrane: archive sets removed_at (root + child cascade), the active scope
// hides the row, and the archived scope recovers it.
func TestSchemaMap_ArchiveUsesRenamedSoftDeleteColumn(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createSchemaMapTables(t, pg)
	repo := newSchemaPersonRepo(pg)
	ctx := testCtx()

	person := &schemaPerson{FullName: "Grace Hopper", Email: "grace@x.test"}
	domain.AddAggregateChild(person, schemaTag{Label: "navy"})
	ins, _ := domain.GetInsertable(person, nil, "GetInsertable")
	id, err := repo.Scope(ctx).Insert(ins)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	loaded, err := repo.FindByID(id)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	arch, err := domain.GetArchivable(loaded, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if err := repo.Scope(ctx).Archive(arch); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Root + child removed_at populated.
	var rootArchived, childArchived bool
	if err := pg.Pool().QueryRow(context.Background(),
		`SELECT removed_at IS NOT NULL FROM tb_people WHERE person_pk = $1`, id.Value()).Scan(&rootArchived); err != nil {
		t.Fatalf("root archive probe: %v", err)
	}
	if err := pg.Pool().QueryRow(context.Background(),
		`SELECT removed_at IS NOT NULL FROM tb_tags WHERE person_ref = $1`, id.Value()).Scan(&childArchived); err != nil {
		t.Fatalf("child archive probe: %v", err)
	}
	if !rootArchived || !childArchived {
		t.Errorf("archive must set removed_at on root + child (root=%v child=%v)", rootArchived, childArchived)
	}

	// Active scope hides it; archived scope recovers it.
	if _, err := repo.FindByID(id); err == nil {
		t.Errorf("active scope must not return the archived person")
	}
	if _, err := repo.FindArchivedByID(id); err != nil {
		t.Errorf("archived scope must recover the person: %v", err)
	}
}
