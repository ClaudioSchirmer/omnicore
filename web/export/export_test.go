package export

import (
	"bytes"
	"encoding/csv"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// idLabel renders headers deterministically for tests: the json wire leaf when
// the column carries no labelKey, "L:<key>" otherwise — mirroring the wrapper's
// fallback-to-wire-name rule.
func idLabel(labelKey, wireLeaf string) string {
	if labelKey == "" {
		return wireLeaf
	}
	return "L:" + labelKey
}

type captureSink struct{ rows []Row }

func (s *captureSink) Write(r Row) error { s.rows = append(s.rows, r); return nil }
func (s *captureSink) Close() error      { return nil }

// ─── plan fixtures — the plan is now derived from the wire Response type ─────

type exportFlatResponse struct {
	Name  string `json:"name"`
	Email string `json:"email" exportLabelKey:"EmailKey"`
}

type exportGeoResponse struct {
	Lat string `json:"lat"`
}

type exportAddressResponse struct {
	City string             `json:"city"`
	Geo  *exportGeoResponse `json:"geo,omitempty"`
}

type exportUserResponse struct {
	Name      string                  `json:"name"`
	Email     string                  `json:"email" exportLabelKey:"EmailKey"`
	Addresses []exportAddressResponse `json:"addresses"`
}

func planOf[T any](t *testing.T) *Plan {
	t.Helper()
	var zero T
	plan := PlanFor(reflect.TypeOf(zero))
	if plan == nil || plan.Root == nil {
		t.Fatalf("PlanFor(%T) yielded no plan", zero)
	}
	return plan
}

// ─── PlanFor — structure derived from the Response DTO ───────────────────────

func TestPlanFor_FlatColumnsCarryWireAndLabel(t *testing.T) {
	plan := planOf[exportFlatResponse](t)
	cols := plan.Root.Columns
	if len(cols) != 2 || len(plan.Root.Children) != 0 {
		t.Fatalf("expected 2 root columns, no children, got %+v", plan.Root)
	}
	if cols[0].GoField != "Name" || cols[0].WireLeaf != "name" || cols[0].LabelKey != "" {
		t.Errorf("Name column mismatch: %+v", cols[0])
	}
	if cols[1].GoField != "Email" || cols[1].WireLeaf != "email" || cols[1].LabelKey != "EmailKey" {
		t.Errorf("Email column mismatch: %+v", cols[1])
	}
}

func TestPlanFor_NestedSegmentsBecomeChildNodes(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	if len(plan.Root.Columns) != 2 {
		t.Fatalf("expected 2 root columns, got %+v", plan.Root.Columns)
	}
	if len(plan.Root.Children) != 1 {
		t.Fatalf("expected 1 child node (addresses), got %d", len(plan.Root.Children))
	}
	addr := plan.Root.Children[0]
	if addr.GoSegment != "Addresses" || addr.WireSegment != "addresses" {
		t.Fatalf("addresses segment mismatch: %+v", addr)
	}
	if len(addr.Columns) != 1 || addr.Columns[0].WireLeaf != "city" {
		t.Fatalf("addresses columns mismatch: %+v", addr.Columns)
	}
	if len(addr.Children) != 1 {
		t.Fatalf("expected geo grandchild, got %d", len(addr.Children))
	}
	geo := addr.Children[0]
	if geo.GoSegment != "Geo" || geo.WireSegment != "geo" {
		t.Fatalf("geo segment mismatch: %+v", geo)
	}
	if len(geo.Columns) != 1 || geo.Columns[0].WireLeaf != "lat" {
		t.Fatalf("geo columns mismatch: %+v", geo.Columns)
	}
}

type exportSkipBase struct {
	ID string `json:"id"`
}

type exportSkipResponse struct {
	exportSkipBase
	Name   string `json:"name"`
	Secret string `json:"-"`
}

func TestPlanFor_EmbeddedPromotionAndJSONDashSkip(t *testing.T) {
	plan := planOf[exportSkipResponse](t)
	cols := plan.Root.Columns
	if len(cols) != 2 {
		t.Fatalf("expected 2 columns (embedded id promoted, Secret skipped), got %+v", cols)
	}
	if cols[0].GoField != "ID" || cols[0].WireLeaf != "id" {
		t.Errorf("promoted embedded column mismatch: %+v", cols[0])
	}
	if cols[1].WireLeaf != "name" {
		t.Errorf("name column mismatch: %+v", cols[1])
	}
}

type exportScalarStructResponse struct {
	ID   domain.ID `json:"id"`
	When time.Time `json:"when"`
	Tags []string  `json:"tags"`
}

func TestPlanFor_SelfMarshalingStructsAndScalarSlicesAreColumns(t *testing.T) {
	plan := planOf[exportScalarStructResponse](t)
	if len(plan.Root.Children) != 0 {
		t.Fatalf("domain.ID/time.Time/[]string must not become child nodes: %+v", plan.Root.Children)
	}
	if len(plan.Root.Columns) != 3 {
		t.Fatalf("expected 3 scalar columns, got %+v", plan.Root.Columns)
	}
}

// TestPlanFor_ResponseDeclaredIDIsAnExportedColumn pins the behavior change:
// a Response declaring `id` exports it as a real column (wire token "id"),
// and inclusion pruning by its Go path keeps it.
func TestPlanFor_ResponseDeclaredIDIsAnExportedColumn(t *testing.T) {
	plan := planOf[exportSkipResponse](t)
	found := false
	for _, c := range plan.Root.Columns {
		if c.WireLeaf == "id" && c.GoField == "ID" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a root `id` column, got %+v", plan.Root.Columns)
	}

	pruned := plan.PruneToProjection(map[string]int{"ID": 1})
	if len(pruned.Root.Columns) != 1 || pruned.Root.Columns[0].WireLeaf != "id" {
		t.Fatalf("inclusion by Go path ID must keep the id column alone, got %+v", pruned.Root.Columns)
	}

	// Rendering the pruned plan emits the id header + value.
	sink := &captureSink{}
	items := []exportSkipResponse{{exportSkipBase: exportSkipBase{ID: "u1"}, Name: "John"}}
	if err := Generate(pruned, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.rows) != 2 {
		t.Fatalf("expected header + 1 data row, got %+v", sink.rows)
	}
	if sink.rows[0].Cells[0].Value != "id" || sink.rows[1].Cells[0].Value != "u1" {
		t.Fatalf("id column render mismatch: %+v", sink.rows)
	}
}

// ─── PruneToProjection — Go-path keyed narrowing ─────────────────────────────

func TestPruneToProjection_EmptyOrAutoIDOnlyKeepsWholePlan(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	if got := plan.PruneToProjection(nil); got != plan {
		t.Error("nil projection must return the plan unchanged")
	}
	if got := plan.PruneToProjection(map[string]int{}); got != plan {
		t.Error("empty projection must return the plan unchanged")
	}
	if got := plan.PruneToProjection(map[string]int{"_id": 0}); got != plan {
		t.Error("`_id`-only auto-exclusion must count as whole-doc")
	}
}

func TestPruneToProjection_InclusionKeepsFlaggedLeaves(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	pruned := plan.PruneToProjection(map[string]int{"Name": 1})
	if len(pruned.Root.Columns) != 1 || pruned.Root.Columns[0].GoField != "Name" {
		t.Fatalf("expected only Name to survive, got %+v", pruned.Root.Columns)
	}
	if len(pruned.Root.Children) != 0 {
		t.Fatalf("addresses segment must drop when no leaf under it is included, got %+v", pruned.Root.Children)
	}
}

func TestPruneToProjection_InclusionOfSegmentPathKeepsWholeSegment(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	pruned := plan.PruneToProjection(map[string]int{"Addresses": 1})
	if len(pruned.Root.Columns) != 0 {
		t.Fatalf("root scalars must drop in inclusion mode, got %+v", pruned.Root.Columns)
	}
	if len(pruned.Root.Children) != 1 {
		t.Fatalf("expected the addresses segment kept whole, got %+v", pruned.Root.Children)
	}
	addr := pruned.Root.Children[0]
	if len(addr.Columns) != 1 || addr.Columns[0].GoField != "City" {
		t.Fatalf("segment columns mismatch: %+v", addr.Columns)
	}
	if len(addr.Children) != 1 || addr.Children[0].GoSegment != "Geo" {
		t.Fatalf("ancestor-included segment must keep its own children, got %+v", addr.Children)
	}
}

func TestPruneToProjection_InclusionOfNestedLeafDropsSiblings(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	pruned := plan.PruneToProjection(map[string]int{"Addresses.City": 1})
	if len(pruned.Root.Columns) != 0 {
		t.Fatalf("root scalars must drop, got %+v", pruned.Root.Columns)
	}
	if len(pruned.Root.Children) != 1 {
		t.Fatalf("expected the addresses segment, got %+v", pruned.Root.Children)
	}
	addr := pruned.Root.Children[0]
	if len(addr.Columns) != 1 || addr.Columns[0].GoField != "City" {
		t.Fatalf("expected only City, got %+v", addr.Columns)
	}
	if len(addr.Children) != 0 {
		t.Fatalf("geo must drop (not included), got %+v", addr.Children)
	}
}

func TestPruneToProjection_ExclusionDropsFlaggedPaths(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	pruned := plan.PruneToProjection(map[string]int{"Email": 0})
	cols := pruned.Root.Columns
	if len(cols) != 1 || cols[0].GoField != "Name" {
		t.Fatalf("expected Email dropped, Name kept, got %+v", cols)
	}
	if len(pruned.Root.Children) != 1 {
		t.Fatalf("addresses must survive exclusion of a sibling, got %+v", pruned.Root.Children)
	}
}

func TestPruneToProjection_ExclusionOfSegmentDropsWholeSegment(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	pruned := plan.PruneToProjection(map[string]int{"Addresses": 0})
	if len(pruned.Root.Columns) != 2 {
		t.Fatalf("root scalars must survive, got %+v", pruned.Root.Columns)
	}
	if len(pruned.Root.Children) != 0 {
		t.Fatalf("excluded segment must drop whole (header included), got %+v", pruned.Root.Children)
	}
}

// ─── Generate — layout over typed []TResp items ──────────────────────────────

func TestGenerate_FlatRoot(t *testing.T) {
	plan := planOf[exportFlatResponse](t)
	items := []exportFlatResponse{
		{Name: "John", Email: "j@x"},
		{Name: "Jane", Email: "j@y"},
	}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.rows) != 3 {
		t.Fatalf("expected header + 2 data rows, got %d", len(sink.rows))
	}
	// Header fallback is the json wire name; a labelKey routes through the label fn.
	if !sink.rows[0].Header || sink.rows[0].Cells[0].Value != "name" || sink.rows[0].Cells[1].Value != "L:EmailKey" {
		t.Fatalf("bad header row: %+v", sink.rows[0])
	}
	if sink.rows[1].Cells[0].Value != "John" || sink.rows[2].Cells[0].Value != "Jane" {
		t.Fatalf("bad data rows: %+v / %+v", sink.rows[1], sink.rows[2])
	}
}

func TestGenerate_HierarchyDepthOffsets(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	items := []exportUserResponse{{
		Name: "John", Email: "j@x",
		Addresses: []exportAddressResponse{
			{City: "NYC", Geo: &exportGeoResponse{Lat: "40"}},
			{City: "LA"},
		},
	}}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	// root header(0), root data(0), addr header(1), addr1 data(1), geo header(2),
	// geo data(2), BLANK (addr1's geo cascade), addr2 data(1), BLANK (root item's
	// addresses cascade).
	type want struct {
		depth  int
		header bool
		blank  bool
	}
	wants := []want{
		{0, true, false},
		{0, false, false},
		{1, true, false},
		{1, false, false},
		{2, true, false},
		{2, false, false},
		{0, false, true}, // blank — addr1's grandchild (geo) concluded
		{1, false, false},
		{0, false, true}, // blank — the root item's addresses concluded
	}
	if len(sink.rows) != len(wants) {
		t.Fatalf("row count=%d want %d: %+v", len(sink.rows), len(wants), sink.rows)
	}
	for i, w := range wants {
		r := sink.rows[i]
		isBlank := len(r.Cells) == 0
		if isBlank != w.blank {
			t.Fatalf("row %d blank=%v want %v: %+v", i, isBlank, w.blank, r)
		}
		if isBlank {
			continue
		}
		if r.Depth != w.depth || r.Header != w.header {
			t.Fatalf("row %d depth=%d header=%v want depth=%d header=%v", i, r.Depth, r.Header, w.depth, w.header)
		}
	}
	// the one-to-one Geo embed (pointer struct, non-nil) rendered once under addr1
	if sink.rows[5].Cells[0].Value != "40" {
		t.Fatalf("expected grandchild Geo Lat=40, got %+v", sink.rows[5])
	}
}

func TestGenerate_BlankAfterEachAggregate(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	items := []exportUserResponse{
		{Name: "John", Addresses: []exportAddressResponse{{City: "NYC"}, {City: "LA"}}},
		{Name: "Jane", Addresses: []exportAddressResponse{{City: "SF"}}},
	}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	// name h / John / city h / NYC / LA / BLANK / Jane / city h / SF / BLANK
	blanks := 0
	for _, r := range sink.rows {
		if len(r.Cells) == 0 {
			blanks++
		}
	}
	if blanks != 2 {
		t.Fatalf("expected one blank after each of the 2 aggregates, got %d: %+v", blanks, sink.rows)
	}
	// blank sits right after John's last address (index 5) and is NOT before Jane's data
	if len(sink.rows[5].Cells) != 0 {
		t.Fatalf("expected blank after John's addresses at row 5, got %+v", sink.rows[5])
	}
	if sink.rows[6].Header || sink.rows[6].Cells[0].Value != "Jane" {
		t.Fatalf("expected Jane's data after the blank, got %+v", sink.rows[6])
	}
}

func TestGenerate_CollapsesNestedConclusionBlanks(t *testing.T) {
	// Single address that itself has a grandchild: the grandchild-conclusion blank
	// and the (immediately following) addresses-conclusion blank collapse into one.
	plan := planOf[exportUserResponse](t)
	items := []exportUserResponse{{
		Name:      "John",
		Addresses: []exportAddressResponse{{City: "NYC", Geo: &exportGeoResponse{Lat: "40"}}},
	}}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	// name h / John / city h / NYC / lat h / 40 / BLANK — exactly one blank, no stack.
	blanks := 0
	consecutive := false
	prevBlank := false
	for _, r := range sink.rows {
		b := len(r.Cells) == 0
		if b {
			blanks++
			if prevBlank {
				consecutive = true
			}
		}
		prevBlank = b
	}
	if blanks != 1 {
		t.Fatalf("nested conclusions must collapse to one blank, got %d: %+v", blanks, sink.rows)
	}
	if consecutive {
		t.Fatal("found consecutive blank rows — collapse failed")
	}
}

type exportPtrResponse struct {
	Name *string `json:"name,omitempty"`
	Age  *int64  `json:"age,omitempty"`
}

func TestGenerate_PointerCellsDerefAndNilStaysEmpty(t *testing.T) {
	plan := planOf[exportPtrResponse](t)
	items := []exportPtrResponse{{Name: ptrOf("Ann")}} // Age nil
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.rows) != 2 {
		t.Fatalf("expected header + data, got %+v", sink.rows)
	}
	data := sink.rows[1]
	if data.Cells[0].Value != "Ann" {
		t.Errorf("non-nil pointer must deref to its scalar, got %v", data.Cells[0].Value)
	}
	if data.Cells[1].Value != nil {
		t.Errorf("nil pointer must yield a nil (empty) cell, got %v", data.Cells[1].Value)
	}
}

func TestGenerate_NilPointerSegmentYieldsNoChildRows(t *testing.T) {
	plan := planOf[exportUserResponse](t)
	items := []exportUserResponse{{
		Name:      "Solo",
		Addresses: []exportAddressResponse{{City: "NYC"}}, // Geo nil
	}}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	for _, r := range sink.rows {
		if r.Depth == 2 {
			t.Fatalf("nil Geo pointer must not render a depth-2 group: %+v", sink.rows)
		}
	}
}

// ─── CSV encoder ─────────────────────────────────────────────────────────────

func TestCSVEncoder_BlankRow(t *testing.T) {
	var buf bytes.Buffer
	sink, err := CSV().Open(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_ = sink.Write(Row{Header: true, Cells: []Cell{{Value: "A"}, {Value: "B"}}})
	_ = sink.Write(Row{}) // blank separator
	_ = sink.Write(Row{Cells: []Cell{{Value: "C"}, {Value: "D"}}})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "A,B\n\nC,D") {
		t.Fatalf("a zero-cell Row must render a blank line; got %q", got)
	}
}

func TestCSVEncoder_DepthPaddingAndDelimiter(t *testing.T) {
	enc := CSV(WithDelimiter(';'))
	if enc.ContentType() != "text/csv; charset=utf-8" {
		t.Fatalf("content-type: %q", enc.ContentType())
	}
	if enc.Extension() != "csv" {
		t.Fatalf("extension: %q", enc.Extension())
	}
	var buf bytes.Buffer
	sink, err := enc.Open(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_ = sink.Write(Row{Depth: 0, Header: true, Cells: []Cell{{Value: "Name"}}})
	_ = sink.Write(Row{Depth: 0, Cells: []Cell{{Value: "John"}}})
	_ = sink.Write(Row{Depth: 1, Cells: []Cell{{Value: "NYC"}}})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	r := csv.NewReader(strings.NewReader(buf.String()))
	r.Comma = ';'
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("re-parse with ';' delimiter: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("records=%d", len(recs))
	}
	if len(recs[2]) != 2 || recs[2][0] != "" || recs[2][1] != "NYC" {
		t.Fatalf("depth-1 row should pad one empty leading cell, got %v", recs[2])
	}
}

func TestStringifyCell(t *testing.T) {
	s := "ptr"
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"a", "a"},
		{&s, "ptr"},
		{(*string)(nil), ""},
		{true, "true"},
		{int64(5), "5"},
		{int(7), "7"},
		{3.14, "3.14"},
		// Non-string pointers: nil renders an EMPTY cell (never "<nil>"),
		// non-nil dereferences to its scalar rendering.
		{(*int64)(nil), ""},
		{ptrOf(int64(42)), "42"},
		{(*float64)(nil), ""},
		{ptrOf(2.5), "2.5"},
		{(*bool)(nil), ""},
		{ptrOf(true), "true"},
		{(*time.Time)(nil), ""},
		{ptrOf(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), "2026-09-01T00:00:00Z"},
	}
	for _, c := range cases {
		if got := stringifyCell(c.in); got != c.want {
			t.Errorf("stringifyCell(%v)=%q want %q", c.in, got, c.want)
		}
	}
}

// ptrOf is a test helper: a pointer to any scalar literal.
func ptrOf[T any](v T) *T { return &v }
