package export

import (
	"io"

	"github.com/xuri/excelize/v2"
)

// XLSXOption configures the XLSX encoder at mount time.
type XLSXOption func(*xlsxEncoder)

// WithSheetName sets the worksheet name (default "Sheet1").
func WithSheetName(name string) XLSXOption {
	return func(e *xlsxEncoder) { e.sheet = name }
}

// XLSX builds the Excel (.xlsx) Encoder. It is a drop-in alternative to CSV:
// the format-neutral plan walk, the per-level column offset, the labelKey
// headers, and the `?fields=` pruning are unchanged — only the serialization
// differs. Header rows are emitted bold, and cell values keep their Go type
// (numbers stay numeric, not stringified), which is exactly what `Cell.Value any`
// on the neutral Row was designed to allow.
func XLSX(opts ...XLSXOption) Encoder {
	e := &xlsxEncoder{sheet: "Sheet1"}
	for _, o := range opts {
		o(e)
	}
	if e.sheet == "" {
		e.sheet = "Sheet1"
	}
	return e
}

type xlsxEncoder struct {
	sheet string
}

func (e *xlsxEncoder) ContentType() string {
	return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
}
func (e *xlsxEncoder) Extension() string { return "xlsx" }

func (e *xlsxEncoder) Open(w io.Writer) (Sink, error) {
	f := excelize.NewFile()
	if e.sheet != "Sheet1" {
		if err := f.SetSheetName("Sheet1", e.sheet); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	// Bold style for header rows — created before the StreamWriter so its
	// StyleID is available to the first SetRow.
	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	sw, err := f.NewStreamWriter(e.sheet)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &xlsxSink{f: f, sw: sw, w: w, bold: bold}, nil
}

type xlsxSink struct {
	f    *excelize.File
	sw   *excelize.StreamWriter
	w    io.Writer
	bold int
	row  int // last written 1-based row; StreamWriter requires increasing rows
}

func (s *xlsxSink) Write(r Row) error {
	s.row++
	// Anchor the row at column (Depth+1) so the first Depth columns stay empty —
	// the per-level offset, in the spreadsheet's own coordinate system rather
	// than via empty leading cells.
	anchor, err := excelize.CoordinatesToCellName(r.Depth+1, s.row)
	if err != nil {
		return err
	}
	vals := make([]interface{}, len(r.Cells))
	for i, c := range r.Cells {
		v := normalizeXLSXValue(c.Value)
		if r.Header {
			vals[i] = excelize.Cell{StyleID: s.bold, Value: v}
		} else {
			vals[i] = v
		}
	}
	return s.sw.SetRow(anchor, vals)
}

func (s *xlsxSink) Close() error {
	if err := s.sw.Flush(); err != nil {
		_ = s.f.Close()
		return err
	}
	if err := s.f.Write(s.w); err != nil {
		_ = s.f.Close()
		return err
	}
	return s.f.Close()
}

// normalizeXLSXValue dereferences pointers and maps nil to "" so excelize gets a
// concrete value. Concrete scalars (string, numbers, bool, time.Time) pass
// through untouched so the cell keeps its native type.
func normalizeXLSXValue(v any) any {
	switch t := v.(type) {
	case nil:
		return ""
	case *string:
		if t == nil {
			return ""
		}
		return *t
	default:
		return v
	}
}
