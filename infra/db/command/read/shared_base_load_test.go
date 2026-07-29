package read

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// White-box coverage for the SharedBase (M2) read side in the loader:
// hydrateSharedBase loads the shared columns into the role entity, and a criteria
// on a shared-base field LEFT JOINs the base table.

type roleLoadEntity struct {
	domain.BaseEntity
	Name      string // shared (lives on the base)
	Matricula string // role-own
}

func (e *roleLoadEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate}
}
func (e *roleLoadEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func roleLoadSchema() *TableSchema {
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name")
	return NewTableSchema[*roleLoadEntity]("aluno").
		ID("id").
		Field("Matricula", "matricula").
		SharedBase(base, "pessoa_id")
}

func TestHydrateSharedBase_PopulatesRoleFromBase(t *testing.T) {
	query := func(sql string, args []any) (Rows, error) {
		if strings.Contains(sql, "JOIN") && strings.Contains(sql, "pessoa") {
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				if p, ok := dest[0].(*string); ok { // leading key (role pk), discarded
					*p = "a1"
				}
				if p, ok := dest[1].(*string); ok { // base column "name" → role Name
					*p = "Ana"
				}
				return nil
			}}, nil
		}
		return &fakeDBRows{}, nil
	}
	l := NewAggregateLoader[*roleLoadEntity](fakeEngine(query), func() *roleLoadEntity { return &roleLoadEntity{} }).
		WithSchema(roleLoadSchema())

	e := &roleLoadEntity{Matricula: "M1"}
	e.SetID(domain.NewID("a1"))
	if err := l.hydrateSharedBase(context.Background(), []*roleLoadEntity{e}, []string{"a1"}); err != nil {
		t.Fatalf("hydrateSharedBase: %v", err)
	}
	if e.Name != "Ana" {
		t.Errorf("shared-base field not hydrated: Name=%q, want \"Ana\"", e.Name)
	}
}

// A criteria on a SHARED-BASE field LEFT JOINs the base table (role ParentID → base ID).
func TestFindRoots_SharedBaseFilterJoins(t *testing.T) {
	var rootSQL string
	query := func(sql string, args []any) (Rows, error) {
		if strings.Contains(sql, "FROM ") && strings.Contains(sql, "aluno") {
			rootSQL = sql
		}
		return &fakeDBRows{rows: 0}, nil
	}
	l := NewAggregateLoader[*roleLoadEntity](fakeEngine(query), func() *roleLoadEntity { return &roleLoadEntity{} }).
		WithSchema(roleLoadSchema())

	if _, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("Name", "Ana"))); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if !strings.Contains(rootSQL, "LEFT JOIN") || !strings.Contains(rootSQL, "pessoa") {
		t.Errorf("a shared-base filter must LEFT JOIN the base table, got: %q", rootSQL)
	}
}
