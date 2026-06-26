package infra

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// --- #6: BaseRepository.WithSchema validates the flat path -------------------

// flatArchivable is a non-aggregate entity declaring Archive in Modes(). The
// validated WithSchema must reject a schema with no SoftDelete column at
// construction.
type flatArchivable struct {
	domain.BaseEntity
	Name string
}

func (e *flatArchivable) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *flatArchivable) BuildRules(string, domain.Service, *domain.Rules) {}

func TestBaseRepositoryWithSchema_ModesVsSoftDelete_Panics(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{NewEntity: func() *flatArchivable { return &flatArchivable{} }}
	schema := NewTableSchema[*flatArchivable]("flats").PK("id") // no SoftDelete
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic: Archive in Modes() without SoftDelete on the flat path")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "SoftDelete") {
			t.Errorf("panic must mention SoftDelete, got %q", msg)
		}
	}()
	repo.WithSchema(schema)
}

func TestBaseRepositoryWithSchema_Valid_SetsSchema(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{NewEntity: func() *flatArchivable { return &flatArchivable{} }}
	schema := NewTableSchema[*flatArchivable]("flats").PK("id").SoftDelete("deleted_at")
	repo.WithSchema(schema)
	if repo.Schema != schema {
		t.Error("WithSchema must bind the schema on the happy path")
	}
}

func TestBaseRepositoryWithSchema_NilFactory_Panics(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{} // NewEntity nil
	schema := NewTableSchema[*flatArchivable]("flats").PK("id").SoftDelete("deleted_at")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic: nil NewEntity surfaced at WithSchema construction")
		}
	}()
	repo.WithSchema(schema)
}

// --- #5: aggregate boundary vs schema children ------------------------------

type guardChildVO struct{ ID string }

func (c guardChildVO) BuildRules(string, domain.Service, *domain.Rules) {}
func (c guardChildVO) GetID() string                                    { return c.ID }

type guardAggRoot struct {
	domain.AggregateRoot
	Name string
}

func (a *guardAggRoot) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeArchive, domain.ModeUnarchive}
}
func (a *guardAggRoot) BuildRules(string, domain.Service, *domain.Rules) {}
func (a *guardAggRoot) GetAggregateRoot() *domain.AggregateRoot          { return &a.AggregateRoot }
func (a *guardAggRoot) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{guardChildVO{}}
}

func guardChildSchema() *TableSchema {
	return NewTableSchema[guardChildVO]("guard_children").PK("id").FK("root_id")
}

func TestAggregateChildrenGuard_Match_OK(t *testing.T) {
	bar := NewBaseAggregateRepository[*guardAggRoot](nil, func() *guardAggRoot { return &guardAggRoot{} })
	schema := NewTableSchema[*guardAggRoot]("guards").PK("id").SoftDelete("deleted_at").
		Child(guardChildSchema())
	bar.WithSchema(schema) // boundaries agree — must not panic
}

func TestAggregateChildrenGuard_DeclaredButNoChildSchema_Panics(t *testing.T) {
	bar := NewBaseAggregateRepository[*guardAggRoot](nil, func() *guardAggRoot { return &guardAggRoot{} })
	schema := NewTableSchema[*guardAggRoot]("guards").PK("id").SoftDelete("deleted_at") // no Child
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic: AggregateChildren() declares guardChildVO but schema has no Child(...)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "guardChildVO") || !strings.Contains(msg, "AggregateChildren") {
			t.Errorf("panic must name the offending type and side, got %q", msg)
		}
	}()
	bar.WithSchema(schema)
}

func TestAggregateChildrenGuard_ChildSchemaButNotDeclared_Panics(t *testing.T) {
	// barTestEntity (defined in base_aggregate_repository_test.go) is NOT an
	// AggregateRootProvider, so its declared boundary is empty — a schema with
	// a Child(...) must be flagged.
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)
	schema := NewTableSchema[*barTestEntity]("bars").PK("id").SoftDelete("deleted_at").
		Child(guardChildSchema())
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic: schema declares Child(...) absent from AggregateChildren()")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "guardChildVO") {
			t.Errorf("panic must name the offending type, got %q", msg)
		}
	}()
	bar.WithSchema(schema)
}
