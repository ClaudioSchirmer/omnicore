package read

import (
	"context"
	"database/sql"
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

func TestHydrateChildren_ChildSchemaWithoutColumnsErrors(t *testing.T) {
	schema := NewTableSchema[*covAgg]("cov_aggs").ID("id").Revision("revision").Field("Name", "name").
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
	// The child's own id + managed columns are trailing sql.Null* targets now (the
	// id left the struct when it moved into domain.Managed), so the fake row fills
	// *string (leading key + business columns) AND *sql.NullString (the trailing id)
	// by position.
	childRow := func(vals []string) func(string, []any) (Rows, error) {
		return func(string, []any) (Rows, error) {
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				for j, d := range dest {
					if j >= len(vals) {
						continue
					}
					switch p := d.(type) {
					case *string:
						*p = vals[j]
					case *sql.NullString:
						*p = sql.NullString{String: vals[j], Valid: vals[j] != ""}
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
		// ParentID is the leading key → its DecodeID failure is the fk-decode branch.
		// Row order: fk (leading), Label (business col), id (trailing carrier col).
		{"fkDecodeError", decodeErrFakeEngine(childRow([]string{"bad-fk", "L", "c1"}), "bad-fk")},
		// The child's own id is a trailing column decoded by managedScan.apply.
		{"childPKDecodeError", decodeErrFakeEngine(childRow([]string{"r1", "L", "bad-pk"}), "bad-pk")},
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
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
		Child(NewTableSchema[addrLoad]("endereco").ID("id").ParentID("pessoa_id").Field("Street", "street"))
	return NewTableSchema[*roleLoadEntity]("aluno").
		ID("id").
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
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
		Child(noColsChildSchema("pessoa_id"))
	schema := NewTableSchema[*roleAggLoad]("aluno").
		ID("id").Revision("revision").
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
	// endereco JOIN row order: dest[0]=role pk (leading), dest[1]=street (business),
	// dest[2]=child id (trailing *sql.NullString carrier column).
	baseChildRow := func(vals []string) func(string, []any) (Rows, error) {
		return func(string, []any) (Rows, error) {
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				for j, d := range dest {
					if j >= len(vals) {
						continue
					}
					switch p := d.(type) {
					case *string:
						*p = vals[j]
					case *sql.NullString:
						*p = sql.NullString{String: vals[j], Valid: vals[j] != ""}
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
		{"roleIDDecodeError", decodeErrFakeEngine(baseChildRow([]string{"bad-root", "S", "c1"}), "bad-root")},
		{"childPKDecodeError", decodeErrFakeEngine(baseChildRow([]string{"a1", "S", "bad-pk"}), "bad-pk")},
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
