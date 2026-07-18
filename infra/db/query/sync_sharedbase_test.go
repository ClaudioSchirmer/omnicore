package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Coverage for the SharedBase (M2) CDC fan-out: buildViewIndex registers a role
// view under its base table, and SyncEngine.process recomposes every role
// document referencing a changed shared identity.

func fanOutRoleSchema() *core.TableSchema {
	base := core.NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name")
	return core.NewTableSchema[*builderTestEntity]("aluno").
		PK("id").
		Field("Email", "email").
		SharedBase(base, "pessoa_id")
}

func TestBuildViewIndex_RegistersSharedBase(t *testing.T) {
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	idx := buildViewIndex([]*ViewDefinition{view})
	if got := len(idx.bySharedBase["pessoa"]); got != 1 {
		t.Fatalf("bySharedBase[pessoa] = %d views, want 1", got)
	}
	// A base table is not a view root, so it must not land in byPGTable.
	if _, ok := idx.byPGTable["pessoa"]; ok {
		t.Errorf("a shared base must not be indexed as a view root")
	}
}

// A base event recomposes the role docs referencing that identity (fan-out).
func TestProcess_SharedBaseFanOut(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}} // one aluno references the base
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"p1", "Ana"}}), nil
		case strings.Contains(sql, "FROM aluno"):
			return mapsFromColsData([]string{"id", "email", "pessoa_id"}, [][]any{{"a1", "a@x", "p1"}}), nil
		}
		return nil, nil
	})
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	// A base change (aggregate_type = the base table) fans out to the role views.
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "pessoa", EventType: "UPDATED", AggregateID: "p1"}); err != nil {
		t.Fatalf("process base event: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Errorf("base fan-out must recompose the referencing role doc, got %d upserts", len(coll.updates))
	}
}
