package export

import (
	"bytes"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestXLSXEncoder_OffsetsAndTypedCells(t *testing.T) {
	enc := XLSX(WithSheetName("Export"))
	if enc.ContentType() != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Fatalf("content-type: %q", enc.ContentType())
	}
	if enc.Extension() != "xlsx" {
		t.Fatalf("extension: %q", enc.Extension())
	}

	var buf bytes.Buffer
	sink, err := enc.Open(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// root header / root data (numeric) / addr header (depth 1) / addr data (depth 1)
	_ = sink.Write(Row{Depth: 0, Header: true, Cells: []Cell{{Value: "Name"}, {Value: "Age"}}})
	_ = sink.Write(Row{Depth: 0, Cells: []Cell{{Value: "John"}, {Value: int64(42)}}})
	_ = sink.Write(Row{Depth: 1, Header: true, Cells: []Cell{{Value: "City"}}})
	_ = sink.Write(Row{Depth: 1, Cells: []Cell{{Value: "NYC"}}})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := excelize.OpenReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer f.Close()

	check := func(axis, want string) {
		got, err := f.GetCellValue("Export", axis)
		if err != nil {
			t.Fatalf("GetCellValue(%s): %v", axis, err)
		}
		if got != want {
			t.Errorf("cell %s = %q, want %q", axis, got, want)
		}
	}
	check("A1", "Name")
	check("B1", "Age")
	check("A2", "John")
	check("B2", "42") // int64 stored as a numeric cell; GetCellValue renders "42"
	check("A3", "")   // depth-1 row leaves column A empty (the offset)
	check("B3", "City")
	check("A4", "")
	check("B4", "NYC")

	// header row is bold (StyleID non-zero on a header cell, zero on a data cell)
	hStyle, _ := f.GetCellStyle("Export", "A1")
	dStyle, _ := f.GetCellStyle("Export", "A2")
	if hStyle == dStyle {
		t.Errorf("expected header cell to carry a distinct (bold) style; both = %d", hStyle)
	}
}
