package export

import (
	"bytes"
	"testing"

	"github.com/xuri/excelize/v2"
)

// extractChildItems normalizes a node's GoSegment value into a slice of docs.
// The []map[string]any and map[string]any shapes are exercised by export_test;
// here we cover the []any branch (with a non-map element filtered out) and the
// default branch (a value of an unexpected shape yields no rows).
func TestExtractChildItems_AnySliceFiltersNonMaps(t *testing.T) {
	in := []any{
		map[string]any{"City": "NYC"},
		"not-a-map", // must be skipped, not panic
		map[string]any{"City": "LA"},
	}
	out := extractChildItems(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 maps (non-map filtered), got %d: %+v", len(out), out)
	}
	if out[0]["City"] != "NYC" || out[1]["City"] != "LA" {
		t.Fatalf("unexpected contents: %+v", out)
	}
}

func TestExtractChildItems_AnySliceAllNonMaps(t *testing.T) {
	out := extractChildItems([]any{1, "x", true})
	if len(out) != 0 {
		t.Fatalf("expected no rows when no element is a map, got %+v", out)
	}
}

func TestExtractChildItems_UnexpectedShapeYieldsNil(t *testing.T) {
	cases := []any{nil, "string", 42, []string{"a", "b"}}
	for _, c := range cases {
		if out := extractChildItems(c); out != nil {
			t.Errorf("extractChildItems(%v) = %+v, want nil", c, out)
		}
	}
}

// normalizeXLSXValue dereferences *string and maps nil to "" so excelize gets a
// concrete value; concrete scalars pass through untouched.
func TestNormalizeXLSXValue(t *testing.T) {
	s := "hello"
	var nilStr *string
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"nil", nil, ""},
		{"string-ptr", &s, "hello"},
		{"nil-string-ptr", nilStr, ""},
		{"plain-string", "x", "x"},
		{"int64", int64(7), int64(7)},
		{"bool", true, true},
		{"float", 3.14, 3.14},
		// Non-string pointers: nil is an empty cell, non-nil dereferences —
		// excelize must always receive a concrete value, never a pointer.
		{"nil-int64-ptr", (*int64)(nil), ""},
		{"int64-ptr", ptrOf(int64(9)), int64(9)},
		{"nil-float-ptr", (*float64)(nil), ""},
		{"float-ptr", ptrOf(1.25), 1.25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeXLSXValue(c.in); got != c.want {
				t.Errorf("normalizeXLSXValue(%v) = %v (%T), want %v (%T)", c.in, got, got, c.want, c.want)
			}
		})
	}
}

// Open with the default sheet name skips the SetSheetName rename branch; this
// exercises that path (the existing tests always rename via WithSheetName).
func TestXLSXEncoder_OpenDefaultSheet(t *testing.T) {
	var buf bytes.Buffer
	enc := XLSX() // default "Sheet1"
	sink, err := enc.Open(&buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sink.Write(Row{Cells: []Cell{{Value: "A"}}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer f.Close()
	got, _ := f.GetCellValue("Sheet1", "A1")
	if got != "A" {
		t.Fatalf("default sheet A1 = %q, want A", got)
	}
}

// WithSheetName("") falls back to the default "Sheet1" (the guard inside XLSX).
func TestXLSX_EmptySheetNameFallsBackToDefault(t *testing.T) {
	var buf bytes.Buffer
	sink, err := XLSX(WithSheetName("")).Open(&buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = sink.Write(Row{Cells: []Cell{{Value: "Z"}}})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer f.Close()
	if got, _ := f.GetCellValue("Sheet1", "A1"); got != "Z" {
		t.Fatalf("expected fallback Sheet1 A1=Z, got %q", got)
	}
}
