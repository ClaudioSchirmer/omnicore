//go:build integration && postgres

package postgres

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// Integration proof for the loader's Exists probe and aggregate DSL against a
// REAL Postgres: what the unit fakes cannot certify — the concrete types pgx
// delivers for SUM/AVG/MIN/MAX (NUMERIC → pgtype.Numeric vs native),
// NULL-on-empty-set scanning, the scope gate over live rows, and Ne("ID")
// end to end.

type probeItem struct {
	domain.BaseEntity
	Code  string
	Cents int64
	Area  float64
}

func (e *probeItem) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (*probeItem) BuildRules(string, domain.Service, *domain.Rules) {}

func probeSchema() *core.TableSchema {
	return core.NewTableSchema[*probeItem]("probe_items").
		ID("id").
		Field("Code", "code").
		Field("Cents", "cents").
		Field("Area", "area").
		SoftDelete("deleted_at")
}

func probeSetup(t *testing.T) (*Postgres, string, func()) {
	t.Helper()
	pg, cleanup := newTestPG(t)
	createTable(t, pg, `CREATE TABLE probe_items (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		code TEXT NOT NULL,
		cents BIGINT NOT NULL,
		area DOUBLE PRECISION NOT NULL,
		deleted_at TIMESTAMP
	)`)
	ctx := context.Background()
	var idA string
	if err := pg.Pool().QueryRow(ctx,
		`INSERT INTO probe_items (code, cents, area) VALUES ('A', 1050, 10.5) RETURNING id`).Scan(&idA); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	// 10.50 + 1.20 + 5.00 in minor units — integer arithmetic end to end.
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO probe_items (code, cents, area) VALUES ('B', 120, 20.25), ('C', 500, 30.0)`); err != nil {
		t.Fatalf("seed B/C: %v", err)
	}
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO probe_items (code, cents, area, deleted_at) VALUES ('A', 999999, 99.9, NOW())`); err != nil {
		t.Fatalf("seed archived: %v", err)
	}
	return pg, idA, cleanup
}

func probeLoader(pg *Postgres) *read.AggregateLoader[*probeItem] {
	return read.NewAggregateLoader[*probeItem](pg, func() *probeItem { return &probeItem{} }).
		WithSchema(probeSchema())
}

func TestProbes_Postgres_ExistsScopeAndExcludeSelf(t *testing.T) {
	pg, idA, cleanup := probeSetup(t)
	defer cleanup()
	l := probeLoader(pg)
	ctx := context.Background()

	if ok, err := l.Exists(ctx, criteria.Where(criteria.Eq("Code", "A"))); err != nil || !ok {
		t.Fatalf("Exists(A) = (%v, %v), want true (active row)", ok, err)
	}
	if ok, err := l.Exists(ctx, criteria.Where(criteria.Eq("Code", "ZZ"))); err != nil || ok {
		t.Fatalf("Exists(ZZ) = (%v, %v), want false", ok, err)
	}
	// The archived 'A' twin must be invisible: excluding the active A row by id
	// leaves only the archived one → false proves the scope gate on live data.
	if ok, err := l.Exists(ctx, criteria.Where(criteria.And(
		criteria.Eq("Code", "A"), criteria.Ne("ID", idA)))); err != nil || ok {
		t.Fatalf("Exists(A, excluding the active row) = (%v, %v), want false — the archived twin must not count", ok, err)
	}
	// Excluding some other id keeps the active A visible → the exclude-self
	// uniqueness shape works end to end.
	if ok, err := l.Exists(ctx, criteria.Where(criteria.And(
		criteria.Eq("Code", "A"), criteria.Ne("ID", "00000000-0000-0000-0000-000000000000")))); err != nil || !ok {
		t.Fatalf("Exists(A, excluding an unrelated id) = (%v, %v), want true", ok, err)
	}
}

func TestProbes_Postgres_AggregateDSL(t *testing.T) {
	pg, _, cleanup := probeSetup(t)
	defer cleanup()
	l := probeLoader(pg)
	ctx := context.Background()

	// Every fact in ONE SELECT. SUM/AVG over BIGINT arrive as pg NUMERIC
	// (pgtype.Numeric through pgx): the minor-unit sum must come back exact.
	total := read.Count()
	cents := read.SumInt("Cents")
	area := read.Sum("Area")
	avgArea := read.Avg("Area")
	loCents, hiCents := read.MinInt("Cents"), read.MaxInt("Cents")
	loArea, hiArea := read.Min("Area"), read.Max("Area")
	if err := l.Aggregate(ctx, nil, total, cents, area, avgArea, loCents, hiCents, loArea, hiArea); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if total.Value != 3 {
		t.Fatalf("Count = %d, want 3 actives (archived excluded)", total.Value)
	}
	if cents.Value != 1670 || !cents.Found {
		t.Fatalf("SumInt(Cents) = (%d, %v), want (1670, true) — 10.50+1.20+5.00 in cents", cents.Value, cents.Found)
	}
	if area.Value != 60.75 {
		t.Fatalf("Sum(Area) = %v, want 60.75", area.Value)
	}
	if avgArea.Value != 20.25 || !avgArea.Found {
		t.Fatalf("Avg(Area) = (%v, %v), want (20.25, true)", avgArea.Value, avgArea.Found)
	}
	// The archived twin carries 999999 cents — the extremes prove the scope
	// gate rides the single aggregate SELECT too.
	if loCents.Value != 120 || hiCents.Value != 1050 {
		t.Fatalf("MinInt/MaxInt(Cents) = %d/%d, want 120/1050 (archived 999999 excluded)", loCents.Value, hiCents.Value)
	}
	if loArea.Value != 10.5 || hiArea.Value != 30.0 {
		t.Fatalf("Min/Max(Area) = %v/%v, want 10.5/30", loArea.Value, hiArea.Value)
	}

	// Empty set: Count 0; every field spec reports Found=false — SQL NULL
	// handled per spec.
	none := criteria.Where(criteria.Eq("Code", "ZZ"))
	if err := l.Aggregate(ctx, none, total, cents, avgArea, hiCents); err != nil {
		t.Fatalf("Aggregate(empty): %v", err)
	}
	if total.Value != 0 || cents.Value != 0 || cents.Found || avgArea.Found || hiCents.Found {
		t.Fatalf("empty set: Count=%d SumInt=(%d,%v) Avg.Found=%v MaxInt.Found=%v, want 0/(0,false)/false/false",
			total.Value, cents.Value, cents.Found, avgArea.Found, hiCents.Found)
	}

	// An exact-integer spec over a fractional column must error loudly, never
	// truncate.
	if err := l.Aggregate(ctx, nil, read.SumInt("Area")); err == nil {
		t.Fatal("SumInt over DOUBLE PRECISION must error loudly (use Sum)")
	}
}

func TestProbes_Postgres_AggregateBy(t *testing.T) {
	pg, _, cleanup := probeSetup(t)
	defer cleanup()
	l := probeLoader(pg)
	ctx := context.Background()

	// Per-group facts in ONE SELECT, groups ordered by key. The archived 'A'
	// twin (999999 cents) must not leak into the A group — the scope gate rides
	// the grouped SELECT too.
	total := read.Count()
	cents := read.SumInt("Cents")
	avgArea := read.Avg("Area")
	groups, err := l.AggregateBy(ctx, nil, read.By("Code"), total, cents, avgArea)
	if err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("len(groups) = %d, want 3 (A, B, C — archived twin folded out)", len(groups))
	}
	wantSums := []struct {
		code  string
		cents int64
		area  float64
	}{{"A", 1050, 10.5}, {"B", 120, 20.25}, {"C", 500, 30.0}}
	for i, w := range wantSums {
		if got := groups[i].KeyString("Code"); got != w.code {
			t.Fatalf("groups[%d] key = %q, want %q (ordered by key)", i, got, w.code)
		}
		if n := read.GroupResult(groups[i], total); n.Value != 1 {
			t.Errorf("%s COUNT = %d, want 1", w.code, n.Value)
		}
		if s := read.GroupResult(groups[i], cents); s.Value != w.cents || !s.Found {
			t.Errorf("%s SUM(Cents) = (%d, %v), want (%d, true) — pg NUMERIC normalized exactly", w.code, s.Value, s.Found, w.cents)
		}
		if a := read.GroupResult(groups[i], avgArea); a.Value != w.area || !a.Found {
			t.Errorf("%s AVG(Area) = (%v, %v), want (%v, true)", w.code, a.Value, a.Found, w.area)
		}
	}

	// A predicate narrows the grouped set the same way it narrows Aggregate.
	filtered, err := l.AggregateBy(ctx, criteria.Where(criteria.Gt("Cents", 130)), read.By("Code"), read.Count())
	if err != nil {
		t.Fatalf("AggregateBy(filtered): %v", err)
	}
	if len(filtered) != 2 || filtered[0].KeyString("Code") != "A" || filtered[1].KeyString("Code") != "C" {
		t.Fatalf("filtered groups = %d, want exactly A and C", len(filtered))
	}

	// Empty set: zero groups, never a NULL-keyed placeholder row.
	empty, err := l.AggregateBy(ctx, criteria.Where(criteria.Eq("Code", "ZZ")), read.By("Code"), read.Count())
	if err != nil {
		t.Fatalf("AggregateBy(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty set produced %d groups, want 0", len(empty))
	}
}
