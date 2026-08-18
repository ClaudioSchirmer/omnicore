package export

import (
	"bytes"
	"errors"
	"testing"
	"time"
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
		// time.Time renders RFC3339 — same shape as the JSON surface, so a
		// date exports identically on every read path.
		{time.Date(2015, 3, 10, 0, 0, 0, 0, time.UTC), "2015-03-10T00:00:00Z"},
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
	plan := planOf[exportUserResponse](t)
	items := []exportUserResponse{
		{Name: "John", Addresses: []exportAddressResponse{{City: "NYC"}}},
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
	if err := Generate(&Plan{Root: nil}, nil, idLabel, &captureSink{}); err != nil {
		t.Fatalf("nil root must be a no-op, got %v", err)
	}
}

func TestGenerate_NonSliceItemsIsANoOp(t *testing.T) {
	plan := planOf[exportFlatResponse](t)
	sink := &captureSink{}
	if err := Generate(plan, "not-a-slice", idLabel, sink); err != nil {
		t.Fatalf("non-slice items must be a no-op, got %v", err)
	}
	if len(sink.rows) != 0 {
		t.Fatalf("expected no rows for non-slice items, got %+v", sink.rows)
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

// A Row with a negative Depth produces an invalid anchor column (Depth+1 <= 0),
// so excelize.CoordinatesToCellName rejects it — directly exercising Write's
// error branch (otherwise a defensive guard, since Generate never emits a
// negative Depth).
func TestXLSXSink_WriteInvalidDepthReturnsError(t *testing.T) {
	var buf bytes.Buffer
	sink, err := XLSX().Open(&buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sink.Write(Row{Depth: -1, Cells: []Cell{{Value: "x"}}}); err == nil {
		t.Fatal("expected Write to reject a negative-Depth anchor coordinate")
	}
}

// Open must surface SetSheetName's error when the configured sheet name is
// invalid (excelize rejects names containing the reserved characters
// : \ / ? * [ ]). The temporary excelize.File is closed and the error returned
// rather than yielding a half-built Sink.
func TestXLSX_OpenInvalidSheetNameReturnsError(t *testing.T) {
	var buf bytes.Buffer
	_, err := XLSX(WithSheetName("bad:name?")).Open(&buf)
	if err == nil {
		t.Fatal("expected Open to fail for an invalid worksheet name")
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
