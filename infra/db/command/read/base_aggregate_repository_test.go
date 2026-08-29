package read

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
	// The embedded BaseRepository's own context-name derivation is covered by the
	// write package's base_repository_context_name_test; here we assert the read
	// loader (db) shares the same T-derived name.
	if got := bar.Loader.contextName(); got != "barTestEntity" {
		t.Errorf("Loader derived from T = %q, got %q", "barTestEntity", got)
	}
}

func TestBaseAggregateRepository_WithSchemaThreadsBothSides(t *testing.T) {
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)
	schema := NewTableSchema[*barTestEntity]("bars").ID("id").Revision("revision").DeletedAt("deleted_at")
	bar.WithSchema(schema)
	// The write-side binding (BaseRepository.schema is unexported and set only
	// via WithSchema) is asserted in the write package's
	// TestBaseRepositoryWithSchema_Valid_SetsSchema; the aggregate WithSchema
	// reaches it through r.BaseRepository.WithSchema(schema). Here we assert the
	// half unique to the aggregate path: the SAME call threads the Loader.
	if bar.Loader.schema != schema {
		t.Error("WithSchema must thread the schema into the Loader")
	}
}

// ptrChildVO is an AggregateValueObject implemented on a pointer receiver and
// returned as a pointer from AggregateChildren — so validateDeclaredChildren
// walks the reflect.Ptr deref loop before matching the schema's Child(...).
type ptrChildVO struct {
	domain.Managed
	Label string
}

func (c *ptrChildVO) BuildRules(string, domain.Service, *domain.Rules) {}

type ptrAggEntity struct {
	domain.AggregateRoot
	Name string
}

func (e *ptrAggEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *ptrAggEntity) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *ptrAggEntity) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *ptrAggEntity) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{&ptrChildVO{}}
}

func TestValidateDeclaredChildren_PointerVO(t *testing.T) {
	bar := NewBaseAggregateRepository[*ptrAggEntity](nil, func() *ptrAggEntity { return &ptrAggEntity{} })
	schema := NewTableSchema[*ptrAggEntity]("ptr_aggs").ID("id").Revision("revision").Field("Name", "name").DeletedAt("deleted_at").
		Child(NewTableSchema[*ptrChildVO]("ptr_children").
			ID("id").ParentID("ptr_agg_id").Field("Label", "label").DeletedAt("deleted_at"))
	// Boundaries agree (pointer VO derefs to ptrChildVO == schema child key) →
	// no panic; the deref loop is the branch under test.
	bar.WithSchema(schema)
	if schema.ChildSchema("ptrChildVO") == nil {
		t.Error("pointer child VO must register under its dereferenced type name")
	}
}

func TestValidateDeclaredChildren_BoundaryMismatchPanics(t *testing.T) {
	bar := NewBaseAggregateRepository[*ptrAggEntity](nil, func() *ptrAggEntity { return &ptrAggEntity{} })
	// Schema declares no Child while AggregateChildren() names ptrChildVO →
	// boundary mismatch panic.
	schema := NewTableSchema[*ptrAggEntity]("ptr_aggs").ID("id").Revision("revision").Field("Name", "name").DeletedAt("deleted_at")
	defer func() {
		if recover() == nil {
			t.Fatal("expected aggregate boundary mismatch panic")
		}
	}()
	bar.WithSchema(schema)
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
