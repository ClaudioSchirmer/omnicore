package read

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// Exists is the hydration-free primitive for write-path uniqueness pre-checks.
// It must compile to a bare probe over the SAME resolution + scope-gate
// semantics as FindOne/FindAll — and the ID must be addressable as the fixed
// Go field "ID" (the exclude-self shape). The scalar facts live in the
// aggregate DSL (aggregate.go / aggregate_test.go).

type ecCodedEntity struct {
	domain.BaseEntity
	Code string
}

func (e *ecCodedEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *ecCodedEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func ecCodedLoader(queryFn func(sql string, args []any) (Rows, error)) *AggregateLoader[*ecCodedEntity] {
	return NewAggregateLoader[*ecCodedEntity](fakeEngine(queryFn), func() *ecCodedEntity { return &ecCodedEntity{} }).
		WithSchema(NewTableSchema[*ecCodedEntity]("listings").
			ID("id").Field("Code", "announcement_code").DeletedAt("deleted_at"))
}

func ecSchema() *TableSchema {
	return NewTableSchema[*aggLoaderTestEntity]("listings").
		ID("id").
		DeletedAt("deleted_at")
}

func ecLoader(queryFn func(sql string, args []any) (Rows, error)) *AggregateLoader[*aggLoaderTestEntity] {
	return NewAggregateLoader[*aggLoaderTestEntity](fakeEngine(queryFn), newAggLoaderTestEntity).
		WithSchema(ecSchema())
}

func TestExists_CompilesBareProbe_TrueOnRow(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	l := ecCodedLoader(func(sql string, args []any) (Rows, error) {
		gotSQL, gotArgs = sql, args
		return &fakeDBRows{rows: 1}, nil
	})

	q := criteria.Where(criteria.And(
		criteria.Eq("Code", "AN-1"),
		criteria.Not(criteria.Eq("ID", "0198aaaa-0000-7000-8000-000000000001")),
	))

	taken, err := l.Exists(context.Background(), q)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !taken {
		t.Fatal("a returned row must report true")
	}
	if !strings.HasPrefix(gotSQL, "SELECT 1 FROM listings") || !strings.HasSuffix(gotSQL, "LIMIT 1") {
		t.Errorf("Exists must be a bare SELECT 1 … LIMIT 1 probe, got %q", gotSQL)
	}
	// A dialect may render identifiers unquoted, so the table and WHERE must be
	// separated — "listingsWHERE" lexes as one identifier.
	if !strings.Contains(gotSQL, "FROM listings WHERE ") {
		t.Errorf("the WHERE clause must be whitespace-separated from the FROM, got %q", gotSQL)
	}
	if !strings.Contains(gotSQL, "announcement_code = $1") {
		t.Errorf("predicate must resolve Go field → column, got %q", gotSQL)
	}
	if !strings.Contains(gotSQL, "NOT (id = $2)") && !strings.Contains(gotSQL, "id <> $2") {
		t.Errorf("the ID must be addressable as \"ID\" for exclude-self, got %q", gotSQL)
	}
	if !strings.Contains(gotSQL, "deleted_at IS NULL") {
		t.Errorf("the default scope gate (active-only) must apply, got %q", gotSQL)
	}
	if len(gotArgs) != 2 {
		t.Errorf("expected 2 args (code + excluded id), got %v", gotArgs)
	}
	if strings.Contains(gotSQL, "SELECT COUNT") || strings.Contains(gotSQL, "JOIN") {
		t.Errorf("no count, no join for a flat probe: %q", gotSQL)
	}
}

func TestExists_FalseOnNoRows(t *testing.T) {
	l := ecLoader(func(string, []any) (Rows, error) { return &fakeDBRows{rows: 0}, nil })
	taken, err := l.Exists(context.Background(), criteria.Where(nil))
	if err != nil || taken {
		t.Fatalf("no rows must report (false, nil), got (%v, %v)", taken, err)
	}
}

func TestExists_NeOperatorOnID(t *testing.T) {
	var gotSQL string
	l := ecLoader(func(sql string, _ []any) (Rows, error) {
		gotSQL = sql
		return &fakeDBRows{}, nil
	})
	if _, err := l.Exists(context.Background(),
		criteria.Where(criteria.Ne("ID", "0198bbbb-0000-7000-8000-000000000002"))); err != nil {
		t.Fatalf("Ne on the ID field must be addressable, got %v", err)
	}
	if !strings.Contains(gotSQL, "id <> $1") {
		t.Errorf("Ne(\"ID\", …) must render against the ID column, got %q", gotSQL)
	}
}

func TestExists_NilQueryMeansAnyActive(t *testing.T) {
	var gotSQL string
	l := ecLoader(func(sql string, _ []any) (Rows, error) {
		gotSQL = sql
		return &fakeDBRows{}, nil
	})
	if _, err := l.Exists(context.Background(), nil); err != nil {
		t.Fatalf("nil query must mean 'any active row', got %v", err)
	}
	if !strings.Contains(gotSQL, "deleted_at IS NULL") {
		t.Errorf("nil query still gates on active, got %q", gotSQL)
	}
}

func TestExists_UnknownFieldErrors(t *testing.T) {
	l := ecLoader(nil)
	if _, err := l.Exists(context.Background(), criteria.Where(criteria.Eq("Nope", 1))); err == nil {
		t.Fatal("an unresolvable criteria field must error, not silently match nothing")
	}
}
