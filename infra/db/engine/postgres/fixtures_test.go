//go:build postgres

package postgres

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Shared test fixtures for the PG engine tests. They mirror the relational-model
// (db) package's own fixtures; each package keeps its own copy since test
// fixtures do not cross a package boundary. The schemas are built through the
// public core.NewTableSchema surface — the engine consumes the neutral schema.

// aggLoaderTestEntity is a flat entity used by the AggregateLoader tests.
type aggLoaderTestEntity struct {
	domain.BaseEntity
}

func (e *aggLoaderTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *aggLoaderTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func newAggLoaderTestEntity() *aggLoaderTestEntity { return &aggLoaderTestEntity{} }

// covAgg is a self-contained aggregate fixture declaring all five modes.
type covAgg struct {
	domain.AggregateRoot
	Name string
}

func (e *covAgg) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *covAgg) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *covAgg) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *covAgg) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{covChild{}}
}

type covChild struct {
	ID    string
	Label string
}

func (c covChild) GetID() domain.ID                                 { return domain.NewID(c.ID) }
func (c covChild) BuildRules(string, domain.Service, *domain.Rules) {}

var covAggSchema = core.NewTableSchema[*covAgg]("cov_aggs").
	ID("id").
	Field("Name", "name").
	SoftDelete("deleted_at").
	Child(core.NewTableSchema[covChild]("cov_children").
		ID("id").
		ParentID("cov_agg_id").
		Field("Label", "label").
		SoftDelete("deleted_at"))

// builderTestEntity is the flat entity exercised by executor/Build*Event tests.
type builderTestEntity struct {
	domain.BaseEntity
	Name  string
	Email string
}

func (e *builderTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *builderTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

var builderTestSchema = core.NewTableSchema[*builderTestEntity]("builder_test_entities").
	ID("id").
	Field("Name", "name").
	Field("Email", "email").
	SoftDelete("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

func newBuilderCtx() persistence.RequestContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func newBuilderCtxWithIdentity(subject, issuer string, claims map[string]any) persistence.RequestContext {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{
		Subject: subject,
		Issuer:  issuer,
		Claims:  claims,
	})
	return ctx
}
