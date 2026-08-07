package relational

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// pageLoader is a stateful, no-DB query.RelationalReader: it serves a fixed,
// id-ordered row set honoring the criteria's Offset/Limit exactly as the SoR
// would, so the offset-in-cursor pagination (resolveWindow's after / before /
// backward branches) can be exercised end-to-end through ReadPage without a
// database. The reader always appends OrderBy("ID"); rows are pre-sorted to
// match, so the fake can ignore the order clause and slice by offset.
type pageLoader struct {
	table string
	rows  []domain.Entity
}

func (p *pageLoader) FindAllEntities(_ context.Context, q *criteria.Query) ([]domain.Entity, error) {
	off := q.OffsetValue()
	if off < 0 {
		off = 0
	}
	if off >= int64(len(p.rows)) {
		return []domain.Entity{}, nil
	}
	end := int64(len(p.rows))
	if lim := q.LimitValue(); lim > 0 && off+lim < end {
		end = off + lim
	}
	return p.rows[off:end], nil
}
func (p *pageLoader) CountEntities(context.Context, *criteria.Query) (int64, error) {
	return int64(len(p.rows)), nil
}
func (p *pageLoader) BoundTable() string { return p.table }

// mkRows builds n guardEnt roots with ids/names r0..r{n-1}, ascending — the
// deterministic order the reader's ORDER BY ID tiebreak assumes.
func mkRows(n int) []domain.Entity {
	out := make([]domain.Entity, n)
	for i := 0; i < n; i++ {
		e := &guardEnt{Name: rowName(i)}
		e.SetID(domain.NewID(rowName(i)))
		out[i] = e
	}
	return out
}

func rowName(i int) string { return "r" + string(rune('0'+i)) }

func pageReaderWith(rows []domain.Entity) *RelationalViewReader {
	vdef := query.View("v").Schema(guardSchema("gadgets")).Version(1).RelationalSource(&pageLoader{table: "gadgets", rows: rows})
	r := NewRelationalViewReader([]*query.ViewDefinition{vdef})
	r.SetMaxLimitResolver(func(string) int64 { return 100 })
	return r
}

// names extracts each item's Go "Name" field, the per-row identity the tests
// assert on (so a reader returning the wrong ROWS — not just the wrong count —
// is caught).
func names(p queries.Page) []string {
	out := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		out = append(out, it["Name"].(string))
	}
	return out
}

func eqNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}

// Fix #2: paging `before` the very first row (a cursor pointing at offset 0)
// must return an EMPTY page — not load the whole table. Pre-fix, resolveWindow
// produced fetchLimit==0, ReadPage issued q.Limit(0), and applyWindow rendered
// NO LIMIT clause, so the loader streamed every row and bypassed MaxLimit.
func TestReadPage_BeforeFirstCursor_EmptyPageNoFullLoad(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()

	first, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.ItemCursors) == 0 {
		t.Fatal("first page carried no item cursors")
	}
	startCursor := first.ItemCursors[0] // encodes offset 0 — the first row

	page, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, Before: startCursor})
	if err != nil {
		t.Fatalf("before-first page: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("before the first row must yield 0 items, got %d (%v)", len(page.Items), names(page))
	}
	if page.HasPreviousPage {
		t.Error("HasPrev must be false before the first row")
	}
	if !page.HasNextPage {
		t.Error("HasNext must be true — rows exist ahead of the window")
	}
}

// TestReadPage_ForwardAfter_WalksFullSetNoDup proves the forward path: the
// default first page, then following NextCursor with `after`, visits every row
// exactly once in order, with HasPrev/HasNext derived correctly at each step
// (the over-fetch probe drives HasNext).
func TestReadPage_ForwardAfter_WalksFullSetNoDup(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()
	crit := queries.ReadCriteria{Limit: 2}

	p1, err := r.ReadPage(ctx, "v", crit)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	eqNames(t, names(p1), "r0", "r1")
	if p1.HasPreviousPage || !p1.HasNextPage {
		t.Fatalf("page1 flags: hasPrev=%v hasNext=%v, want false/true", p1.HasPreviousPage, p1.HasNextPage)
	}

	crit.After = p1.EndCursor
	p2, err := r.ReadPage(ctx, "v", crit)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	eqNames(t, names(p2), "r2", "r3")
	if !p2.HasPreviousPage || !p2.HasNextPage {
		t.Fatalf("page2 flags: hasPrev=%v hasNext=%v, want true/true", p2.HasPreviousPage, p2.HasNextPage)
	}

	crit.After = p2.EndCursor
	p3, err := r.ReadPage(ctx, "v", crit)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	eqNames(t, names(p3), "r4")
	if !p3.HasPreviousPage || p3.HasNextPage {
		t.Fatalf("page3 flags: hasPrev=%v hasNext=%v, want true/false (last page)", p3.HasPreviousPage, p3.HasNextPage)
	}
}

// TestReadPage_Backward_AnchorsAtEnd proves the bare-backward branch (GraphQL
// last:N with no before): it anchors at the tail via one COUNT and returns the
// final window, HasNext false (it IS the end), HasPrev true.
func TestReadPage_Backward_AnchorsAtEnd(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	p, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{Limit: 2, Backward: true})
	if err != nil {
		t.Fatalf("backward: %v", err)
	}
	eqNames(t, names(p), "r3", "r4")
	if !p.HasPreviousPage || p.HasNextPage {
		t.Fatalf("backward flags: hasPrev=%v hasNext=%v, want true/false", p.HasPreviousPage, p.HasNextPage)
	}
}

// TestReadPage_Before_WalksBack proves the before branch: given a cursor reached
// by walking forward, paging `before` it returns the preceding window and lands
// back at the start (HasPrev false, HasNext true).
func TestReadPage_Before_WalksBack(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()

	p1, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	p2, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, After: p1.EndCursor})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	// p2 covers offsets 2..3; its PrevCursor is offset 2 — paging BEFORE it must
	// return offsets 0..1, back at the start.
	back, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, Before: p2.StartCursor})
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	eqNames(t, names(back), "r0", "r1")
	if back.HasPreviousPage || !back.HasNextPage {
		t.Fatalf("before flags: hasPrev=%v hasNext=%v, want false/true", back.HasPreviousPage, back.HasNextPage)
	}
}
