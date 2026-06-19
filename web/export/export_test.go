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
	// root header(0), root data(0), addr header(1), addr1 data(1),
	// geo header(2), geo data(2), addr2 data(1)
	wantDepth := []int{0, 0, 1, 1, 2, 2, 1}
	if len(sink.rows) != len(wantDepth) {
		t.Fatalf("row count=%d want %d: %+v", len(sink.rows), len(wantDepth), sink.rows)
	}
	for i, d := range wantDepth {
		if sink.rows[i].Depth != d {
			t.Fatalf("row %d depth=%d want %d", i, sink.rows[i].Depth, d)
		}
	}
	// the one-to-one Geo embed (map, not slice) rendered once under addr1
	if sink.rows[5].Cells[0].Value != "40" {
		t.Fatalf("expected grandchild Geo Lat=40, got %+v", sink.rows[5])
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
