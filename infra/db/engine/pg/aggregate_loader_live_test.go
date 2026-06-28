package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/jackc/pgx/v5"
)

// These tests drive the live SELECT/scan path of aggregate_loader.go through the
// querier() seam: l.pg.querier().Query(...) resolves to fakePool.queryHandler,
// so findRoots and hydrateChildren run end-to-end against an in-process result
// set. The scan funcs populate the destination pointers the loader assembles
// from the core.TableSchema (id/fk leading key + mapped columns), mirroring the auto
// scanPlan order.

const liveRootID = "00000000-0000-0000-0000-0000000000a1"

// loaderPostgres wires a Postgres whose pool routes every Query through qh.
func loaderPostgres(qh func(sql string, args []any) (pgx.Rows, error)) *Postgres {
	p := newFakePool()
	p.queryHandler = qh
	return newFakePostgres(p)
}

// setStrings assigns vals positionally to the *string destinations in dest.
func setStrings(dest []any, vals ...string) {
	for i, d := range dest {
		if p, ok := d.(*string); ok && i < len(vals) {
			*p = vals[i]
		}
	}
}

// --- findRoots: auto-scan, flat entity (no children) -----------------------

func TestFindOne_NotFound(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 0}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema)

	_, err := l.FindOne(context.Background(), criteria.Where(nil))
	if err == nil {
		t.Fatal("expected NotFound error from FindOne on zero rows")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected a NotificationCarrier (404), got %T: %v", err, err)
	}
}

func TestFindOne_SingleRoot(t *testing.T) {
	pg := loaderPostgres(func(sql string, _ []any) (pgx.Rows, error) {
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "Jane", "jane@example.com")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema)

	got, err := l.FindOne(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if id := got.GetID(); id == nil || id.Value() != liveRootID {
		t.Errorf("root id = %v, want %q", got.GetID(), liveRootID)
	}
	if got.Name != "Jane" || got.Email != "jane@example.com" {
		t.Errorf("scanned fields = %q/%q, want Jane/jane@example.com", got.Name, got.Email)
	}
}

func TestFindOne_MultipleRoots_Error(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 2, scan: func(idx int, dest []any) error {
			setStrings(dest, fmt.Sprintf("id-%d", idx), "n", "e")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema)

	_, err := l.FindOne(context.Background(), criteria.Where(nil))
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("expected >1 row error, got %v", err)
	}
}

func TestFindAll_Empty(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 0}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema)

	got, err := l.FindAll(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d", len(got))
	}
}

// --- findRoots error branches ----------------------------------------------

func TestFindRoots_QueryError(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return nil, errFake
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFake) {
		t.Fatalf("expected query error, got %v", err)
	}
}

func TestFindRoots_ScanError(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 1, scan: func(int, []any) error { return errFake }}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFake) {
		t.Fatalf("expected scan error, got %v", err)
	}
}

func TestFindRoots_RowsErr(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 1, nextErr: errFake, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "n", "e")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFake) {
		t.Fatalf("expected rows.Err propagation, got %v", err)
	}
}

func TestFindRoots_NoColumnsSchema(t *testing.T) {
	// A schema with only a PK declares no scan columns and no manual scanner:
	// findRoots returns the "schema declares no columns" config error.
	bare := core.NewTableSchema[*builderTestEntity]("bare").PK("id")
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(bare)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); err == nil ||
		!strings.Contains(err.Error(), "no columns") {
		t.Fatalf("expected no-columns config error, got %v", err)
	}
}

// --- hydrateChildren: aggregate root + batched children --------------------

func TestFindOne_AggregateWithChildren(t *testing.T) {
	pg := loaderPostgres(func(sql string, _ []any) (pgx.Rows, error) {
		if strings.Contains(sql, "cov_children") {
			return &fakeRows{rows: 2, scan: func(idx int, dest []any) error {
				// dest = [fk, id, label]
				setStrings(dest, liveRootID, fmt.Sprintf("c%d", idx), fmt.Sprintf("label-%d", idx))
				return nil
			}}, nil
		}
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "agg")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*covAgg](pg, func() *covAgg { return &covAgg{} }).
		WithSchema(covAggSchema)

	got, err := l.FindOne(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	kids := domain.GetCurrentItemsOf[covChild](got.GetAggregateRoot())
	if len(kids) != 2 {
		t.Fatalf("expected 2 hydrated children, got %d", len(kids))
	}
}

func TestFindAll_MultipleRootsAndChildren(t *testing.T) {
	roots := []string{
		"00000000-0000-0000-0000-0000000000b1",
		"00000000-0000-0000-0000-0000000000b2",
	}
	pg := loaderPostgres(func(sql string, args []any) (pgx.Rows, error) {
		if strings.Contains(sql, "cov_children") {
			// One batched query WHERE fk IN ($1,$2) — 2 roots → 2 placeholders.
			if len(args) != 2 {
				t.Errorf("child batch expected 2 args, got %d", len(args))
			}
			// row0,row1 → root0 ; row2 → root1
			fks := []string{roots[0], roots[0], roots[1]}
			return &fakeRows{rows: 3, scan: func(idx int, dest []any) error {
				setStrings(dest, fks[idx], fmt.Sprintf("c%d", idx), "lbl")
				return nil
			}}, nil
		}
		return &fakeRows{rows: 2, scan: func(idx int, dest []any) error {
			setStrings(dest, roots[idx], "agg")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*covAgg](pg, func() *covAgg { return &covAgg{} }).
		WithSchema(covAggSchema)

	got, err := l.FindAll(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 roots, got %d", len(got))
	}
	counts := map[string]int{}
	for _, r := range got {
		counts[r.GetID().Value()] = len(domain.GetCurrentItemsOf[covChild](r.GetAggregateRoot()))
	}
	if counts[roots[0]] != 2 || counts[roots[1]] != 1 {
		t.Errorf("child grouping = %v, want root0:2 root1:1", counts)
	}
}

func TestHydrateChildren_QueryError(t *testing.T) {
	pg := loaderPostgres(func(sql string, _ []any) (pgx.Rows, error) {
		if strings.Contains(sql, "cov_children") {
			return nil, errFake
		}
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "agg")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*covAgg](pg, func() *covAgg { return &covAgg{} }).
		WithSchema(covAggSchema)

	if _, err := l.FindOne(context.Background(), criteria.Where(nil)); !errors.Is(err, errFake) {
		t.Fatalf("expected child query error, got %v", err)
	}
}

func TestHydrateChildren_ScanError(t *testing.T) {
	pg := loaderPostgres(func(sql string, _ []any) (pgx.Rows, error) {
		if strings.Contains(sql, "cov_children") {
			return &fakeRows{rows: 1, scan: func(int, []any) error { return errFake }}, nil
		}
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "agg")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*covAgg](pg, func() *covAgg { return &covAgg{} }).
		WithSchema(covAggSchema)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFake) {
		t.Fatalf("expected child scan error, got %v", err)
	}
}

// --- manual root scanner path ----------------------------------------------

func TestFindOne_ManualRootScanner(t *testing.T) {
	pg := loaderPostgres(func(sql string, _ []any) (pgx.Rows, error) {
		// Manual scanner uses SELECT * — assert the loader took that branch.
		if !strings.Contains(sql, "SELECT *") {
			t.Errorf("manual scanner expected SELECT *, got %q", sql)
		}
		return &fakeRows{rows: 1}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema).
		WithRootScanner(func(core.Row) (*builderTestEntity, error) {
			e := &builderTestEntity{Name: "manual"}
			e.SetID(domain.NewID(liveRootID))
			return e, nil
		})

	got, err := l.FindOne(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindOne (manual): %v", err)
	}
	if got.GetID().Value() != liveRootID || got.Name != "manual" {
		t.Errorf("manual scan result = %q/%q", got.GetID().Value(), got.Name)
	}
}

func TestFindOne_ManualRootScanner_EmptyID(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 1}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema).
		WithRootScanner(func(core.Row) (*builderTestEntity, error) {
			return &builderTestEntity{Name: "noid"}, nil // never calls SetID
		})

	if _, err := l.FindOne(context.Background(), criteria.Where(nil)); err == nil ||
		!strings.Contains(err.Error(), "must populate the id") {
		t.Fatalf("expected empty-id config error, got %v", err)
	}
}

func TestFindRoots_ManualScannerError(t *testing.T) {
	pg := loaderPostgres(func(string, []any) (pgx.Rows, error) {
		return &fakeRows{rows: 1}, nil
	})
	l := read.NewAggregateLoader[*builderTestEntity](pg, func() *builderTestEntity { return &builderTestEntity{} }).
		WithSchema(builderTestSchema).
		WithRootScanner(func(core.Row) (*builderTestEntity, error) { return nil, errFake })

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); !errors.Is(err, errFake) {
		t.Fatalf("expected manual scanner error, got %v", err)
	}
}

// --- manual child scanner path ---------------------------------------------

func TestHydrateChildren_ManualChildScanner(t *testing.T) {
	pg := loaderPostgres(func(sql string, args []any) (pgx.Rows, error) {
		if strings.Contains(sql, "cov_children") {
			// Manual child scanner runs one SELECT * per root id ($1).
			if !strings.Contains(sql, "SELECT *") {
				t.Errorf("manual child scanner expected SELECT *, got %q", sql)
			}
			return &fakeRows{rows: 1}, nil
		}
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "agg")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*covAgg](pg, func() *covAgg { return &covAgg{} }).
		WithSchema(covAggSchema).
		WithChildScanner("covChild", func(core.Rows) (domain.AggregateValueObject, error) {
			return covChild{ID: "m1", Label: "manual"}, nil
		})

	got, err := l.FindOne(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindOne (manual child): %v", err)
	}
	kids := domain.GetCurrentItemsOf[covChild](got.GetAggregateRoot())
	if len(kids) != 1 || kids[0].Label != "manual" {
		t.Fatalf("expected 1 manual child, got %v", kids)
	}
}

func TestHydrateChildren_ManualChildScannerError(t *testing.T) {
	pg := loaderPostgres(func(sql string, _ []any) (pgx.Rows, error) {
		if strings.Contains(sql, "cov_children") {
			return &fakeRows{rows: 1}, nil
		}
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			setStrings(dest, liveRootID, "agg")
			return nil
		}}, nil
	})
	l := read.NewAggregateLoader[*covAgg](pg, func() *covAgg { return &covAgg{} }).
		WithSchema(covAggSchema).
		WithChildScanner("covChild", func(core.Rows) (domain.AggregateValueObject, error) {
			return nil, errFake
		})

	if _, err := l.FindOne(context.Background(), criteria.Where(nil)); !errors.Is(err, errFake) {
		t.Fatalf("expected manual child scanner error, got %v", err)
	}
}
