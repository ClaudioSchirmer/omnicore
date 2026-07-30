package read

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Branch coverage for LoadSharedBaseIdentity + loadBaseChildrenConstructor:
// the cold/skip fast paths and each probe/scan/decode error surface, all over
// the scriptable read seam.

// baseRowThenNone scripts the identity probe hit (pessoa) and leaves the
// other probes empty.
func baseRowThenNone(onSQL func(sql string)) func(string, []any) (Rows, error) {
	return func(sql string, _ []any) (Rows, error) {
		if onSQL != nil {
			onSQL(sql)
		}
		if strings.Contains(sql, "FROM pessoa") {
			return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
				if p, ok := dest[0].(*string); ok {
					*p = "p1"
				}
				if len(dest) > 1 {
					if p, ok := dest[1].(*string); ok {
						*p = "Ana"
					}
				}
				return nil
			}}, nil
		}
		return &fakeDBRows{}, nil
	}
}

func newRoleLoader(queryFn func(string, []any) (Rows, error), schema *TableSchema) *AggregateLoader[*roleAggLoad] {
	return NewAggregateLoader[*roleAggLoad](fakeEngine(queryFn), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(schema)
}

func TestLoadSharedBaseIdentity_FastPaths(t *testing.T) {
	t.Run("noSharedBaseIsNoop", func(t *testing.T) {
		l := newCovAggLoader(fakeEngine(nil), covAggSchema)
		fresh := &covAgg{Name: "x"}
		got, existed, err := l.LoadSharedBaseIdentity(context.Background(), fresh)
		if err != nil || existed || got != fresh {
			t.Fatalf("no shared base: %v %v %v", got, existed, err)
		}
	})
	t.Run("emptyNaturalKeyIsCold", func(t *testing.T) {
		l := newRoleLoader(baseRowThenNone(nil), roleAggLoadSchemaSD())
		fresh := &roleAggLoad{Name: "", Matricula: "M1"} // empty natural key
		got, existed, err := l.LoadSharedBaseIdentity(context.Background(), fresh)
		if err != nil || existed || got != fresh {
			t.Fatalf("empty natural key: %v %v %v", got, existed, err)
		}
	})
}

func TestLoadSharedBaseIdentity_ErrorSurfaces(t *testing.T) {
	fresh := func() *roleAggLoad { return &roleAggLoad{Name: "Ana", Matricula: "M1"} }

	t.Run("identityProbeQueryError", func(t *testing.T) {
		query := func(sql string, _ []any) (Rows, error) {
			if strings.Contains(sql, "FROM pessoa") {
				return nil, errFakeDB
			}
			return &fakeDBRows{}, nil
		}
		l := newRoleLoader(query, roleAggLoadSchemaSD())
		if _, _, err := l.LoadSharedBaseIdentity(context.Background(), fresh()); err == nil {
			t.Fatal("expected the probe error")
		}
	})
	t.Run("baseRowScanError", func(t *testing.T) {
		query := func(sql string, _ []any) (Rows, error) {
			if strings.Contains(sql, "FROM pessoa") {
				return &fakeDBRows{rows: 1, scan: func(int, []any) error { return errFakeDB }}, nil
			}
			return &fakeDBRows{}, nil
		}
		l := newRoleLoader(query, roleAggLoadSchemaSD())
		if _, _, err := l.LoadSharedBaseIdentity(context.Background(), fresh()); err == nil {
			t.Fatal("expected the scan error")
		}
	})
	t.Run("baseIDDecodeError", func(t *testing.T) {
		eng := decodeErrFakeEngine(baseRowThenNone(nil), "") // every DecodeID fails
		l := NewAggregateLoader[*roleAggLoad](eng, func() *roleAggLoad { return &roleAggLoad{} }).
			WithSchema(roleAggLoadSchemaSD())
		if _, _, err := l.LoadSharedBaseIdentity(context.Background(), fresh()); err == nil {
			t.Fatal("expected the decode error")
		}
	})
	t.Run("activeRoleProbeError", func(t *testing.T) {
		query := func(sql string, _ []any) (Rows, error) {
			switch {
			case strings.Contains(sql, "FROM pessoa"):
				return baseRowThenNone(nil)(sql, nil)
			case strings.Contains(sql, "FROM aluno"):
				return nil, errFakeDB
			}
			return &fakeDBRows{}, nil
		}
		l := newRoleLoader(query, roleAggLoadSchemaSD())
		if _, _, err := l.LoadSharedBaseIdentity(context.Background(), fresh()); err == nil {
			t.Fatal("expected the role probe error")
		}
	})
	t.Run("baseChildQueryError", func(t *testing.T) {
		query := func(sql string, _ []any) (Rows, error) {
			switch {
			case strings.Contains(sql, "FROM pessoa"):
				return baseRowThenNone(nil)(sql, nil)
			case strings.Contains(sql, "FROM endereco"):
				return nil, errFakeDB
			}
			return &fakeDBRows{}, nil
		}
		l := newRoleLoader(query, roleAggLoadSchemaSD())
		if _, _, err := l.LoadSharedBaseIdentity(context.Background(), fresh()); err == nil {
			t.Fatal("expected the base-child query error")
		}
	})
	t.Run("baseChildScanError", func(t *testing.T) {
		query := func(sql string, _ []any) (Rows, error) {
			switch {
			case strings.Contains(sql, "FROM pessoa"):
				return baseRowThenNone(nil)(sql, nil)
			case strings.Contains(sql, "FROM endereco"):
				return &fakeDBRows{rows: 1, scan: func(int, []any) error { return errFakeDB }}, nil
			}
			return &fakeDBRows{}, nil
		}
		l := newRoleLoader(query, roleAggLoadSchemaSD())
		if _, _, err := l.LoadSharedBaseIdentity(context.Background(), fresh()); err == nil {
			t.Fatal("expected the base-child scan error")
		}
	})
	t.Run("baseChildRowsErr", func(t *testing.T) {
		query := func(sql string, _ []any) (Rows, error) {
			switch {
			case strings.Contains(sql, "FROM pessoa"):
				return baseRowThenNone(nil)(sql, nil)
			case strings.Contains(sql, "FROM endereco"):
				return &fakeDBRows{nextErr: errFakeDB}, nil
			}
			return &fakeDBRows{}, nil
		}
		l := newRoleLoader(query, roleAggLoadSchemaSD())
		if _, _, err := l.LoadSharedBaseIdentity(context.Background(), fresh()); err == nil {
			t.Fatal("expected the base-child cursor error")
		}
	})
}

func TestLoadBaseChildrenConstructor_SkipPaths(t *testing.T) {
	t.Run("baseWithoutChildren", func(t *testing.T) {
		base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name")
		schema := NewTableSchema[*roleAggLoad]("aluno").
			ID("id").Revision("revision").Field("Matricula", "matricula").DeletedAt("deleted_at").
			SharedBase(base, "pessoa_id")
		l := newRoleLoader(baseRowThenNone(nil), schema)
		got, existed, err := l.LoadSharedBaseIdentity(context.Background(), &roleAggLoad{Name: "Ana"})
		if err != nil || !existed || got == nil {
			t.Fatalf("existing identity without base children: %v %v %v", got, existed, err)
		}
	})
	t.Run("baseChildWithoutColumnsSkips", func(t *testing.T) {
		base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name").
			Child(noColsChildSchema("pessoa_id"))
		schema := NewTableSchema[*noColsRole]("aluno").
			ID("id").Field("Matricula", "matricula").DeletedAt("deleted_at").
			SharedBase(base, "pessoa_id")
		l := NewAggregateLoader[*noColsRole](fakeEngine(baseRowThenNone(nil)), func() *noColsRole { return &noColsRole{} }).
			WithSchema(schema)
		got, existed, err := l.LoadSharedBaseIdentity(context.Background(), &noColsRole{Name: "Ana"})
		if err != nil || !existed || got == nil {
			t.Fatalf("column-less base child must be skipped: %v %v %v", got, existed, err)
		}
	})
}

// noColsRole is roleAggLoad with the column-less child in its aggregate
// boundary — the ScanPlan-empty skip path.
type noColsRole struct {
	domain.AggregateRoot
	Name      string
	Matricula string
}

func (e *noColsRole) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate}
}
func (e *noColsRole) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *noColsRole) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *noColsRole) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{noColsChild{}}
}
