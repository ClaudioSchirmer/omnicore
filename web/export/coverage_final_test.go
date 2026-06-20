package export

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// stringifyCell remaining branches: int32, float32, fmt.Stringer, default.
type stringerCell struct{}

func (stringerCell) String() string { return "S" }

func TestStringifyCell_RemainingBranches(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int32(9), "9"},
		{float32(2.5), "2.5"},
		{stringerCell{}, "S"},
		{struct{ X int }{X: 1}, "{1}"}, // default fmt.Sprintf
		{[]int{1, 2}, "[1 2]"},         // default
	}
	for _, c := range cases {
		if got := stringifyCell(c.in); got != c.want {
			t.Errorf("stringifyCell(%v)=%q want %q", c.in, got, c.want)
		}
	}
}

// errSink fails on the first Write so the emitter's err-guarded short-circuits
// (write early-return and separator early-return) are exercised.
type errSink struct{ writes int }

func (s *errSink) Write(r Row) error { s.writes++; return errors.New("sink boom") }
func (s *errSink) Close() error      { return nil }

func TestGenerate_PropagatesSinkErrorAndShortCircuits(t *testing.T) {
	plan := &queries.ExportPlan{Root: &queries.ExportNode{
		Columns: []queries.ExportColumn{{GoField: "Name", WireLeaf: "name"}},
		Children: []*queries.ExportNode{{
			GoSegment: "Addresses", WireSegment: "addresses",
			Columns: []queries.ExportColumn{{GoField: "City", WireLeaf: "city"}},
		}},
	}}
	items := []map[string]any{
		{"Name": "John", "Addresses": []map[string]any{{"City": "NYC"}}},
	}
	sink := &errSink{}
	err := Generate(plan, items, idLabel, sink)
	if err == nil {
		t.Fatal("expected the sink error to propagate out of Generate")
	}
	// After the first failed Write, every subsequent write/separator must
	// short-circuit, so the sink is hit exactly once.
	if sink.writes != 1 {
		t.Fatalf("expected exactly one Write attempt after error, got %d", sink.writes)
	}
}

func TestGenerate_NilPlanAndNilRoot(t *testing.T) {
	if err := Generate(nil, nil, idLabel, &captureSink{}); err != nil {
		t.Fatalf("nil plan must be a no-op, got %v", err)
	}
	if err := Generate(&queries.ExportPlan{Root: nil}, nil, idLabel, &captureSink{}); err != nil {
		t.Fatalf("nil root must be a no-op, got %v", err)
	}
}

// errWriter fails every write so xlsxSink.Close's f.Write(w) error branch runs.
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("writer boom") }

func TestXLSXSink_CloseWriteError(t *testing.T) {
	sink, err := XLSX().Open(errWriter{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sink.Write(Row{Cells: []Cell{{Value: "A"}}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sink.Close(); err == nil {
		t.Fatal("expected Close to surface the underlying writer error")
	}
}

// Writing a row whose anchor coordinate is invalid is not reachable through the
// public API (Depth is always >= 0 and row counter increments), so the
// CoordinatesToCellName error branch in Write stays as a defensive guard.
func TestXLSXSink_WriteHeaderPointerValue(t *testing.T) {
	var buf bytes.Buffer
	sink, _ := XLSX().Open(&buf)
	s := "h"
	// header row with a *string cell exercises normalize within the header branch
	if err := sink.Write(Row{Header: true, Cells: []Cell{{Value: &s}}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
