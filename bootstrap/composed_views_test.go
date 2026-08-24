package bootstrap

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/gofiber/fiber/v3"
)

type cvBootGadget struct{ ID, Code string }

func cvBootPrimary() *query.ViewDefinition {
	schema := core.NewTableSchema[cvBootGadget]("gadgets").
		ID("id").Field("Code", "code").DeletedAt("deleted_at")
	return query.View("gadgets").Version(1).Schema(schema)
}

func cvBootComposed(name string) *query.ComposedViewDefinition {
	return query.ComposedView(name).
		Primary(cvBootPrimary()).
		Link(query.JoinUpstream(
			core.NewExternalSchema("upstream_gadgets").ID("id").Field("Code", "code"),
			"UpstreamMirror", "upstreamMirror")).On("id")
}

type composingStubFeature struct {
	composed []*query.ComposedViewDefinition
}

func (f *composingStubFeature) Mount(app *fiber.App, d Deps) {}
func (f *composingStubFeature) ComposedViews() []*query.ComposedViewDefinition {
	return f.composed
}

func TestCollectComposedViews(t *testing.T) {
	plain := &writeOnlyFeature{}
	composing := &composingStubFeature{composed: []*query.ComposedViewDefinition{
		cvBootComposed("gadgets_full"), nil, // nil entries are skipped
	}}
	out, err := collectComposedViews([]Feature{plain, composing})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 || out[0].Name() != "gadgets_full" {
		t.Fatalf("unexpected collection: %+v", out)
	}
}

func TestCollectComposedViews_RejectsCrossFeatureCollision(t *testing.T) {
	a := &composingStubFeature{composed: []*query.ComposedViewDefinition{cvBootComposed("gadgets_full")}}
	b := &composingStubFeature{composed: []*query.ComposedViewDefinition{cvBootComposed("gadgets_full")}}
	_, err := collectComposedViews([]Feature{a, b})
	if err == nil || !strings.Contains(err.Error(), "declared by both") {
		t.Fatalf("expected the cross-feature collision rejection, got: %v", err)
	}
}

func TestUpstreamCollectionSet(t *testing.T) {
	set := upstreamCollectionSet([]UpstreamSubscription{
		{Topic: "a.events", Collection: "upstream_a"},
		{Topic: "b.events", Collection: "upstream_b"},
	})
	if !set["upstream_a"] || !set["upstream_b"] || len(set) != 2 {
		t.Fatalf("unexpected set: %#v", set)
	}
}

// The list form is what the DB-per-service guard claims: declaration order, and
// nothing for an entry with no collection (the declaration guard rejects that
// entry itself — this only proves the empty name never reaches the claimed set,
// where it would whitelist a collection literally named "").
func TestUpstreamCollectionNames(t *testing.T) {
	got := upstreamCollectionNames([]UpstreamSubscription{
		{Topic: "a.events", Collection: "upstream_a"},
		{Topic: "bad.events"},
		{Topic: "b.events", Collection: "upstream_b"},
	})
	want := []string{"upstream_a", "upstream_b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(upstreamCollectionNames(nil)) != 0 {
		t.Error("no subscriptions must claim no collections")
	}
}

func TestQueryConfig_MaxLinkManyLimitValidation(t *testing.T) {
	q := &QueryConfig{MaxLimit: 100, MaxLinkManyLimit: -1}
	if err := q.validate(); err == nil || !strings.Contains(err.Error(), "maxLinkManyLimit") {
		t.Fatalf("expected the negative maxLinkManyLimit rejection, got: %v", err)
	}
	q.MaxLinkManyLimit = 50
	if err := q.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
