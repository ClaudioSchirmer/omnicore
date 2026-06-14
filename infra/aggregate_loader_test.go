package infra

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/jackc/pgx/v5"
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
	rootScanner := func(pgx.Row) (*aggLoaderTestEntity, error) { return nil, nil }
	childScanner := func(pgx.Rows) (domain.AggregateValueObject, error) { return nil, nil }

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
	first := func(pgx.Rows) (domain.AggregateValueObject, error) { return nil, nil }
	second := func(pgx.Rows) (domain.AggregateValueObject, error) { return nil, nil }

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

func TestAggregateLoader_NewInitializesChildAutoMap(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity)
	if l.childAuto == nil {
		t.Fatal("childAuto map must be initialized")
	}
	if len(l.childAuto) != 0 {
		t.Errorf("childAuto map must be empty, got %d", len(l.childAuto))
	}
}

// Phase 19: WithChild[V] auto-registers typeName via reflect.Type.Name() and cols
// via reflection on the exported fields. The table is resolved at Load time via
// resolveChildTable (not stored in the spec).
func TestAggregateLoader_WithChild_RegistersTypeNameAndCols(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity)
	l = WithChild[fakeVO](l)

	spec, ok := l.childAuto["fakeVO"]
	if !ok {
		t.Fatal("fakeVO should be registered in childAuto")
	}
	// Read side via domainColumns INCLUDES "id" (needed to populate the struct).
	// Write side (InferColumns in infer.go) is what filters "id" out.
	wantCols := []string{"id", "label"}
	if got := spec.columns; len(got) != 2 || got[0] != wantCols[0] || got[1] != wantCols[1] {
		t.Errorf("cols = %v, want %v", got, wantCols)
	}
	if spec.scanInto == nil {
		t.Error("scanInto must be set")
	}
}

func TestAggregateLoader_AutoAndManualCoexist(t *testing.T) {
	manualScanner := func(pgx.Rows) (domain.AggregateValueObject, error) { return nil, nil }
	l := NewAggregateLoader[*aggLoaderTestEntity](nil, newAggLoaderTestEntity).
		WithChildScanner("Manual", manualScanner)
	l = WithChild[fakeVO](l)

	if len(l.childScanners) != 1 || len(l.childAuto) != 1 {
		t.Errorf("manual=%d auto=%d, want 1 e 1", len(l.childScanners), len(l.childAuto))
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
