package export

import (
	"bytes"
	"testing"

	"github.com/xuri/excelize/v2"
)

// A non-pointer 1:1 struct segment renders exactly one child row group per
// parent item — the struct-branch of the typed child descent (the slice branch
// and the nil-pointer branch are covered in export_test.go).
type exportProfileResponse struct {
	Bio string `json:"bio"`
}

type exportOwnerResponse struct {
	Name    string                `json:"name"`
	Profile exportProfileResponse `json:"profile"`
}

func TestGenerate_OneToOneStructSegmentRendersSingleGroup(t *testing.T) {
	plan := planOf[exportOwnerResponse](t)
	items := []exportOwnerResponse{{Name: "Ann", Profile: exportProfileResponse{Bio: "dev"}}}
	sink := &captureSink{}
	if err := Generate(plan, items, idLabel, sink); err != nil {
		t.Fatal(err)
	}
	// name h(0) / Ann(0) / bio h(1) / dev(1) / BLANK
	if len(sink.rows) != 5 {
		t.Fatalf("expected 5 rows, got %+v", sink.rows)
	}
	if !sink.rows[2].Header || sink.rows[2].Depth != 1 || sink.rows[2].Cells[0].Value != "bio" {
		t.Fatalf("expected depth-1 bio header, got %+v", sink.rows[2])
	}
	if sink.rows[3].Depth != 1 || sink.rows[3].Cells[0].Value != "dev" {
		t.Fatalf("expected depth-1 profile data, got %+v", sink.rows[3])
	}
	if len(sink.rows[4].Cells) != 0 {
		t.Fatalf("expected trailing blank separator, got %+v", sink.rows[4])
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
