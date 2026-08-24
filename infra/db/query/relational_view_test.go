package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// stubLoader is an AggregateReader carrying only what a declaration reads: the
// schema. The two read methods are never reached by declaration-time code.
type stubLoader struct{ schema *core.TableSchema }

func (s stubLoader) FindAllEntities(context.Context, *criteria.Query) ([]domain.Entity, error) {
	return nil, nil
}
func (s stubLoader) CountEntities(context.Context, *criteria.Query) (int64, error) { return 0, nil }
func (s stubLoader) Schema() *core.TableSchema                                     { return s.schema }
func (s stubLoader) JoinFields() map[string][]string                               { return nil }

func relLoader(table string) stubLoader { return stubLoader{schema: rootSchema(table)} }

// The loader is the single structural input: the view takes its schema and its
// root table FROM it, so the two can never disagree.
func TestRelationalView_SchemaComesFromTheLoader(t *testing.T) {
	v := RelationalView("things_rel", relLoader("things"))

	if v.Name() != "things_rel" {
		t.Errorf("Name() = %q", v.Name())
	}
	if v.SchemaDef() == nil || v.SchemaDef().Table() != "things" {
		t.Fatalf("SchemaDef() must come from the loader, got %v", v.SchemaDef())
	}
	if v.RootTable() != "things" {
		t.Errorf("RootTable() = %q, want %q", v.RootTable(), "things")
	}
	if v.Loader() == nil {
		t.Error("Loader() must expose the reader the view was declared over")
	}
}

// A nil loader leaves the accessors safe (nil schema, empty table) rather than
// panicking — the declaration is rejected by ValidateRelationalViews at boot.
func TestRelationalView_NilLoaderAccessorsAreSafe(t *testing.T) {
	v := RelationalView("broken", nil)
	if v.SchemaDef() != nil {
		t.Error("SchemaDef() on a nil loader must be nil")
	}
	if v.RootTable() != "" {
		t.Errorf("RootTable() on a nil loader must be empty, got %q", v.RootTable())
	}
}

func TestRelationalView_Ceilings(t *testing.T) {
	plain := RelationalView("a_rel", relLoader("a"))
	if plain.MaxLimitValue() != 0 || plain.MaxExportRowsValue() != 0 {
		t.Error("an undeclared ceiling must read as 0 (defer to the yaml default)")
	}
	// Unset defers to the yaml default; a zero yaml default falls to the framework.
	if got := plain.ResolveMaxExportRows(500); got != 500 {
		t.Errorf("unset + yaml 500 = %d, want 500", got)
	}
	if got := plain.ResolveMaxExportRows(0); got != DefaultMaxExportRows {
		t.Errorf("unset + no yaml = %d, want %d", got, DefaultMaxExportRows)
	}

	capped := RelationalView("b_rel", relLoader("b")).MaxLimit(5).MaxExportRows(3)
	if capped.MaxLimitValue() != 5 {
		t.Errorf("MaxLimitValue() = %d, want 5", capped.MaxLimitValue())
	}
	// The per-view override wins over the yaml default.
	if got := capped.ResolveMaxExportRows(500); got != 3 {
		t.Errorf("per-view override = %d, want 3", got)
	}
}

// The translator tree is built from the schema alone — no embed segments, no
// roles, because a relational view declares neither.
func TestRelationalView_BuildViewNodeFromSchemaAlone(t *testing.T) {
	n := RelationalView("things_rel", relLoader("things")).BuildViewNode()
	if n == nil || !n.hasSchema() {
		t.Fatal("BuildViewNode must carry the loader's schema")
	}
	if len(n.embeds) != 0 {
		t.Errorf("a relational view node must register no embed segments, got %d", len(n.embeds))
	}
}

func TestValidateRelationalViews_AcceptsAWellFormedSet(t *testing.T) {
	views := []*RelationalViewDefinition{
		RelationalView("a_rel", relLoader("a")),
		RelationalView("b_rel", relLoader("b")),
	}
	if err := ValidateRelationalViews(views, nil, nil); err != nil {
		t.Fatalf("well-formed set rejected: %v", err)
	}
}

func TestValidateRelationalViews_RejectsNilLoader(t *testing.T) {
	err := ValidateRelationalViews([]*RelationalViewDefinition{RelationalView("a_rel", nil)}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "nil loader") {
		t.Fatalf("a nil loader must be rejected, got %v", err)
	}
}

func TestValidateRelationalViews_RejectsSchemalessLoader(t *testing.T) {
	err := ValidateRelationalViews(
		[]*RelationalViewDefinition{RelationalView("a_rel", stubLoader{})}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no schema bound") {
		t.Fatalf("a schemaless loader must be rejected, got %v", err)
	}
}

func TestValidateRelationalViews_RejectsEmptyName(t *testing.T) {
	err := ValidateRelationalViews(
		[]*RelationalViewDefinition{RelationalView("", relLoader("a"))}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "empty name") {
		t.Fatalf("an empty name must be rejected, got %v", err)
	}
}

// A read-model name is ONE namespace across every backing: the surfaces address a
// name, not a store, so a collision with either other family aborts the boot.
func TestValidateRelationalViews_RejectsCollisionWithAView(t *testing.T) {
	mongo := []*ViewDefinition{View("things").Schema(rootSchema("things"))}
	err := ValidateRelationalViews(
		[]*RelationalViewDefinition{RelationalView("things", relLoader("things"))}, mongo, nil)
	if err == nil || !strings.Contains(err.Error(), "collides with a view") {
		t.Fatalf("collision with a Mongo view must be rejected, got %v", err)
	}
}

func TestValidateRelationalViews_RejectsCollisionWithAComposedView(t *testing.T) {
	composed := []*ComposedViewDefinition{ComposedView("things_full")}
	err := ValidateRelationalViews(
		[]*RelationalViewDefinition{RelationalView("things_full", relLoader("things"))}, nil, composed)
	if err == nil || !strings.Contains(err.Error(), "collides with a composed view") {
		t.Fatalf("collision with a composed view must be rejected, got %v", err)
	}
}

func TestValidateRelationalViews_RejectsCollisionBetweenTwoRelationalViews(t *testing.T) {
	views := []*RelationalViewDefinition{
		RelationalView("dup_rel", relLoader("a")),
		RelationalView("dup_rel", relLoader("b")),
	}
	err := ValidateRelationalViews(views, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "collides with a relational view") {
		t.Fatalf("two relational views of the same name must be rejected, got %v", err)
	}
}

// A nil entry in the slice is skipped, not dereferenced.
func TestValidateRelationalViews_SkipsNilEntries(t *testing.T) {
	if err := ValidateRelationalViews([]*RelationalViewDefinition{nil}, nil, nil); err != nil {
		t.Fatalf("a nil entry must be skipped, got %v", err)
	}
}

// The reserved slot suffixes are refused in EVERY read-model family, because the
// three share one namespace and a rule that held in one of them would be the
// harder thing to remember. This is the relational family's half of that.
func TestValidateRelationalViews_RefusesAReservedSlotSuffix(t *testing.T) {
	err := ValidateRelationalViews([]*RelationalViewDefinition{
		RelationalView("gadgets__0", relLoader("gadgets")),
	}, nil, nil)
	if err == nil {
		t.Fatal("a name ending in a blue-green slot suffix must abort the boot")
	}
	if !strings.Contains(err.Error(), "the framework reserves") {
		t.Errorf("the abort must explain the rule, got: %v", err)
	}
}
