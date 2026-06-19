package infra

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

type barTestEntity struct {
	domain.BaseEntity
	Name string
}

func (e *barTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *barTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func newBARTestEntity() *barTestEntity { return &barTestEntity{} }

func TestBaseAggregateRepository_ConstructorWiresBothSides(t *testing.T) {
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)

	if bar.NewEntity == nil {
		t.Error("BaseRepository.NewEntity must be populated by the factory")
	}
	if bar.Loader == nil {
		t.Fatal("Loader must be initialized")
	}
	if bar.Loader.newEntity == nil {
		t.Error("Loader must receive the same single-source factory")
	}
}

func TestBaseAggregateRepository_NewPromotedFromBaseRepository(t *testing.T) {
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)
	got := bar.New()
	if got == nil {
		t.Fatal("New() must return entity via injected factory")
	}
}

func TestBaseAggregateRepository_ContextNameSharedDerivation(t *testing.T) {
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)
	if got := bar.effectiveContextName(); got != "barTestEntity" {
		t.Errorf("BaseRepository derived from T = %q, got %q", "barTestEntity", got)
	}
	if got := bar.Loader.effectiveContextName(); got != "barTestEntity" {
		t.Errorf("Loader derived from T = %q, got %q", "barTestEntity", got)
	}
}

func TestBaseAggregateRepository_WithSchemaThreadsBothSides(t *testing.T) {
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)
	schema := NewTableSchema[*barTestEntity]("bars").PK("ID", "id").SoftDelete("deleted_at")
	bar.WithSchema(schema)
	if bar.Schema != schema {
		t.Error("WithSchema must set the write-side Schema")
	}
	if bar.Loader.schema != schema {
		t.Error("WithSchema must thread the schema into the Loader")
	}
}

// Compile-time check: a struct that only embeds BaseAggregateRepository
// satisfies persistence.ScopedRepository[T] (domain.Reader[T] + Scope) and
// domain.ArchivedFinder[T] — FindByID, New() and Scope are all promoted via
// embed, FindArchivedByID is provided by BaseAggregateRepository itself.
type barUserRepo struct {
	BaseAggregateRepository[*barTestEntity]
}

var (
	_ persistence.ScopedRepository[*barTestEntity] = (*barUserRepo)(nil)
	_ domain.ArchivedFinder[*barTestEntity]        = (*barUserRepo)(nil)
)
