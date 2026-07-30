//go:build integration && sqlserver

package sqlserver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// Integration proof for the loader's Exists probe and aggregate DSL against a
// REAL SQL Server: what the unit fakes cannot certify — the concrete types
// go-mssqldb delivers for SUM/AVG/MIN/MAX (BIGINT stays native int64; AVG over
// FLOAT stays float64), NULL-on-empty-set scanning, the scope gate over live
// rows, the TOP-rendered existence probes, and Ne("ID") encoding a uuid string
// against a BINARY(16) primary key end to end.

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
		DeletedAt("deleted_at")
}

func probeSetup(t *testing.T) (*Engine, string) {
	t.Helper()
	eng, raw := setup(t)
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, `CREATE TABLE probe_items (
		id BINARY(16) NOT NULL PRIMARY KEY,
		code VARCHAR(32) NOT NULL,
		cents BIGINT NOT NULL,
		area FLOAT NOT NULL,
		deleted_at DATETIME2(6) NULL
	)`); err != nil {
		t.Fatalf("create probe_items: %v", err)
	}
	idA := uuid.New()
	seed := func(id uuid.UUID, code string, cents int64, area float64, archived bool) {
		t.Helper()
		var del any
		if archived {
			del = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO probe_items (id, code, cents, area, deleted_at) VALUES (@p1, @p2, @p3, @p4, @p5)`,
			id[:], code, cents, area, del); err != nil {
			t.Fatalf("seed %s: %v", code, err)
		}
	}
	// 10.50 + 1.20 + 5.00 in minor units — integer arithmetic end to end.
	seed(idA, "A", 1050, 10.5, false)
	seed(uuid.New(), "B", 120, 20.25, false)
	seed(uuid.New(), "C", 500, 30.0, false)
	seed(uuid.New(), "A", 999999, 99.9, true) // archived twin of A
	return eng, idA.String()
}

func probeLoader(eng *Engine) *read.AggregateLoader[*probeItem] {
	return read.NewAggregateLoader[*probeItem](eng, func() *probeItem { return &probeItem{} }).
		WithSchema(probeSchema())
}

func TestProbes_SQLServer_ExistsScopeAndExcludeSelf(t *testing.T) {
	eng, idA := probeSetup(t)
	l := probeLoader(eng)
	ctx := context.Background()

	if ok, err := l.Exists(ctx, criteria.Where(criteria.Eq("Code", "A"))); err != nil || !ok {
		t.Fatalf("Exists(A) = (%v, %v), want true (active row)", ok, err)
	}
	if ok, err := l.Exists(ctx, criteria.Where(criteria.Eq("Code", "ZZ"))); err != nil || ok {
		t.Fatalf("Exists(ZZ) = (%v, %v), want false", ok, err)
	}
	// Ne("ID", <uuid string>) against a BINARY(16) ID — the encode path that a
	// text/binary mismatch would silently break: excluding the active A leaves
	// only the archived twin → false proves BOTH the encoding and the gate.
	if ok, err := l.Exists(ctx, criteria.Where(criteria.And(
		criteria.Eq("Code", "A"), criteria.Ne("ID", idA)))); err != nil || ok {
		t.Fatalf("Exists(A, excluding the active row) = (%v, %v), want false — encoding or scope gate broke", ok, err)
	}
	if ok, err := l.Exists(ctx, criteria.Where(criteria.And(
		criteria.Eq("Code", "A"), criteria.Ne("ID", "00000000-0000-0000-0000-000000000000")))); err != nil || !ok {
		t.Fatalf("Exists(A, excluding an unrelated id) = (%v, %v), want true", ok, err)
	}
}

func TestProbes_SQLServer_AggregateDSL(t *testing.T) {
	eng, _ := probeSetup(t)
	l := probeLoader(eng)
	ctx := context.Background()

	// Every fact in ONE SELECT. On SQL Server SUM/MIN/MAX over BIGINT stay
	// native int64 and AVG over FLOAT stays float64 — the minor-unit
	// arithmetic must come back exact either way.
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
		t.Fatal("SumInt over FLOAT must error loudly (use Sum)")
	}
}

func TestProbes_SQLServer_AggregateBy(t *testing.T) {
	eng, _ := probeSetup(t)
	l := probeLoader(eng)
	ctx := context.Background()

	// Per-group facts in ONE SELECT, groups ordered by key. The archived 'A'
	// twin (999999 cents) must not leak into the A group.
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
		if got, rawKey := groups[i].KeyString("Code"), groups[i].Key("Code"); got != w.code || rawKey != w.code {
			t.Fatalf("groups[%d] key = (%q, %#v), want %q as a plain string", i, got, rawKey, w.code)
		}
		if n := read.GroupResult(groups[i], total); n.Value != 1 {
			t.Errorf("%s COUNT = %d, want 1", w.code, n.Value)
		}
		if s := read.GroupResult(groups[i], cents); s.Value != w.cents || !s.Found {
			t.Errorf("%s SUM(Cents) = (%d, %v), want (%d, true)", w.code, s.Value, s.Found, w.cents)
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
