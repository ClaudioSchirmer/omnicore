package relational

import (
	"context"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// pageLoader is a stateful, no-DB query.AggregateReader: it serves a fixed,
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
func (p *pageLoader) Schema() *core.TableSchema       { return guardSchema(p.table) }
func (p *pageLoader) JoinFields() map[string][]string { return nil }

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

func pageReaderWith(rows []domain.Entity) *ViewReader {
	vdef := query.RelationalView("v", &pageLoader{table: "gadgets", rows: rows})
	r := NewViewReader([]*query.RelationalViewDefinition{vdef})
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

// TestReadPage_EdgeCursors_OnlyWithNeighbour pins the envelope rule the reader
// shares with the Mongo backing: an EDGE cursor exists only where a neighbouring
// page does — EndCursor iff HasNextPage, StartCursor iff HasPreviousPage. The
// reader used to emit both unconditionally whenever the page had rows, so the
// LAST page handed back an EndCursor while announcing HasNextPage=false; a client
// treating a present cursor as "there is more" would spend it for an empty page,
// and the same view read through a Mongo projection answered differently.
//
// ItemCursors is deliberately NOT gated: it addresses ROWS, not boundaries, so
// the final row still has a cursor (GraphQL edges[].cursor depends on it).
func TestReadPage_EdgeCursors_OnlyWithNeighbour(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()

	// Head of a forward walk: nothing behind, more ahead.
	p1, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if p1.StartCursor != "" {
		t.Errorf("page1 StartCursor = %q, want empty (HasPreviousPage=false)", p1.StartCursor)
	}
	if p1.EndCursor == "" {
		t.Error("page1 EndCursor is empty, want set (HasPreviousPage=false but HasNextPage=true)")
	}

	// Tail of the same walk: rows behind, nothing ahead.
	p2, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, After: p1.EndCursor})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	last, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, After: p2.EndCursor})
	if err != nil {
		t.Fatalf("last page: %v", err)
	}
	if last.HasNextPage {
		t.Fatalf("last page HasNextPage = true, want false (fixture has 5 rows)")
	}
	if last.EndCursor != "" {
		t.Errorf("last page EndCursor = %q, want empty (HasNextPage=false)", last.EndCursor)
	}
	if last.StartCursor == "" {
		t.Error("last page StartCursor is empty, want set (HasPreviousPage=true)")
	}
	if len(last.ItemCursors) != len(last.Items) || last.ItemCursors[0] == "" {
		t.Errorf("last page ItemCursors = %v, want one non-empty cursor per row", last.ItemCursors)
	}

	// A set that fits in ONE page has neither neighbour: both edges stay empty
	// while every row keeps its own cursor.
	only, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("single page: %v", err)
	}
	if only.HasNextPage || only.HasPreviousPage {
		t.Fatalf("single page flags: hasNext=%v hasPrev=%v, want false/false", only.HasNextPage, only.HasPreviousPage)
	}
	if only.StartCursor != "" || only.EndCursor != "" {
		t.Errorf("single page cursors = (%q, %q), want both empty", only.StartCursor, only.EndCursor)
	}
	if len(only.ItemCursors) != len(only.Items) {
		t.Errorf("single page ItemCursors = %d, want %d (one per row)", len(only.ItemCursors), len(only.Items))
	}
}

// TestReadPage_InvalidCursor_IsTypedSchemaRejection pins that a cursor this
// reader refuses comes back as the framework's TYPED rejection — the same
// core.InvalidCursorError the Mongo reader raises — and not a bare sentinel.
// The distinction is the whole difference between a legible 400
// (SchemaViolationNotification) and a 500/Internal: the pipeline maps the typed
// infrastructure error onto SemanticSchema, while an untyped error has nothing
// to map and falls through as an opaque server fault. It bit on every surface —
// REST 500, gRPC {"code":"internal"} — for a cursor spent under a changed
// filter or archived gate, which is ordinary consumer behaviour, not abuse.
func TestReadPage_InvalidCursor_IsTypedSchemaRejection(t *testing.T) {
	r := pageReaderWith(mkRows(5))
	ctx := context.Background()

	p1, err := r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}

	// A cursor issued with NO filter, spent under one: the context hash differs,
	// so the reader must refuse — typed.
	_, err = r.ReadPage(ctx, "v", queries.ReadCriteria{
		Limit:  2,
		After:  p1.EndCursor,
		Filter: map[string]any{"Name": "r0"},
	})
	assertTypedCursorRejection(t, "context-hash mismatch", err)

	// An undecodable cursor takes the same path.
	_, err = r.ReadPage(ctx, "v", queries.ReadCriteria{Limit: 2, After: "not-base64---"})
	assertTypedCursorRejection(t, "undecodable cursor", err)
}

// assertTypedCursorRejection fails unless err carries the framework's
// notification envelope (the carrier every layer above reads via errors.As) —
// which is what turns the refusal into a 400 rather than a 500.
func assertTypedCursorRejection(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a rejection, got nil", label)
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("%s: error %v (%T) does not carry notifications — it would surface as 500/Internal", label, err, err)
	}
	notes := carrier.NotificationContexts()
	if len(notes) == 0 {
		t.Fatalf("%s: carrier holds no notification", label)
	}
	found := false
	for _, n := range notes {
		for _, m := range n.Messages() {
			if _, ok := m.Notification.(domain.SchemaViolationNotification); ok {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("%s: want a SchemaViolationNotification, got %+v", label, notes)
	}
}

// TestReadPage_FieldsProjectionWithoutID_WalkAdvances is the relational half of
// the cross-engine parity guard for a walk whose selection OMITS the identity
// (?fields=name, a GraphQL selection without id, a gRPC read mask).
//
// The Mongo reader pages by KEYSET and puts the stored `_id` in the cursor's
// trailing tiebreak slot, so a projection that dropped it once produced a
// cursor over a missing value. This engine pages by OFFSET — the cursor carries
// a row index, not row values, and the projection is applied in memory AFTER
// the document is built — so nothing about the selection can reach the cursor.
// The test pins that: flipping a view's backing between the two engines must
// not change whether a paged read can be walked, nor the wire shape it serves.
func TestReadPage_FieldsProjectionWithoutID_WalkAdvances(t *testing.T) {
	r := pageReaderWith(mkRows(3))
	ctx := context.Background()
	crit := queries.ReadCriteria{
		Limit:      1,
		OrderBy:    []queries.OrderByField{{Field: "Name"}},
		Projection: queries.ProjectOnlyPaths("Name"),
	}

	after := ""
	for i, want := range []string{"r0", "r1", "r2"} {
		crit.After = after
		page, err := r.ReadPage(ctx, "v", crit)
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		eqNames(t, names(page), want)
		if _, leaked := page.Items[0]["ID"]; leaked {
			t.Fatalf("page %d: the identity leaked onto the wire for a Name-only selection: %#v",
				i+1, page.Items[0])
		}
		if i < 2 {
			if !page.HasNextPage || page.EndCursor == "" {
				t.Fatalf("page %d: want a next page and an EndCursor, got hasNext=%v cursor=%q",
					i+1, page.HasNextPage, page.EndCursor)
			}
			if page.EndCursor == after {
				t.Fatalf("page %d: EndCursor repeated the incoming cursor — the walk is stalled", i+1)
			}
			after = page.EndCursor
		}
	}
}
