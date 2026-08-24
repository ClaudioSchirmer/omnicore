//go:build postgres || mysql || sqlserver || oracle || sqlite

package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/relational"
)

// The wiring these tests cover is the seam between three pieces that each have
// their own unit tests but had never been exercised TOGETHER: the collector that
// walks the features, the validation across the three read-model families, and
// the registration that makes the read seam route a declared name to the
// relational engine instead of the fallback. A view that collects and validates
// but never routes is a feature that silently does nothing.

// countingReader stands in for the projection backing, so a dispatch that
// reaches the fallback is observable rather than merely wrong.
type countingReader struct{ hits int }

func (r *countingReader) ReadPage(context.Context, string, queries.ReadCriteria) (queries.Page, error) {
	r.hits++
	return queries.Page{Items: []map[string]any{{"backing": "fallback"}}}, nil
}

func (r *countingReader) ReadByID(context.Context, string, string, queries.ReadCriteria) (map[string]any, bool, error) {
	r.hits++
	return map[string]any{"backing": "fallback"}, true, nil
}

func relViewFor(name, table string) *query.RelationalViewDefinition {
	return query.RelationalView(name, relStub(table))
}

// A declared relational view must be ROUTED: its reads go to the relational
// engine, and every other name still falls through to the projection backing.
func TestRelationalWiring_RegisteredViewRoutesAwayFromTheFallback(t *testing.T) {
	fallback := &countingReader{}
	seam := query.NewViewReaderEngine(fallback)

	relViews := []*query.RelationalViewDefinition{relViewFor("gadgets_rel", "gadgets")}
	rel := relational.NewViewReader(relViews)
	if rel.Empty() {
		t.Fatal("a declared view must register on the engine")
	}
	seam.Register(rel, rel.ViewNames())

	// The relational engine answers for its own name — the read reaches it (the
	// stub loader returns nothing, which is a valid empty page, not a fallback).
	if _, err := seam.ReadPage(context.Background(), "gadgets_rel", queries.ReadCriteria{}); err != nil {
		t.Fatalf("the registered view must be served by the relational engine: %v", err)
	}
	if fallback.hits != 0 {
		t.Fatalf("a registered view must NOT reach the fallback, got %d hits", fallback.hits)
	}

	// Anything else still goes to the projection backing.
	page, err := seam.ReadPage(context.Background(), "gadgets", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("an unregistered view must reach the fallback: %v", err)
	}
	if fallback.hits != 1 || page.Items[0]["backing"] != "fallback" {
		t.Fatalf("unregistered reads must land on the fallback, got %d hits / %v", fallback.hits, page.Items)
	}
}

// The capability boundary lives in the ENGINE, and the wiring must not blunt it:
// a request the relational read cannot serve is refused THROUGH the seam, with
// the shared backing-neutral notification.
func TestRelationalWiring_TheEngineRefusalSurvivesTheSeam(t *testing.T) {
	seam := query.NewViewReaderEngine(&countingReader{})
	rel := relational.NewViewReader([]*query.RelationalViewDefinition{relViewFor("gadgets_rel", "gadgets")})
	seam.Register(rel, rel.ViewNames())

	_, err := seam.ReadPage(context.Background(), "gadgets_rel", queries.ReadCriteria{Search: "anything"})
	if err == nil {
		t.Fatal("free-text search must be refused by the relational engine")
	}
	// The refusal must arrive as a typed notification, not an opaque error: that
	// is what every surface renders as a 400 naming the capability.
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("the refusal must carry notifications, got %T: %v", err, err)
	}
	ctxs := carrier.NotificationContexts()
	if len(ctxs) != 1 || len(ctxs[0].Messages()) != 1 {
		t.Fatalf("expected one notification, got %d context(s)", len(ctxs))
	}
	msg := ctxs[0].Messages()[0]
	if got := reflect.TypeOf(msg.Notification).Name(); got != "UnsupportedCapabilityNotification" {
		t.Errorf("notification = %q, want the shared backing-neutral one", got)
	}
	if got := msg.Notification.Semantic(); got != domain.SemanticSchema {
		t.Errorf("semantic = %v, want SemanticSchema (400)", got)
	}
	if msg.FieldName != "search" {
		t.Errorf("the refusal must name the capability, got %q", msg.FieldName)
	}
}

// One resolver serves both families, and the wiring feeds it both — a relational
// view's ceiling must reach its engine, or the cascade would differ by backing.
func TestRelationalWiring_TheCeilingReachesTheRelationalEngine(t *testing.T) {
	capped := query.RelationalView("capped_rel", relStub("gadgets")).MaxLimit(2)
	relViews := []*query.RelationalViewDefinition{capped}

	seam := query.NewViewReaderEngine(&countingReader{})
	rel := relational.NewViewReader(relViews)
	rel.SetMaxLimitResolver(buildViewMaxLimitResolver(nil, relViews, 100))
	seam.Register(rel, rel.ViewNames())

	_, err := seam.ReadPage(context.Background(), "capped_rel", queries.ReadCriteria{Limit: 50})
	if err == nil {
		t.Fatal("a request over the per-view ceiling must be refused")
	}
}

// The three families share ONE namespace, and the boot validation is what
// enforces it. A relational name colliding with a projected one must abort.
func TestRelationalWiring_NameCollisionAbortsBeforeAnythingIsRegistered(t *testing.T) {
	mongo := []*query.ViewDefinition{query.View("gadgets").Schema(wiringSchema("gadgets"))}
	rel := []*query.RelationalViewDefinition{relViewFor("gadgets", "gadgets")}

	err := query.ValidateRelationalViews(rel, mongo, nil)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("a cross-family collision must abort the boot, got %v", err)
	}
}

// A service that declares no relational view registers nothing: the seam keeps
// an empty route and every read costs exactly what it did before the feature
// existed.
func TestRelationalWiring_NoDeclarationsRegisterNothing(t *testing.T) {
	fallback := &countingReader{}
	seam := query.NewViewReaderEngine(fallback)

	rel := relational.NewViewReader(nil)
	if !rel.Empty() {
		t.Fatal("no declarations must yield an empty reader")
	}
	// The wiring skips Register entirely when the reader is empty.
	if _, err := seam.ReadPage(context.Background(), "anything", queries.ReadCriteria{}); err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if fallback.hits != 1 {
		t.Fatalf("every read must reach the fallback, got %d hits", fallback.hits)
	}
}

type wiringEnt struct{ domain.BaseEntity }

func (e *wiringEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *wiringEnt) BuildRules(string, domain.Service, *domain.Rules) {}

func wiringSchema(table string) *core.TableSchema {
	return core.NewTableSchema[*wiringEnt](table).ID("id")
}
