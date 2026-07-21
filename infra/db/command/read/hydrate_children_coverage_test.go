package read

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// White-box coverage for hydrateChildren / hydrateBaseChildren edge branches:
// manual-scanner row loop, no-columns configuration guards, and the per-branch
// query/scan/decode error propagation.

func TestHydrateChildren_NoEntitiesIsNoop(t *testing.T) {
	query := func(string, []any) (Rows, error) {
		t.Fatal("hydrateChildren must not query with no entities")
		return nil, nil
	}
	l := newCovAggLoader(fakeEngine(query), covAggSchema)
	if err := l.hydrateChildren(context.Background(), nil, nil, criteria.ScopeActive); err != nil {
		t.Fatalf("hydrateChildren: %v", err)
	}
}

func TestHydrateChildren_ManualScannerRowsAttach(t *testing.T) {
	var manualSQL string
	mapsFn := func(sql string, _ []any) ([]map[string]any, error) {
		manualSQL = sql
		return []map[string]any{{}, {}}, nil // 2 rows; manual scanner owns the decode, content irrelevant
	}
	n := 0
	manual := func(map[string]any) (domain.AggregateValueObject, error) {
		n++
		return covChild{ID: "c" + string(rune('0'+n)), Label: "L"}, nil
	}
	l := newCovAggLoader(fakeEngineWithMaps(nil, mapsFn), covAggSchema).WithChildScanner("covChild", manual)

	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID("r1"))
	if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, criteria.ScopeActive); err != nil {
		t.Fatalf("hydrateChildren: %v", err)
	}
	// The manual path is one explicit-column SELECT per root, FK-filtered — never SELECT *.
	if !strings.Contains(manualSQL, "FROM cov_children WHERE cov_agg_id = $1") || strings.Contains(manualSQL, "SELECT *") {
		t.Errorf("manual child SELECT wrong (must name columns, FK-filtered): %q", manualSQL)
	}
	items := domain.GetCurrentItemsOf[covChild](&root.AggregateRoot)
	if len(items) != 2 {
		t.Fatalf("manual-scanned children not attached: got %d, want 2", len(items))
	}
}

func TestHydrateChildren_ManualScannerRowErrorPropagates(t *testing.T) {
	mapsFn := func(string, []any) ([]map[string]any, error) { return []map[string]any{{}}, nil }
	manual := func(map[string]any) (domain.AggregateValueObject, error) { return nil, errFakeDB }
	l := newCovAggLoader(fakeEngineWithMaps(nil, mapsFn), covAggSchema).WithChildScanner("covChild", manual)

	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID("r1"))
	if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, criteria.ScopeActive); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected the manual scanner error, got %v", err)
	}
}

func TestHydrateChildren_ChildSchemaWithoutColumnsErrors(t *testing.T) {
	schema := NewTableSchema[*covAgg]("cov_aggs").PK("id").Revision("revision").Field("Name", "name").
		Child(noColsChildSchema("cov_agg_id"))
	l := newCovAggLoader(fakeEngine(nil), schema)

	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID("r1"))
	err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, criteria.ScopeActive)
	if err == nil || !strings.Contains(err.Error(), "schema declares no columns") {
		t.Fatalf("expected the child no-columns configuration error, got %v", err)
	}
}

func TestHydrateChildren_AutoScanBranchErrors(t *testing.T) {
	childRow := func(vals []string) func(string, []any) (Rows, error) {
		return func(string, []any) (Rows, error) {
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				for j, d := range dest {
					if p, ok := d.(*string); ok && j < len(vals) {
						*p = vals[j]
					}
				}
				return nil
			}}, nil
		}
	}
	cases := []struct {
		name string
		eng  RelationalEngine
	}{
		{"queryError", fakeEngine(func(string, []any) (Rows, error) { return nil, errFakeDB })},
		{"scanError", fakeEngine(func(string, []any) (Rows, error) {
			return &fakeDBRows{rows: 1, scan: func(int, []any) error { return errFakeDB }}, nil
		})},
		// FK is the leading key → its DecodeID failure is the fk-decode branch.
		{"fkDecodeError", decodeErrFakeEngine(childRow([]string{"bad-fk", "c1", "L"}), "bad-fk")},
		// The child's own PK is decoded after the scan (decodeChildPK).
		{"childPKDecodeError", decodeErrFakeEngine(childRow([]string{"r1", "bad-pk", "L"}), "bad-pk")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newCovAggLoader(tc.eng, covAggSchema)
			root := &covAgg{Name: "a"}
			root.SetID(domain.NewID("r1"))
			if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, criteria.ScopeActive); !errors.Is(err, errFakeDB) {
				t.Fatalf("%s: expected errFakeDB, got %v", tc.name, err)
			}
		})
	}
}

// ─── hydrateBaseChildren ─────────────────────────────────────────────────────

// flatRoleWithBaseChildrenSchema anchors a FLAT role (no AggregateRootProvider)
// on a shared base that DOES declare native children — the shape that reaches
// the provider check inside hydrateBaseChildren.
func flatRoleWithBaseChildrenSchema() *TableSchema {
	base := NewSharedBase("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("name").
		Child(NewTableSchema[addrLoad]("endereco").PK("id").FK("pessoa_id").Field("Street", "street"))
	return NewTableSchema[*roleLoadEntity]("aluno").
		PK("id").
		Field("Matricula", "matricula").
		SharedBase(base, "pessoa_id")
}

func TestHydrateBaseChildren_NoSharedBaseIsNoop(t *testing.T) {
	query := func(string, []any) (Rows, error) {
		t.Fatal("hydrateBaseChildren must not query without a shared base")
		return nil, nil
	}
	l := newCovAggLoader(fakeEngine(query), covAggSchema)
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID("r1"))
	if err := l.hydrateBaseChildren(context.Background(), []*covAgg{root}, []string{"r1"}, criteria.ScopeActive); err != nil {
		t.Fatalf("hydrateBaseChildren: %v", err)
	}
}

func TestHydrateBaseChildren_FlatEntityIsNoop(t *testing.T) {
	query := func(string, []any) (Rows, error) {
		t.Fatal("hydrateBaseChildren must not query for a flat entity")
		return nil, nil
	}
	l := NewAggregateLoader[*roleLoadEntity](fakeEngine(query), func() *roleLoadEntity { return &roleLoadEntity{} }).
		WithSchema(flatRoleWithBaseChildrenSchema())
	e := &roleLoadEntity{Matricula: "M1"}
	e.SetID(domain.NewID("a1"))
	if err := l.hydrateBaseChildren(context.Background(), []*roleLoadEntity{e}, []string{"a1"}, criteria.ScopeActive); err != nil {
		t.Fatalf("hydrateBaseChildren: %v", err)
	}
}

func TestHydrateBaseChildren_NoEntitiesIsNoop(t *testing.T) {
	query := func(string, []any) (Rows, error) {
		t.Fatal("hydrateBaseChildren must not query with no entities")
		return nil, nil
	}
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(roleAggLoadSchema())
	if err := l.hydrateBaseChildren(context.Background(), nil, nil, criteria.ScopeActive); err != nil {
		t.Fatalf("hydrateBaseChildren: %v", err)
	}
}

func TestHydrateBaseChildren_BaseChildWithoutColumnsErrors(t *testing.T) {
	base := NewSharedBase("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("name").
		Child(noColsChildSchema("pessoa_id"))
	schema := NewTableSchema[*roleAggLoad]("aluno").
		PK("id").Revision("revision").
		Field("Matricula", "matricula").
		SharedBase(base, "pessoa_id")
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(nil), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(schema)

	e := &roleAggLoad{Matricula: "M1"}
	e.SetID(domain.NewID("a1"))
	err := l.hydrateBaseChildren(context.Background(), []*roleAggLoad{e}, []string{"a1"}, criteria.ScopeActive)
	if err == nil || !strings.Contains(err.Error(), "schema declares no columns") {
		t.Fatalf("expected the base-child no-columns configuration error, got %v", err)
	}
}

func TestHydrateBaseChildren_BranchErrors(t *testing.T) {
	// endereco JOIN row: dest[0]=role pk (leading), dest[1]=child ID, dest[2]=street.
	baseChildRow := func(vals []string) func(string, []any) (Rows, error) {
		return func(string, []any) (Rows, error) {
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				for j, d := range dest {
					if p, ok := d.(*string); ok && j < len(vals) {
						*p = vals[j]
					}
				}
				return nil
			}}, nil
		}
	}
	cases := []struct {
		name string
		eng  RelationalEngine
	}{
		{"queryError", fakeEngine(func(string, []any) (Rows, error) { return nil, errFakeDB })},
		{"scanError", fakeEngine(func(string, []any) (Rows, error) {
			return &fakeDBRows{rows: 1, scan: func(int, []any) error { return errFakeDB }}, nil
		})},
		{"roleIDDecodeError", decodeErrFakeEngine(baseChildRow([]string{"bad-root", "c1", "S"}), "bad-root")},
		{"childPKDecodeError", decodeErrFakeEngine(baseChildRow([]string{"a1", "bad-pk", "S"}), "bad-pk")},
		{"rowsErr", fakeEngine(func(string, []any) (Rows, error) { return &fakeDBRows{nextErr: errFakeDB}, nil })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewAggregateLoader[*roleAggLoad](tc.eng, func() *roleAggLoad { return &roleAggLoad{} }).
				WithSchema(roleAggLoadSchema())
			e := &roleAggLoad{Matricula: "M1"}
			e.SetID(domain.NewID("a1"))
			if err := l.hydrateBaseChildren(context.Background(), []*roleAggLoad{e}, []string{"a1"}, criteria.ScopeActive); !errors.Is(err, errFakeDB) {
				t.Fatalf("%s: expected errFakeDB, got %v", tc.name, err)
			}
		})
	}
}
