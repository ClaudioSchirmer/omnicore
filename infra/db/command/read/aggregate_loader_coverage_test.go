package read

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// White-box coverage for AggregateLoader.FindOne / FindAll / findRoots — the
// criteria entry points — over the scriptable neutral read seam. The fake
// querier dispatches per SQL shape: root SELECT vs child batched SELECT vs
// sibling SELECT vs shared-base JOIN.

// covAggQuery scripts the two covAgg SELECT shapes: the root SELECT
// (FROM cov_aggs) and the batched child SELECT (FROM cov_children).
func covAggQuery(rootRows, childRows func() Rows, capture *string) func(string, []any) (Rows, error) {
	return func(sql string, _ []any) (Rows, error) {
		switch {
		case strings.Contains(sql, "FROM cov_children"):
			return childRows(), nil
		case strings.Contains(sql, "FROM cov_aggs"):
			if capture != nil {
				*capture = sql
			}
			return rootRows(), nil
		}
		return &fakeDBRows{}, nil
	}
}

func covAggRootRow(id, name string) func() Rows {
	return func() Rows {
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			if p, ok := dest[0].(*string); ok {
				*p = id
			}
			if p, ok := dest[1].(*string); ok {
				*p = name
			}
			return nil
		}}
	}
}

func covChildRow(fk, id, label string) func() Rows {
	return func() Rows {
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			vals := []string{fk, id, label}
			for j, d := range dest {
				if p, ok := d.(*string); ok && j < len(vals) {
					*p = vals[j]
				}
			}
			return nil
		}}
	}
}

func noRows() Rows { return &fakeDBRows{} }

// covAggMaps scripts the manual-scanner SELECT shapes (QueryMaps path): the root
// SELECT (FROM cov_aggs) and the manual child SELECT (FROM cov_children).
func covAggMaps(rootMaps, childMaps func() []map[string]any, capture *string) func(string, []any) ([]map[string]any, error) {
	return func(sql string, _ []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM cov_children"):
			return childMaps(), nil
		case strings.Contains(sql, "FROM cov_aggs"):
			if capture != nil {
				*capture = sql
			}
			return rootMaps(), nil
		}
		return nil, nil
	}
}

func covAggRootMap(id, name string) func() []map[string]any {
	return func() []map[string]any { return []map[string]any{{"id": id, "name": name}} }
}

func noMaps() []map[string]any { return nil }

func TestFindOne_FoundHydratesRootAndChildren(t *testing.T) {
	var rootSQL string
	l := newCovAggLoader(fakeEngine(covAggQuery(covAggRootRow("r1", "Ana"), covChildRow("r1", "c1", "L1"), &rootSQL)), covAggSchema)

	e, err := l.FindOne(context.Background(), criteria.ByID(domain.NewID("r1")))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got := e.GetID().Value(); got != "r1" {
		t.Errorf("root id = %q, want %q", got, "r1")
	}
	if e.Name != "Ana" {
		t.Errorf("root field not scanned: Name=%q, want %q", e.Name, "Ana")
	}
	// The >1 probe bounds the root SELECT with LIMIT 2, overriding any Query limit.
	if !strings.Contains(rootSQL, "LIMIT 2") {
		t.Errorf("FindOne must probe with LIMIT 2, got %q", rootSQL)
	}
	items := domain.GetCurrentItemsOf[covChild](&e.AggregateRoot)
	if len(items) != 1 || items[0].GetID().Value() != "c1" || items[0].Label != "L1" {
		t.Errorf("children not hydrated: %+v", items)
	}
}

func TestFindOne_NotFoundIsRecordNotFound(t *testing.T) {
	l := newCovAggLoader(fakeEngine(covAggQuery(noRows, noRows, nil)), covAggSchema)

	_, err := l.FindOne(context.Background(), criteria.ByID(domain.NewID("missing")))
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("not-found must be a NotificationCarrier, got %T (%v)", err, err)
	}
	if !carrierHasNotification(carrier, "RecordNotFoundNotification") {
		t.Errorf("not-found must carry RecordNotFoundNotification, got %v", carrier.NotificationContexts())
	}
}

func TestFindOne_MoreThanOneRowErrors(t *testing.T) {
	twoRoots := func() Rows {
		return &fakeDBRows{rows: 2, scan: func(i int, dest []any) error {
			if p, ok := dest[0].(*string); ok {
				*p = []string{"r1", "r2"}[i]
			}
			return nil
		}}
	}
	l := newCovAggLoader(fakeEngine(covAggQuery(twoRoots, noRows, nil)), covAggSchema)

	_, err := l.FindOne(context.Background(), criteria.Where(criteria.Eq("Name", "dup")))
	if err == nil || !strings.Contains(err.Error(), "matched more than one row") {
		t.Fatalf("expected the >1 developer-mistake error, got %v", err)
	}
}

func TestFindOne_RootQueryErrorPropagates(t *testing.T) {
	l := newCovAggLoader(fakeEngine(func(string, []any) (Rows, error) { return nil, errFakeDB }), covAggSchema)
	if _, err := l.FindOne(context.Background(), criteria.ByID(domain.NewID("r1"))); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected root query error, got %v", err)
	}
}

func TestFindAll_HydratesBatchedChildrenAcrossRoots(t *testing.T) {
	var childSQL string
	var childArgs []any
	query := func(sql string, args []any) (Rows, error) {
		switch {
		case strings.Contains(sql, "FROM cov_children"):
			childSQL, childArgs = sql, args
			return &fakeDBRows{rows: 2, scan: func(i int, dest []any) error {
				vals := [][]string{{"r1", "c1", "L1"}, {"r2", "c2", "L2"}}[i]
				for j, d := range dest {
					if p, ok := d.(*string); ok && j < len(vals) {
						*p = vals[j]
					}
				}
				return nil
			}}, nil
		case strings.Contains(sql, "FROM cov_aggs"):
			return &fakeDBRows{rows: 2, scan: func(i int, dest []any) error {
				if p, ok := dest[0].(*string); ok {
					*p = []string{"r1", "r2"}[i]
				}
				if p, ok := dest[1].(*string); ok {
					*p = []string{"Ana", "Bob"}[i]
				}
				return nil
			}}, nil
		}
		return &fakeDBRows{}, nil
	}
	l := newCovAggLoader(fakeEngine(query), covAggSchema)

	all, err := l.FindAll(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("roots = %d, want 2", len(all))
	}
	// One batched SELECT for both roots: WHERE fk IN ($1, $2).
	if !strings.Contains(childSQL, "IN ($1, $2)") || len(childArgs) != 2 {
		t.Errorf("children must load in ONE batched SELECT, got %q args=%v", childSQL, childArgs)
	}
	for i, want := range []struct{ id, label string }{{"c1", "L1"}, {"c2", "L2"}} {
		items := domain.GetCurrentItemsOf[covChild](&all[i].AggregateRoot)
		if len(items) != 1 || items[0].GetID().Value() != want.id || items[0].Label != want.label {
			t.Errorf("root %d children grouped wrong: %+v", i, items)
		}
	}
}

func TestFindAll_NoMatchReturnsEmptySlice(t *testing.T) {
	l := newCovAggLoader(fakeEngine(covAggQuery(noRows, noRows, nil)), covAggSchema)
	all, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("Name", "none")))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if all == nil || len(all) != 0 {
		t.Errorf("no match must be an empty (non-nil) slice, got %#v", all)
	}
}

func TestFindAll_RootQueryErrorPropagates(t *testing.T) {
	l := newCovAggLoader(fakeEngine(func(string, []any) (Rows, error) { return nil, errFakeDB }), covAggSchema)
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected root query error, got %v", err)
	}
}

// ─── FindOne/FindAll hydrate-step error propagation ─────────────────────────
//
// Each hydrate step (siblings, shared base, children, base-children) surfaces
// its error through both entry points. Every case scripts the ROOT SELECT to
// return one row and fails the step-specific SELECT shape.

func TestFindOneAndFindAll_SiblingHydrateErrorPropagates(t *testing.T) {
	query := func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "FROM usuario") { // sibling SELECT
			return nil, errFakeDB
		}
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			if p, ok := dest[0].(*string); ok {
				*p = "r1"
			}
			return nil
		}}, nil
	}
	l := NewAggregateLoader[*sibLoadEntity](fakeEngine(query), func() *sibLoadEntity { return &sibLoadEntity{} }).
		WithSchema(sibLoadSchema())

	if _, err := l.FindOne(context.Background(), criteria.Where(criteria.Eq("Name", "x"))); !errors.Is(err, errFakeDB) {
		t.Errorf("FindOne must surface the sibling hydrate error, got %v", err)
	}
	if _, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("Name", "x"))); !errors.Is(err, errFakeDB) {
		t.Errorf("FindAll must surface the sibling hydrate error, got %v", err)
	}
}

func TestFindOneAndFindAll_SharedBaseHydrateErrorPropagates(t *testing.T) {
	query := func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "JOIN") { // role→base JOIN SELECT
			return nil, errFakeDB
		}
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			if p, ok := dest[0].(*string); ok {
				*p = "a1"
			}
			return nil
		}}, nil
	}
	l := NewAggregateLoader[*roleLoadEntity](fakeEngine(query), func() *roleLoadEntity { return &roleLoadEntity{} }).
		WithSchema(roleLoadSchema())

	if _, err := l.FindOne(context.Background(), criteria.Where(criteria.Eq("Matricula", "M1"))); !errors.Is(err, errFakeDB) {
		t.Errorf("FindOne must surface the shared-base hydrate error, got %v", err)
	}
	if _, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("Matricula", "M1"))); !errors.Is(err, errFakeDB) {
		t.Errorf("FindAll must surface the shared-base hydrate error, got %v", err)
	}
}

func TestFindOneAndFindAll_ChildrenHydrateErrorPropagates(t *testing.T) {
	query := func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "FROM cov_children") {
			return nil, errFakeDB
		}
		return covAggRootRow("r1", "Ana")(), nil
	}
	l := newCovAggLoader(fakeEngine(query), covAggSchema)

	if _, err := l.FindOne(context.Background(), criteria.ByID(domain.NewID("r1"))); !errors.Is(err, errFakeDB) {
		t.Errorf("FindOne must surface the children hydrate error, got %v", err)
	}
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Errorf("FindAll must surface the children hydrate error, got %v", err)
	}
}

func TestFindOneAndFindAll_BaseChildrenHydrateErrorPropagates(t *testing.T) {
	query := func(sql string, _ []any) (Rows, error) {
		switch {
		case strings.Contains(sql, "endereco"): // base-child JOIN SELECT
			return nil, errFakeDB
		case strings.Contains(sql, "JOIN"): // shared-base hydrate — absent base row is fine
			return &fakeDBRows{}, nil
		}
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			if p, ok := dest[0].(*string); ok {
				*p = "a1"
			}
			return nil
		}}, nil
	}
	l := NewAggregateLoader[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} }).
		WithSchema(roleAggLoadSchema())

	if _, err := l.FindOne(context.Background(), criteria.Where(criteria.Eq("Matricula", "M1"))); !errors.Is(err, errFakeDB) {
		t.Errorf("FindOne must surface the base-children hydrate error, got %v", err)
	}
	if _, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("Matricula", "M1"))); !errors.Is(err, errFakeDB) {
		t.Errorf("FindAll must surface the base-children hydrate error, got %v", err)
	}
}

// ─── findRoots: criteria compile + auto-scan edges ───────────────────────────

func TestFindRoots_UnknownWhereFieldErrors(t *testing.T) {
	l := newCovAggLoader(fakeEngine(nil), covAggSchema)
	if _, err := l.FindAll(context.Background(), criteria.Where(criteria.Eq("Nope", 1))); err == nil {
		t.Fatal("an unresolvable criteria field must fail fast")
	}
}

func TestFindRoots_UnknownOrderFieldErrors(t *testing.T) {
	l := newCovAggLoader(fakeEngine(nil), covAggSchema)
	if _, err := l.FindAll(context.Background(), criteria.Where(nil).OrderBy("Nope")); err == nil || !strings.Contains(err.Error(), "unknown order field") {
		t.Fatalf("an unresolvable order field must fail fast, got %v", err)
	}
}

func TestFindRoots_SchemaWithoutColumnsErrors(t *testing.T) {
	bare := NewTableSchema[*covAgg]("cov_aggs").ID("id").Revision("revision") // no Field(...) → empty scan plan
	l := newCovAggLoader(fakeEngine(nil), bare)
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); err == nil || !strings.Contains(err.Error(), "schema declares no columns") {
		t.Fatalf("expected the no-columns configuration error, got %v", err)
	}
}

func TestFindRoots_AutoScanRowScanErrorPropagates(t *testing.T) {
	failScan := func() Rows {
		return &fakeDBRows{rows: 1, scan: func(int, []any) error { return errFakeDB }}
	}
	l := newCovAggLoader(fakeEngine(covAggQuery(failScan, noRows, nil)), covAggSchema)
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected the row scan error, got %v", err)
	}
}

func TestFindRoots_AutoScanDecodeIDErrorPropagates(t *testing.T) {
	l := newCovAggLoader(decodeErrFakeEngine(covAggQuery(covAggRootRow("r1", "Ana"), noRows, nil), ""), covAggSchema)
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected the DecodeID error, got %v", err)
	}
}

func TestFindRoots_AutoScanRowsErrPropagates(t *testing.T) {
	erring := func() Rows { return &fakeDBRows{nextErr: errFakeDB} }
	l := newCovAggLoader(fakeEngine(covAggQuery(erring, noRows, nil)), covAggSchema)
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected rows.Err to propagate, got %v", err)
	}
}

// ─── findRoots: manual root scanner path ─────────────────────────────────────

func manualCovAggScanner(m map[string]any) (*covAgg, error) {
	id, _ := m["id"].(string)
	name, _ := m["name"].(string)
	e := &covAgg{Name: name}
	e.SetID(domain.NewID(id))
	return e, nil
}

func TestFindRoots_ManualRootScanner_ExplicitColumnsAndIDRecovered(t *testing.T) {
	var rootSQL string
	l := newCovAggLoader(
		fakeEngineWithMaps(covAggQuery(noRows, noRows, nil), covAggMaps(covAggRootMap("r1", "Ana"), noMaps, &rootSQL)),
		covAggSchema,
	).WithRootScanner(manualCovAggScanner)

	all, err := l.FindAll(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if !strings.Contains(rootSQL, "FROM cov_aggs") || strings.Contains(rootSQL, "SELECT *") {
		t.Errorf("a manual root scanner drives an explicit-column SELECT (never SELECT *), got %q", rootSQL)
	}
	if len(all) != 1 || all[0].GetID().Value() != "r1" || all[0].Name != "Ana" {
		t.Errorf("manual-scanned root wrong: %+v", all)
	}
}

func TestFindRoots_ManualRootScanner_QueryErrorPropagates(t *testing.T) {
	mapsErr := func(string, []any) ([]map[string]any, error) { return nil, errFakeDB }
	l := newCovAggLoader(fakeEngineWithMaps(nil, mapsErr), covAggSchema).
		WithRootScanner(manualCovAggScanner)
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected query error, got %v", err)
	}
}

func TestFindRoots_ManualRootScanner_ScannerErrorPropagates(t *testing.T) {
	l := newCovAggLoader(
		fakeEngineWithMaps(covAggQuery(noRows, noRows, nil), covAggMaps(covAggRootMap("r1", "Ana"), noMaps, nil)),
		covAggSchema,
	).WithRootScanner(func(map[string]any) (*covAgg, error) { return nil, errFakeDB })
	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFakeDB) {
		t.Fatalf("expected scanner error, got %v", err)
	}
}

func TestFindRoots_ManualRootScanner_MissingIDErrors(t *testing.T) {
	l := newCovAggLoader(
		fakeEngineWithMaps(covAggQuery(noRows, noRows, nil), covAggMaps(covAggRootMap("r1", "Ana"), noMaps, nil)),
		covAggSchema,
	).WithRootScanner(func(map[string]any) (*covAgg, error) { return &covAgg{Name: "no id set"}, nil })
	_, err := l.FindAll(context.Background(), criteria.Where(nil))
	if err == nil || !strings.Contains(err.Error(), "must populate the id") {
		t.Fatalf("a manual scanner that skips SetID is a loud configuration error, got %v", err)
	}
}
