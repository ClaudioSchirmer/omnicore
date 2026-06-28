package relational

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

func TestAggregateLoader_FluentSettersChain(t *testing.T) {
	rootScanner := func(Row) (*aggLoaderTestEntity, error) { return nil, nil }
	childScanner := func(Rows) (domain.AggregateValueObject, error) { return nil, nil }

	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).
		WithContextName("AggLoaderTest").
		WithRootScanner(rootScanner).
		WithChildScanner("Address", childScanner).
		WithChildScanner("Phone", childScanner)

	if l.contextName != "AggLoaderTest" {
		t.Errorf("expected contextName %q, got %q", "AggLoaderTest", l.contextName)
	}
	if l.rootScanner == nil {
		t.Errorf("expected rootScanner to be set")
	}
	if got := len(l.childScanners); got != 2 {
		t.Errorf("expected 2 child scanners registered, got %d", got)
	}
	if _, ok := l.childScanners["Address"]; !ok {
		t.Errorf("expected scanner key %q", "Address")
	}
	if _, ok := l.childScanners["Phone"]; !ok {
		t.Errorf("expected scanner key %q", "Phone")
	}
}

func TestAggregateLoader_NewInitializesChildScannersMap(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity)
	if l.childScanners == nil {
		t.Fatal("expected childScanners map to be initialized (non-nil)")
	}
	if len(l.childScanners) != 0 {
		t.Errorf("expected empty childScanners map, got len=%d", len(l.childScanners))
	}
}

func TestAggregateLoader_WithChildScannerReplacesOnSameKey(t *testing.T) {
	first := func(Rows) (domain.AggregateValueObject, error) { return nil, nil }
	second := func(Rows) (domain.AggregateValueObject, error) { return nil, nil }

	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).
		WithChildScanner("Address", first).
		WithChildScanner("Address", second)

	if got := len(l.childScanners); got != 1 {
		t.Fatalf("expected 1 scanner after replacement, got %d", got)
	}
}

// ─── Auto-scan ────────────────────────────────────────────────────────────────

// fakeVO is an AggregateValueObject value-type used in auto-scan tests.
type fakeVO struct {
	ID    string
	Label string
}

func (v fakeVO) GetID() string                                    { return v.ID }
func (v fakeVO) BuildRules(string, domain.Service, *domain.Rules) {}

// A child's read column plan comes from its TableSchema (the same the write
// side uses); scanPlan INCLUDES the PK column (the child's ID is scanned).
func TestChildSchema_ScanPlanIncludesPK(t *testing.T) {
	child := NewTableSchema[fakeVO]("tags").
		PK("id").
		Field("Label", "label")
	cols, byCol := child.ScanPlan()
	wantCols := []string{"id", "label"}
	if len(cols) != 2 || cols[0] != wantCols[0] || cols[1] != wantCols[1] {
		t.Errorf("cols = %v, want %v", cols, wantCols)
	}
	if _, ok := byCol["id"]; !ok {
		t.Error("byCol must resolve the id column")
	}
}

// The loader auto-scans every child declared on the schema unless a manual
// scanner overrides it by type name.
func TestAggregateLoader_WithSchema_DrivesChildren(t *testing.T) {
	manualScanner := func(Rows) (domain.AggregateValueObject, error) { return nil, nil }
	root := NewTableSchema[*aggLoaderTestEntity]("agg").
		Child(NewTableSchema[fakeVO]("tags").PK("id").FK("agg_id").Field("Label", "label"))
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).
		WithSchema(root).
		WithChildScanner("Manual", manualScanner)

	if l.schema == nil || l.schema.ChildSchema("fakeVO") == nil {
		t.Fatal("schema child fakeVO must be registered")
	}
	if len(l.childScanners) != 1 {
		t.Errorf("manual scanners = %d, want 1", len(l.childScanners))
	}
}

func TestAggregateLoader_EffectiveContextName_ExplicitWins(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).
		WithContextName("AdminEnt")
	if got := l.effectiveContextName(); got != "AdminEnt" {
		t.Errorf("explicit override must win, got %q", got)
	}
}

func TestAggregateLoader_EffectiveContextName_DerivedFromT(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity)
	if got := l.effectiveContextName(); got != "aggLoaderTestEntity" {
		t.Errorf("expected derived name %q, got %q", "aggLoaderTestEntity", got)
	}
}
