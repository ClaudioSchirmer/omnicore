package write

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Pure-function coverage for audit_builder.go helpers — nil-guards and the
// "nothing observable" branches that the Build*Event happy paths do not reach.

func TestLabelKeysByGoField_NilSchema(t *testing.T) {
	if out := (*core.TableSchema)(nil).LabelKeysByGoField(); out != nil {
		t.Errorf("nil schema must yield nil label map, got %v", out)
	}
}

func TestFilterClaims_NoOverlapReturnsNil(t *testing.T) {
	// allowlist non-empty but none of its keys are present in `all`.
	if out := filterClaims(map[string]any{"present": 1}, []string{"absent"}); out != nil {
		t.Errorf("no overlap must yield nil, got %v", out)
	}
	if out := filterClaims(nil, []string{"x"}); out != nil {
		t.Errorf("empty claims must yield nil, got %v", out)
	}
}

func TestGoFieldValues_ExternalSchemaSkipsUnindexed(t *testing.T) {
	// A type-less external schema carries fields with index < 0, so every field
	// is skipped — the index<0 guard branch.
	ext := NewExternalSchema("users").ID("id").Field("Name", "name")
	out := ext.GoFieldValues(struct{ Name string }{Name: "x"})
	if len(out) != 0 {
		t.Errorf("external goFieldValues must skip unindexed fields, got %v", out)
	}
}

func TestGoFieldValues_NonStructReturnsEmpty(t *testing.T) {
	if out := builderTestSchema.GoFieldValues(42); len(out) != 0 {
		t.Errorf("non-struct must yield empty map, got %v", out)
	}
	if out := (*core.TableSchema)(nil).GoFieldValues(&builderTestEntity{}); len(out) != 0 {
		t.Errorf("nil schema must yield empty map, got %v", out)
	}
}

func TestChildrenOf_NilGuards(t *testing.T) {
	if out := childrenOf(nil, &builderTestEntity{}, "insert"); out != nil {
		t.Errorf("nil schema must yield nil children, got %v", out)
	}
	if out := childrenOf(builderTestSchema, nil, "insert"); out != nil {
		t.Errorf("nil src must yield nil children, got %v", out)
	}
}

func TestChildrenOf_FlatEntityHasNoChildren(t *testing.T) {
	// A non-aggregate entity is not an AggregateRootProvider → nil.
	if out := childrenOf(builderTestSchema, &builderTestEntity{Name: "x"}, "insert"); out != nil {
		t.Errorf("flat entity must yield nil children, got %v", out)
	}
}

// nilRootProvider is an AggregateRootProvider whose GetAggregateRoot returns
// nil — exercising the root==nil guard in childrenOf.
type nilRootProvider struct{ domain.BaseEntity }

func (n *nilRootProvider) Modes() []domain.EntityMode                       { return nil }
func (n *nilRootProvider) BuildRules(string, domain.Service, *domain.Rules) {}
func (n *nilRootProvider) GetAggregateRoot() *domain.AggregateRoot          { return nil }
func (n *nilRootProvider) AggregateChildren() []domain.AggregateValueObject { return nil }

func TestChildrenOf_NilRootReturnsNil(t *testing.T) {
	if out := childrenOf(builderTestSchema, &nilRootProvider{}, "insert"); out != nil {
		t.Errorf("provider with nil root must yield nil children, got %v", out)
	}
}

func TestOldChildrenIndex_NilGuards(t *testing.T) {
	if out := oldChildrenIndex(nil, &covAgg{}); out != nil {
		t.Errorf("nil schema must yield nil index, got %v", out)
	}
	if out := oldChildrenIndex(covAggSchema, nil); out != nil {
		t.Errorf("nil src must yield nil index, got %v", out)
	}
}

// An update whose only child is an untouched Constructor item produces no
// observable child event: childEventOf returns include=false, so childrenOf
// skips it and ultimately returns nil (no children block).
func TestChildrenOf_UpdateConstructorChildSkipped(t *testing.T) {
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	root.AggregateConstructor([]domain.AggregateValueObject{covChild{ID: "c1", Label: "x"}})
	u, err := domain.GetUpdatable(root, func(*covAgg) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	ev := BuildUpdateEvent(newBuilderCtx(), u, covAggSchema, nil)
	if ev.Children != nil {
		t.Errorf("untouched Constructor child must produce no children block, got %v", ev.Children)
	}
}
