package export

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"
)

// CSVOption configures the CSV encoder at mount time.
type CSVOption func(*csvEncoder)

// WithDelimiter sets the CSV field separator (default ','). Common alternatives
// are ';' (locales where comma is the decimal separator) and '\t' (TSV).
func WithDelimiter(r rune) CSVOption {
	return func(e *csvEncoder) { e.delimiter = r }
}

// CSV builds the CSV Encoder. Options (e.g. WithDelimiter) are encoder-specific
// serialization choices, fixed per route at mount time.
func CSV(opts ...CSVOption) Encoder {
	e := &csvEncoder{delimiter: ','}
	for _, o := range opts {
		o(e)
	}
	return e
}

type csvEncoder struct {
	delimiter rune
}

func (e *csvEncoder) ContentType() string { return "text/csv; charset=utf-8" }
func (e *csvEncoder) Extension() string   { return "csv" }

func (e *csvEncoder) Open(w io.Writer) (Sink, error) {
	cw := csv.NewWriter(w)
	if e.delimiter != 0 {
		cw.Comma = e.delimiter
	}
	return &csvSink{w: cw}, nil
}

type csvSink struct {
	w *csv.Writer
}

func (s *csvSink) Write(r Row) error {
	rec := make([]string, 0, r.Depth+len(r.Cells))
	for i := 0; i < r.Depth; i++ {
		rec = append(rec, "")
	}
	for _, c := range r.Cells {
		rec = append(rec, stringifyCell(c.Value))
	}
	return s.w.Write(rec)
}

func (s *csvSink) Close() error {
	s.w.Flush()
	return s.w.Error()
}

// stringifyCell renders a cell value as text. Pointers are dereferenced (nil →
// empty); time.Time renders RFC3339 (matching the JSON surface, so a date
// exports the same on every read path); fmt.Stringer is honored; numbers/bools
// format without scientific notation surprises; everything else falls back to
// fmt.Sprintf("%v").
func stringifyCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case *string:
		if t == nil {
			return ""
		}
		return *t
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}
