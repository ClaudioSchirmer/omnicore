package export

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// idLabel renders headers deterministically for tests: bare field name when the
// column carries no labelKey, "L:<key>" otherwise.
func idLabel(labelKey, goField string) string {
	if labelKey == "" {
		return goField
	}
	return "L:" + labelKey
}

type captureSink struct{ rows []Row }

func (s *captureSink) Write(r Row) error { s.rows = append(s.rows, r); return nil }
func (s *captureSink) Close() error      { return nil }

func TestGenerate_FlatRoot(t *testing.T) {
	plan := &queries.ExportPlan{Root: &queries.ExportNode{
		Columns: []queries.ExportColumn{
			{GoField: "Name", WireLeaf: "name"},
			{GoField: "Email", WireLeaf: "email", LabelKey: "EmailKey"},
		},
	}}
	items := []map[string]any{
		{"Name": "John", "Email": "j@x"},
		{"Name": "Jane", "Email": "j@y"},
	}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.rows) != 3 {
		t.Fatalf("expected header + 2 data rows, got %d", len(sink.rows))
	}
	if !sink.rows[0].Header || sink.rows[0].Cells[0].Value != "Name" || sink.rows[0].Cells[1].Value != "L:EmailKey" {
		t.Fatalf("bad header row: %+v", sink.rows[0])
	}
	if sink.rows[1].Cells[0].Value != "John" || sink.rows[2].Cells[0].Value != "Jane" {
		t.Fatalf("bad data rows: %+v / %+v", sink.rows[1], sink.rows[2])
	}
}

func TestGenerate_HierarchyDepthOffsets(t *testing.T) {
	plan := &queries.ExportPlan{Root: &queries.ExportNode{
		Columns: []queries.ExportColumn{{GoField: "Name", WireLeaf: "name"}},
		Children: []*queries.ExportNode{{
			GoSegment: "Addresses", WireSegment: "addresses",
			Columns: []queries.ExportColumn{{GoField: "City", WireLeaf: "city"}},
			Children: []*queries.ExportNode{{
				GoSegment: "Geo", WireSegment: "geo",
				Columns: []queries.ExportColumn{{GoField: "Lat", WireLeaf: "lat"}},
			}},
		}},
	}}
	items := []map[string]any{{
		"Name": "John",
		"Addresses": []map[string]any{
			{"City": "NYC", "Geo": map[string]any{"Lat": "40"}},
			{"City": "LA"},
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
	// the one-to-one Geo embed (map, not slice) rendered once under addr1
	if sink.rows[5].Cells[0].Value != "40" {
		t.Fatalf("expected grandchild Geo Lat=40, got %+v", sink.rows[5])
	}
}

func TestGenerate_BlankAfterEachAggregate(t *testing.T) {
	plan := &queries.ExportPlan{Root: &queries.ExportNode{
		Columns: []queries.ExportColumn{{GoField: "Name", WireLeaf: "name"}},
		Children: []*queries.ExportNode{{
			GoSegment: "Addresses", WireSegment: "addresses",
			Columns: []queries.ExportColumn{{GoField: "City", WireLeaf: "city"}},
		}},
	}}
	items := []map[string]any{
		{"Name": "John", "Addresses": []map[string]any{{"City": "NYC"}, {"City": "LA"}}},
		{"Name": "Jane", "Addresses": []map[string]any{{"City": "SF"}}},
	}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	// Name h / John / City h / NYC / LA / BLANK / Jane / City h / SF / BLANK
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
	plan := &queries.ExportPlan{Root: &queries.ExportNode{
		Columns: []queries.ExportColumn{{GoField: "Name", WireLeaf: "name"}},
		Children: []*queries.ExportNode{{
			GoSegment: "Addresses", WireSegment: "addresses",
			Columns: []queries.ExportColumn{{GoField: "City", WireLeaf: "city"}},
			Children: []*queries.ExportNode{{
				GoSegment: "Geo", WireSegment: "geo",
				Columns: []queries.ExportColumn{{GoField: "Lat", WireLeaf: "lat"}},
			}},
		}},
	}}
	items := []map[string]any{{
		"Name":      "John",
		"Addresses": []map[string]any{{"City": "NYC", "Geo": map[string]any{"Lat": "40"}}},
	}}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	// Name h / John / City h / NYC / Lat h / 40 / BLANK  — exactly one blank, no stack.
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
	}
	for _, c := range cases {
		if got := stringifyCell(c.in); got != c.want {
			t.Errorf("stringifyCell(%v)=%q want %q", c.in, got, c.want)
		}
	}
}
