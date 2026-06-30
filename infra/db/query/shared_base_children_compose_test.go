package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// White-box coverage for the SharedBase native-children compose: the base's
// children nest into the role document under their derived Go segment, and the
// ViewNode round-trips that nested collection to Go vocabulary.

func composeRoleWithBaseChild() *core.TableSchema {
	base := core.NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name").
		Child(core.NewTableSchema[fakeVO]("endereco").PK("id").FK("pessoa_id").Field("Label", "street"))
	return core.NewTableSchema[*builderTestEntity]("aluno").
		PK("id").
		Field("Email", "email").
		SharedBase(base, "pessoa_id")
}

func TestCompose_NestsSharedBaseChildren(t *testing.T) {
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM endereco"):
			return mapsFromColsData([]string{"id", "pessoa_id", "street"},
				[][]any{{"e1", "p1", "Main St"}, {"e2", "p1", "2nd Ave"}}), nil
		case strings.Contains(sql, "FROM pessoa"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"p1", "Ana"}}), nil
		case strings.Contains(sql, "FROM aluno"):
			return mapsFromColsData([]string{"id", "email", "pessoa_id"}, [][]any{{"a1", "a@x", "p1"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)
	schema := composeRoleWithBaseChild()
	view := View("aluno").Version(1).Root("aluno").Schema(schema)
	seg := domain.PluralizeWord("fakeVO") // the derived doc field + Go segment

	doc, err := c.Compose(context.Background(), view, "a1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	// The base fields still merge FLAT; the base CHILDREN nest under the segment.
	if doc["name"] != "Ana" || doc["email"] != "a@x" {
		t.Errorf("base fields must still merge flat, got %v", doc)
	}
	nested, ok := doc[seg].([]Document)
	if !ok || len(nested) != 2 {
		t.Fatalf("base-children must nest under %q as a 2-element collection, got %#v", seg, doc[seg])
	}

	// ViewNode round-trips the nested collection into Go vocabulary (column→Go).
	goDoc := view.BuildViewNode().ToGoDoc(doc)
	goNested, ok := goDoc[seg].([]any)
	if !ok || len(goNested) != 2 {
		t.Fatalf("ToGoDoc must keep the base-children collection under %q, got %#v", seg, goDoc[seg])
	}
	first, _ := goNested[0].(map[string]any)
	if first["Label"] != "Main St" {
		t.Errorf("base-child column must translate to its Go field (street→Label), got %#v", first)
	}
}
