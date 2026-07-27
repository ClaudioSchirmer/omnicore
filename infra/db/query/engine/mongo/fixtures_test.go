package mongo

import (
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// Adapter-package copies of the flat test fixtures (the query package has its
// own; fixtures do not cross a package boundary). Minimal entity types anchor
// the schemas the Mongo read-side tests register.

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
	PK("id").
	Field("Name", "name").
	Field("Email", "email").
	SoftDelete("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

type embedFixture struct {
	ID    string
	Label string
	Other string
}

func rootSchema(table string) *core.TableSchema {
	return core.NewTableSchema[embedFixture](table).PK("id").SoftDelete("deleted_at")
}

// labeledSchema / otherSchema are the rootSchema variants for the composer
// tests whose fixture table carries a business column the composer must read
// back (the plain rootSchema declares none). Kept separate so tables without
// these columns keep using the minimal rootSchema.
func labeledSchema(table string) *core.TableSchema {
	return core.NewTableSchema[embedFixture](table).PK("id").Field("Label", "label").SoftDelete("deleted_at")
}

func otherSchema(table string) *core.TableSchema {
	return core.NewTableSchema[embedFixture](table).PK("id").Field("Other", "other").SoftDelete("deleted_at")
}

// derivedName bridges the adapter tests to the index-name derivation that now
// lives as a method on query.IndexSpec.
func derivedName(s *query.IndexSpec) string { return s.DerivedIndexName() }

