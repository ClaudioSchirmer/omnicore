package read

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

type aggLoaderTestEntity struct {
	domain.BaseEntity
}

func (e *aggLoaderTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *aggLoaderTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func newAggLoaderTestEntity() *aggLoaderTestEntity { return &aggLoaderTestEntity{} }

// ─── Auto-scan ────────────────────────────────────────────────────────────────

// fakeVO is an AggregateValueObject value-type used in auto-scan tests.
type fakeVO struct {
	domain.Managed
	Label string
}

func (v fakeVO) BuildRules(string, domain.Service, *domain.Rules) {}

// A child's id now lives in the unexported domain.Managed carrier (idIndex < 0),
// so its ScanPlan — like the root's — EXCLUDES the id column: the loader reads it
// as a trailing carrier column and stamps it via SetID, never into a struct field.
func TestChildSchema_ScanPlanExcludesID(t *testing.T) {
	child := NewTableSchema[fakeVO]("tags").
		ID("id").
		Field("Label", "label")
	cols, byCol := child.ScanPlan()
	if len(cols) != 1 || cols[0] != "label" {
		t.Errorf("cols = %v, want [label] (the id is a trailing carrier column, not in the scan plan)", cols)
	}
	if _, ok := byCol["id"]; ok {
		t.Error("byCol must NOT resolve the id column — it left the scan plan for domain.Managed")
	}
	if child.IDColumn() != "id" {
		t.Errorf("the id column must still be declared: %q", child.IDColumn())
	}
}

// The schema alone drives which children are scanned: every child it declares,
// and nothing else — there is no per-type override.
func TestAggregateLoader_WithSchema_DrivesChildren(t *testing.T) {
	root := NewTableSchema[*aggLoaderTestEntity]("agg").
		Child(NewTableSchema[fakeVO]("tags").ID("id").ParentID("agg_id").Field("Label", "label"))
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).WithSchema(root)

	if l.schema == nil || l.schema.ChildSchema("fakeVO") == nil {
		t.Fatal("schema child fakeVO must be registered")
	}
	if names := l.schema.ChildSchemaNames(); len(names) != 1 {
		t.Errorf("declared children = %v, want exactly one", names)
	}
}

// The fluent setters that remain: WithContextName and WithSchema. The loader has
// no scan-override seat any more — the TableSchema is the one mapping.
func TestAggregateLoader_FluentSettersChain(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).
		WithContextName("AggLoaderTest")
	if l.contextName() != "AggLoaderTest" {
		t.Errorf("contextName = %q, want %q", l.contextName(), "AggLoaderTest")
	}
}

func TestAggregateLoader_EffectiveContextName_ExplicitWins(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).
		WithContextName("AdminEnt")
	if got := l.contextName(); got != "AdminEnt" {
		t.Errorf("explicit override must win, got %q", got)
	}
}

func TestAggregateLoader_EffectiveContextName_DerivedFromT(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity)
	if got := l.contextName(); got != "aggLoaderTestEntity" {
		t.Errorf("expected derived name %q, got %q", "aggLoaderTestEntity", got)
	}
}
