//go:build postgres || mysql || sqlserver || oracle || sqlite

package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/gofiber/fiber/v3"
)

type relStubEntity struct{ domain.BaseEntity }

type relStubLoader struct{ schema *core.TableSchema }

func (l relStubLoader) FindAllEntities(context.Context, *criteria.Query) ([]domain.Entity, error) {
	return nil, nil
}
func (l relStubLoader) CountEntities(context.Context, *criteria.Query) (int64, error) { return 0, nil }
func (l relStubLoader) Schema() *core.TableSchema                                     { return l.schema }

func relStub(table string) relStubLoader {
	return relStubLoader{schema: core.NewTableSchema[*relStubEntity](table).ID("id")}
}

// relFeature declares relational read models; plainFeature declares none, so the
// type assertion skips it.
type relFeature struct {
	views []*query.RelationalViewDefinition
}

func (f *relFeature) Mount(*fiber.App, Deps) {}
func (f *relFeature) RelationalViews() []*query.RelationalViewDefinition {
	return f.views
}

type plainFeature struct{}

func (plainFeature) Mount(*fiber.App, Deps) {}

func TestCollectRelationalViews_AggregatesAcrossFeatures(t *testing.T) {
	a := &relFeature{views: []*query.RelationalViewDefinition{
		query.RelationalView("a_rel", relStub("a")),
	}}
	b := &relFeature{views: []*query.RelationalViewDefinition{
		query.RelationalView("b_rel", relStub("b")),
	}}

	got, err := collectRelationalViews([]Feature{a, plainFeature{}, b})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both features' views, got %d", len(got))
	}
}

// A feature that does not declare the method contributes nothing and costs
// nothing — the opt-in is the type assertion.
func TestCollectRelationalViews_IgnoresNonDeclaringFeatures(t *testing.T) {
	got, err := collectRelationalViews([]Feature{plainFeature{}})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a feature with no RelationalViews() must contribute nothing, got %d", len(got))
	}
}

func TestCollectRelationalViews_RejectsCollisionBetweenFeatures(t *testing.T) {
	a := &relFeature{views: []*query.RelationalViewDefinition{
		query.RelationalView("dup_rel", relStub("a")),
	}}
	b := &relFeature{views: []*query.RelationalViewDefinition{
		query.RelationalView("dup_rel", relStub("b")),
	}}

	_, err := collectRelationalViews([]Feature{a, b})
	if err == nil || !strings.Contains(err.Error(), "declared by both") {
		t.Fatalf("a name declared by two features must be rejected, got %v", err)
	}
}

// A nil entry in a feature's slice is skipped rather than dereferenced.
func TestCollectRelationalViews_SkipsNilEntries(t *testing.T) {
	f := &relFeature{views: []*query.RelationalViewDefinition{nil}}
	got, err := collectRelationalViews([]Feature{f})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a nil view must be skipped, got %d", len(got))
	}
}
