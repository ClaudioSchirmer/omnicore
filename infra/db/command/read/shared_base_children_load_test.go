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

func (a addrLoad) GetID() domain.ID                                 { return domain.NewID(a.ID) }
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
func (e *roleAggLoad) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *roleAggLoad) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{addrLoad{}}
}

func roleAggLoadSchema() *TableSchema {
	base := NewSharedBase("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("name").
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
		if it.GetID().Value() == "" {
			t.Error("a hydrated base-child must carry its own PK (for the UPDATE diff)")
		}
	}
}

// §4.5 load-first: LoadSharedBaseIdentity hydrates the existing identity (base
// fields + base-children as Constructor) by natural key, or returns fresh when cold.
func TestLoadSharedBaseIdentity_WarmHydrates(t *testing.T) {
	query := func(sql string, args []any) (Rows, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				if p, ok := dest[0].(*string); ok { // base pk (leading key)
					*p = "p1"
				}
				if p, ok := dest[1].(*string); ok { // base column "name" → Name
					*p = "Ana"
				}
				return nil
			}}, nil
		case strings.Contains(sql, "FROM endereco"):
			return &fakeDBRows{rows: 2, scan: func(i int, dest []any) error {
				for j, d := range dest {
					if p, ok := d.(*string); ok {
						if j == 0 {
							*p = "p1" // leading fk
						} else {
							*p = fmt.Sprintf("addr-%d-%d", i, j)
						}
					}
				}
				return nil
			}}, nil
		}
		return &fakeDBRows{}, nil
	}
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(roleAggLoadSchema())

	fresh := &roleAggLoad{Name: "Ana", Matricula: "M1"}
	entity, existed, err := l.LoadSharedBaseIdentity(context.Background(), fresh)
	if err != nil || !existed {
		t.Fatalf("warm load: existed=%v err=%v", existed, err)
	}
	if entity.Name != "Ana" {
		t.Errorf("base field must be hydrated onto the new entity, got Name=%q", entity.Name)
	}
	items := domain.GetCurrentItemsOf[addrLoad](&entity.AggregateRoot)
	if len(items) != 2 {
		t.Fatalf("base-children must load as Constructor (for the request to dedup against), got %d", len(items))
	}
}

func TestLoadSharedBaseIdentity_ColdReturnsFresh(t *testing.T) {
	query := func(string, []any) (Rows, error) { return &fakeDBRows{}, nil } // base not found
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(roleAggLoadSchema())

	fresh := &roleAggLoad{Name: "Ana", Matricula: "M1"}
	entity, existed, err := l.LoadSharedBaseIdentity(context.Background(), fresh)
	if err != nil {
		t.Fatalf("cold load: %v", err)
	}
	if existed {
		t.Error("cold load must report existed=false (no shared identity yet)")
	}
	if entity != fresh {
		t.Error("cold load must return the same fresh entity (it carries the request)")
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
