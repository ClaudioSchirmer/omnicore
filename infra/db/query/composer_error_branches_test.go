package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Per-merge error propagation through Compose/ComposeAll: a role view with a
// sibling, a shared base (with a native child) and an own child touches every
// merge stage, so failing the QueryMaps of one table at a time walks each
// error branch.

type composerRoleChild struct {
	ID    string
	Label string
}

func (c composerRoleChild) GetID() domain.ID                                 { return domain.NewID(c.ID) }
func (c composerRoleChild) BuildRules(string, domain.Service, *domain.Rules) {}

type composerRole struct {
	domain.AggregateRoot
	Name      string
	Matricula string
}

func (e *composerRole) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate}
}
func (e *composerRole) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *composerRole) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *composerRole) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{composerRoleChild{}}
}

func composerRoleView() *ViewDefinition {
	base := core.NewSharedBaseSchema("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("name").
		Child(core.NewTableSchema[composerRoleChild]("pessoa_filhos").
			PK("id").FK("pessoa_id").Field("Label", "label"))
	schema := core.NewTableSchema[*composerRole]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SoftDelete("deleted_at").
		Sibling(core.NewSiblingSchema[*composerRole]("aluno_extra").Field("Name", "name")).
		Child(core.NewTableSchema[composerRoleChild]("aluno_filhos").
			PK("id").FK("aluno_id").Field("Label", "label")).
		SharedBase(base, "pessoa_id")
	return View("alunos").Version(1).Schema(schema)
}

// composerMaps scripts QueryMaps per table: failSub errors, everything else
// yields one plausible row for the root and empty sets elsewhere.
func composerMaps(failSub string) func(string, []any) ([]map[string]any, error) {
	return func(sql string, _ []any) ([]map[string]any, error) {
		if failSub != "" && strings.Contains(sql, failSub) {
			return nil, errFake
		}
		if strings.Contains(sql, "FROM aluno") && !strings.Contains(sql, "FROM aluno_") {
			return []map[string]any{{"id": "r1", "matricula": "M1", "pessoa_id": "p1", "deleted_at": nil}}, nil
		}
		return nil, nil
	}
}

func composerViewEngine(failSub string) *fakeEngine {
	return newFakeEngine(&fakeQuerier{queryMapsFn: composerMaps(failSub)})
}

func TestCompose_MergeStageFailures(t *testing.T) {
	view := composerRoleView()
	// Each substring names the table one merge stage reads.
	for _, failSub := range []string{
		"FROM aluno",    // fetchRow (root)
		"aluno_extra",   // mergeOwnerSiblings
		"FROM pessoa",   // mergeSharedBase
		"pessoa_filhos", // mergeSharedBaseChildren
		"aluno_filhos",  // mergeOwnChildren
	} {
		t.Run(failSub, func(t *testing.T) {
			c := NewComposer(composerViewEngine(failSub))
			if _, err := c.Compose(context.Background(), view, "r1"); err == nil {
				t.Fatalf("failing %q must propagate", failSub)
			}
		})
	}

	t.Run("happyComposesAllStages", func(t *testing.T) {
		c := NewComposer(composerViewEngine(""))
		doc, err := c.Compose(context.Background(), view, "r1")
		if err != nil || doc == nil {
			t.Fatalf("Compose: %v, %v", doc, err)
		}
		if doc["matricula"] != "M1" {
			t.Errorf("root fields must survive the merges, got %v", doc)
		}
	})
}

func TestComposeAll_MergeStageFailures(t *testing.T) {
	view := composerRoleView()
	for _, failSub := range []string{
		"aluno_extra",
		"FROM pessoa",
		"pessoa_filhos",
		"aluno_filhos",
	} {
		t.Run(failSub, func(t *testing.T) {
			c := NewComposer(composerViewEngine(failSub))
			if _, err := c.ComposeAll(context.Background(), view); err == nil {
				t.Fatalf("failing %q must propagate", failSub)
			}
		})
	}
	t.Run("happy", func(t *testing.T) {
		c := NewComposer(composerViewEngine(""))
		docs, err := c.ComposeAll(context.Background(), view)
		if err != nil || len(docs) != 1 {
			t.Fatalf("ComposeAll: %v, %v", docs, err)
		}
	})
}
