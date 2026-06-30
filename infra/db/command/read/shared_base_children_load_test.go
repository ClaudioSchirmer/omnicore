package read

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// White-box coverage for the SharedBase native-children read side: the loader
// hydrates the base's children into the role aggregate as Constructor items,
// joining the base-child to the role on the shared base id — what lets an UPDATE
// diff the person-native collection instead of re-inserting (clobbering) it.

type addrLoad struct {
	ID     string
	Street string
}

func (a addrLoad) GetID() string                                    { return a.ID }
func (a addrLoad) BuildRules(string, domain.Service, *domain.Rules) {}

type roleAggLoad struct {
	domain.AggregateRoot
	Name      string // base field
	Matricula string // role-own
}

func (e *roleAggLoad) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate}
}
func (e *roleAggLoad) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *roleAggLoad) GetAggregateRoot() *domain.AggregateRoot { return &e.AggregateRoot }
func (e *roleAggLoad) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{addrLoad{}}
}

func roleAggLoadSchema() *TableSchema {
	base := NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").
		Child(NewTableSchema[addrLoad]("endereco").PK("id").FK("pessoa_id").Field("Street", "street"))
	return NewTableSchema[*roleAggLoad]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SharedBase(base, "pessoa_id")
}

func TestHydrateBaseChildren_LoadsAsConstructor(t *testing.T) {
	var joinSQL string
	query := func(sql string, args []any) (Rows, error) {
		if strings.Contains(sql, "endereco") && strings.Contains(sql, "JOIN") {
			joinSQL = sql
			return &fakeDBRows{rows: 2, scan: func(i int, dest []any) error {
				for j, d := range dest {
					p, ok := d.(*string)
					if !ok {
						continue
					}
					if j == 0 {
						*p = "a1" // leading key = role pk (groups onto the aggregate)
					} else {
						*p = fmt.Sprintf("addr-%d-%d", i, j)
					}
				}
				return nil
			}}, nil
		}
		return &fakeDBRows{}, nil
	}
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(roleAggLoadSchema())

	e := &roleAggLoad{Matricula: "M1"}
	e.SetID(domain.NewID("a1"))
	if err := l.hydrateBaseChildren(context.Background(), []*roleAggLoad{e}, []string{"a1"}, criteria.ScopeActive); err != nil {
		t.Fatalf("hydrateBaseChildren: %v", err)
	}

	// The base-child JOIN must link the base-child to the role on the shared FK.
	if !strings.Contains(joinSQL, "FROM endereco JOIN aluno") || !strings.Contains(joinSQL, "endereco.pessoa_id = aluno.pessoa_id") {
		t.Errorf("base-children must JOIN base-child → role on the shared base id; got %q", joinSQL)
	}
	items := domain.GetCurrentItemsOf[addrLoad](&e.AggregateRoot)
	if len(items) != 2 {
		t.Fatalf("expected 2 base-children hydrated as Constructor, got %d", len(items))
	}
	for _, it := range items {
		if it.GetID() == "" {
			t.Error("a hydrated base-child must carry its own PK (for the UPDATE diff)")
		}
	}
}

// A role without a shared base, or a base without children, hydrates nothing.
func TestHydrateBaseChildren_NoopWithoutBaseChildren(t *testing.T) {
	query := func(string, []any) (Rows, error) { return &fakeDBRows{}, nil }
	l := NewAggregateLoader[*roleLoadEntity](fakeEngine(query), func() *roleLoadEntity { return &roleLoadEntity{} }).
		WithSchema(roleLoadSchema()) // base has no children
	e := &roleLoadEntity{Matricula: "M1"}
	e.SetID(domain.NewID("a1"))
	if err := l.hydrateBaseChildren(context.Background(), []*roleLoadEntity{e}, []string{"a1"}, criteria.ScopeActive); err != nil {
		t.Fatalf("hydrateBaseChildren: %v", err)
	}
}
