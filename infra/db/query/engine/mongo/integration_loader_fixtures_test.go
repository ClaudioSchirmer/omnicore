//go:build integration

package mongo

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/engine/postgres"
)

// Loader/aggregate fixtures for the infra-root view integration tests. They
// mirror the pg-engine package's own loader fixtures (test fixtures do not cross
// a package boundary), shaped so the Mongo-view composer/sync E2E can write a
// real aggregate through read.BaseAggregateRepository.

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

func (v loaderTagVO) GetID() string                                    { return v.ID }
func (v loaderTagVO) BuildRules(string, domain.Service, *domain.Rules) {}

type loaderNoteVO struct {
	ID   string
	Body string
}

func (v loaderNoteVO) GetID() string                                    { return v.ID }
func (v loaderNoteVO) BuildRules(string, domain.Service, *domain.Rules) {}

func createLoaderTables(t *testing.T, p *postgres.Postgres) {
	t.Helper()
	createTable(t, p, `CREATE TABLE loader_roots (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		email TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, p, `CREATE TABLE loader_tag_vos (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		loader_root_id UUID NOT NULL REFERENCES loader_roots(id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, p, `CREATE TABLE loader_note_vos (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		loader_root_id UUID NOT NULL REFERENCES loader_roots(id) ON DELETE CASCADE,
		body TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
}

func loaderTagSchema() *core.TableSchema {
	return core.NewTableSchema[loaderTagVO]("loader_tag_vos").
		PK("id").
		FK("loader_root_id").
		Field("Label", "label").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

// loaderRootSchema declares the loaderRoot aggregate with both children.
func loaderRootSchema() *core.TableSchema {
	return core.NewTableSchema[*loaderRoot]("loader_roots").
		PK("id").
		Field("Name", "name").
		Field("Email", "email").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Child(loaderTagSchema()).
		Child(core.NewTableSchema[loaderNoteVO]("loader_note_vos").
			PK("id").
			FK("loader_root_id").
			Field("Body", "body").
			SoftDelete("deleted_at").
			CreatedAt("created_at").
			UpdatedAt("updated_at"))
}
