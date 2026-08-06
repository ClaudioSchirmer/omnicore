package relational

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// countingLoader wraps pageLoader to record how the reader counts: how many
// COUNTs a single ReadPage issues, and under which archived scope. It also
// serves a distinct archived-inclusive row count, so a count taken under the
// WRONG scope produces a visibly wrong Total.
type countingLoader struct {
	pageLoader
	archived int64 // extra rows visible only with ?includeArchived
	counts   int
	scopes   []criteria.Scope
}

func (c *countingLoader) CountEntities(_ context.Context, q *criteria.Query) (int64, error) {
	c.counts++
	c.scopes = append(c.scopes, q.Scope())
	n := int64(len(c.rows))
	if q.Scope() == criteria.ScopeIncludeArchived {
		n += c.archived
	}
	return n, nil
}

func countingReaderWith(rows []domain.Entity, archived int64) (*RelationalViewReader, *countingLoader) {
	l := &countingLoader{pageLoader: pageLoader{table: "gadgets", rows: rows}, archived: archived}
	vdef := query.View("v").Schema(guardSchema("gadgets")).Version(1).RelationalSource(l)
	r := NewRelationalViewReader([]*query.ViewDefinition{vdef})
	r.SetMaxLimitResolver(func(string) int64 { return 100 })
	return r, l
}

// TestReadPage_Listing_CarriesTotal is the regression this file exists for: a
// NORMAL listing (items requested) must report the full match count, not 0.
// Before the fix, Total was assigned only in the OnlyTotal short-circuit, so
// every relational listing served `"total": 0` on the wire while its items were
// correct — the Mongo reader has always populated it on both paths.
func TestReadPage_Listing_CarriesTotal(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()

	p1, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	eqNames(t, names(p1), "r0", "r1") // a partial page...
	if p1.Total != 5 {
		t.Fatalf("page1 Total = %d, want 5 (the FULL match count, not the page size)", p1.Total)
	}

	// Total is a property of the match set, so it stays put as the window walks:
	// forward via after, back via before, and the bare-backward tail anchor.
	p2, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, After: p1.NextCursor})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if p2.Total != 5 {
		t.Errorf("after-page Total = %d, want 5", p2.Total)
	}
	back, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, Before: p2.PrevCursor})
	if err != nil {
		t.Fatalf("before page: %v", err)
	}
	if back.Total != 5 {
		t.Errorf("before-page Total = %d, want 5", back.Total)
	}
	last, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, Backward: true})
	if err != nil {
		t.Fatalf("backward: %v", err)
	}
	if last.Total != 5 {
		t.Errorf("backward-page Total = %d, want 5", last.Total)
	}
}

// TestReadPage_ListingTotalMatchesOnlyTotal pins the parity the consumer report
// broke: the number a listing reports and the number ?onlyTotal=true reports
// are the same number, because they are counted under the same scoped criteria.
func TestReadPage_ListingTotalMatchesOnlyTotal(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()

	listing, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	countOnly, err := r.ReadPage(ctx, "v", queries.ReadCriteria{OnlyTotal: true})
	if err != nil {
		t.Fatalf("onlyTotal: %v", err)
	}
	if !countOnly.OnlyTotal {
		t.Fatal("count-only read lost its OnlyTotal flag")
	}
	if listing.Total != countOnly.Total {
		t.Fatalf("listing Total = %d but onlyTotal Total = %d — they must agree", listing.Total, countOnly.Total)
	}
}

// TestReadPage_EmptyWindow_CarriesTotal covers the zero-width window (paging
// `before` the very first row): it returns without issuing the row query, and
// must still report the match count — an empty PAGE is not an empty RESULT SET.
func TestReadPage_EmptyWindow_CarriesTotal(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()

	first, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	page, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, Before: first.ItemCursors[0]})
	if err != nil {
		t.Fatalf("before-first page: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected the zero-width window to be empty, got %d items", len(page.Items))
	}
	if page.Total != 5 {
		t.Fatalf("zero-width window Total = %d, want 5", page.Total)
	}
}

// TestReadPage_EmptyResultSet_TotalZero is the honest zero: no rows match, so
// total IS 0 — the fix must not invent a count.
func TestReadPage_EmptyResultSet_TotalZero(t *testing.T) {
	r := pageReaderWith(mkRows(0))
	page, err := r.ReadPage(context.Background(), "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("empty listing: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("empty result set Total = %d, want 0", page.Total)
	}
}

// TestReadPage_TotalHonorsArchivedScope proves the count is taken under the
// SAME archived gate as the items: the default read counts active rows only,
// ?includeArchived counts both. A count under the wrong scope would report more
// rows than the listing can ever page through.
func TestReadPage_TotalHonorsArchivedScope(t *testing.T) {
	r, l := countingReaderWith(mkRows(5), 3)
	ctx := context.Background()

	active, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("active listing: %v", err)
	}
	if active.Total != 5 {
		t.Errorf("default-read Total = %d, want 5 (active only)", active.Total)
	}
	if got := l.scopes[len(l.scopes)-1]; got != criteria.ScopeActive {
		t.Errorf("default read counted under scope %v, want ScopeActive", got)
	}

	all, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, IncludeArchived: true})
	if err != nil {
		t.Fatalf("includeArchived listing: %v", err)
	}
	if all.Total != 8 {
		t.Errorf("includeArchived Total = %d, want 8 (5 active + 3 archived)", all.Total)
	}
	if got := l.scopes[len(l.scopes)-1]; got != criteria.ScopeIncludeArchived {
		t.Errorf("includeArchived read counted under scope %v, want ScopeIncludeArchived", got)
	}
}

// TestReadPage_CountsOncePerRead guards the cost of the fix: ONE count per
// ReadPage, on every window branch. The bare-backward branch used to run its
// own COUNT to anchor the tail offset and then discard it; it now reuses the
// single count the page already needs, so backward paging did not get slower.
func TestReadPage_CountsOncePerRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		crit queries.ReadCriteria
	}{
		{"forward first page", queries.ReadCriteria{Limit: 2}},
		{"bare backward", queries.ReadCriteria{Limit: 2, Backward: true}},
		{"count only", queries.ReadCriteria{OnlyTotal: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l := countingReaderWith(mkRows(5), 0)
			if _, err := r.ReadPage(context.Background(), "v", tc.crit); err != nil {
				t.Fatalf("read: %v", err)
			}
			if l.counts != 1 {
				t.Fatalf("issued %d COUNTs, want exactly 1", l.counts)
			}
		})
	}
}
