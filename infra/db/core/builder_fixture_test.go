package core

import "github.com/ClaudioSchirmer/omnicore/domain"

// builderTestEntity is the core package's own copy of the schema-construction
// test fixture (test fixtures do not cross a package boundary, so the db package
// keeps its own copy under the same name). A minimal Entity with two mapped
// fields, enough to drive NewTableSchema construction + the boot-time guards.
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
