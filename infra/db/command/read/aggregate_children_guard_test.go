package read

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// --- #5: aggregate boundary vs schema children ------------------------------

type guardChildVO struct{ ID string }

func (c guardChildVO) BuildRules(string, domain.Service, *domain.Rules) {}
func (c guardChildVO) GetID() domain.ID                                 { return domain.NewID(c.ID) }

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
	return NewTableSchema[guardChildVO]("guard_children").ID("id").ParentID("root_id")
}

func TestAggregateChildrenGuard_Match_OK(t *testing.T) {
	bar := NewBaseAggregateRepository[*guardAggRoot](nil, func() *guardAggRoot { return &guardAggRoot{} })
	schema := NewTableSchema[*guardAggRoot]("guards").ID("id").Revision("revision").SoftDelete("deleted_at").
		Child(guardChildSchema())
	bar.WithSchema(schema) // boundaries agree — must not panic
}

func TestAggregateChildrenGuard_DeclaredButNoChildSchema_Panics(t *testing.T) {
	bar := NewBaseAggregateRepository[*guardAggRoot](nil, func() *guardAggRoot { return &guardAggRoot{} })
	schema := NewTableSchema[*guardAggRoot]("guards").ID("id").Revision("revision").SoftDelete("deleted_at") // no Child
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
	schema := NewTableSchema[*barTestEntity]("bars").ID("id").Revision("revision").SoftDelete("deleted_at").
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
