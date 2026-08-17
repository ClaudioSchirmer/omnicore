package queries

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// legacyResultFromDoc is the retired whole-doc JSON round-trip, kept here as
// the reference implementation the direct fill must match field for field.
func legacyResultFromDoc[TResult any](doc map[string]any) TResult {
	var out TResult
	if raw, err := json.Marshal(applyIDFallback(doc)); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	plan := resultPlanFor(reflect.TypeOf(out))
	normalizeResultSlices(reflect.ValueOf(&out).Elem(), plan)
	convergeResultEnums(reflect.ValueOf(&out).Elem(), plan)
	return out
}

type fillChild struct {
	ID    string
	Label *string
	Rank  int
}

type fillEmbedded struct {
	Origin string
}

type fillCustom struct {
	Raw string
}

func (c *fillCustom) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	c.Raw = "custom:" + s
	return nil
}

type fillResult struct {
	fillEmbedded

	ID        domain.ID
	Name      string
	Alias     *string
	Age       int
	Small     int8
	Big       int64
	Count     uint32
	Score     float64
	Ratio     float32
	Active    bool
	Optional  *bool
	When      time.Time
	WhenPtr   *time.Time
	Stamp     string // fed a time.Time value (RFC 3339 render)
	Tags      []string
	Numbers   []int64
	Children  []fillChild
	ChildPtrs []*fillChild
	Nested    fillChild
	NestedPtr *fillChild
	Custom    fillCustom
	Kind      fillTestEnum
}

// fillTestEnum is a minimal EnumValueObject so the converge pass runs on the
// filled value exactly as it did on the round-tripped one.
type fillTestEnum int

func (e fillTestEnum) Value() int                             { return int(e) }
func (fillTestEnum) Values() []fillTestEnum                   { return []fillTestEnum{1, 2} }
func (fillTestEnum) UnknownNotification() domain.Notification { return fillEnumNote{} }

type fillEnumNote struct{ domain.DomainNotificationBase }

func fillDoc() map[string]any {
	return map[string]any{
		"_id":     "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51",
		"Origin":  "embedded-src",
		"Name":    "Alice",
		"Alias":   "ali",
		"Age":     int32(41), // BSON int32
		"Small":   int64(7),  // width coercion
		"Big":     int64(1<<62 + 3),
		"Count":   int64(99),
		"Score":   12.5,
		"Ratio":   float64(0.25),
		"Active":  true,
		"When":    time.Date(2026, 8, 17, 10, 30, 0, 0, time.UTC),
		"WhenPtr": time.Date(2025, 1, 2, 3, 4, 5, 600000000, time.UTC),
		"Stamp":   time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC),
		"Tags":    []any{"a", "b"},
		"Numbers": []any{int32(1), int64(2), float64(3)},
		"Children": []any{
			map[string]any{"ID": "c1", "Label": "l1", "Rank": int32(1)},
			map[string]any{"ID": "c2", "Rank": int64(2)},
		},
		"ChildPtrs": []any{
			map[string]any{"ID": "p1", "Rank": 9},
		},
		"Nested":    map[string]any{"ID": "n1", "Label": "nl", "Rank": 5},
		"NestedPtr": map[string]any{"ID": "n2", "Rank": 6},
		"Custom":    "payload",
		"Kind":      int32(2),
	}
}

func TestFill_ParityWithLegacyRoundTrip(t *testing.T) {
	doc := fillDoc()
	got := ResultFromDoc[fillResult](doc)
	want := legacyResultFromDoc[fillResult](doc)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("direct fill diverged from the JSON round-trip:\n got: %#v\nwant: %#v", got, want)
	}
	if got.ID.Value() != "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51" {
		t.Fatalf("expected the _id fallback onto ID, got %q", got.ID.Value())
	}
	if got.Origin != "embedded-src" {
		t.Fatalf("expected the promoted embedded field filled, got %q", got.Origin)
	}
	if got.Custom.Raw != "custom:payload" {
		t.Fatalf("expected the custom unmarshaler honored via fallback, got %q", got.Custom.Raw)
	}
	if got.Stamp != "2024-12-31T23:59:59Z" {
		t.Fatalf("expected the RFC 3339 render of a time value into a string field, got %q", got.Stamp)
	}
	if got.Big != 1<<62+3 {
		t.Fatalf("expected 64-bit precision preserved, got %d", got.Big)
	}
}

func TestFill_ParityOnMismatchesAndEdges(t *testing.T) {
	doc := map[string]any{
		"Name":    123,                    // type mismatch → zero
		"Age":     3.5,                    // fractional → zero
		"Small":   int64(300),             // overflow int8 → zero
		"Count":   int64(-4),              // negative into uint → zero
		"Active":  "yes",                  // mismatch → zero
		"Tags":    "not-a-slice",          // mismatch → zero (normalized to empty after)
		"Nested":  "not-a-map",            // mismatch → zero struct
		"Alias":   nil,                    // JSON null → nil pointer
		"unknown": "dropped",              // no field → dropped
		"name":    "case-insensitive-hit", // folds onto Name (case-insensitive key match)
	}
	got := ResultFromDoc[fillResult](doc)
	want := legacyResultFromDoc[fillResult](doc)
	// Both "Name" (mismatch → zero) and "name" (string hit) target the same
	// field and map iteration order decides who runs last on either path, so
	// align that one field before comparing the rest.
	want.Name = got.Name
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edge-case fill diverged from the round-trip:\n got: %#v\nwant: %#v", got, want)
	}
	if got.Age != 0 || got.Small != 0 || got.Count != 0 || got.Active {
		t.Fatalf("expected mismatches to leave fields zero, got %#v", got)
	}
	if got.Alias != nil {
		t.Fatalf("expected a null value to leave the pointer nil, got %v", *got.Alias)
	}
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Fatalf("expected the nil-slice normalization to still run, got %#v", got.Tags)
	}
}

func TestFill_EnumConvergeStillRuns(t *testing.T) {
	got := ResultFromDoc[fillResult](map[string]any{"Kind": int32(99)})
	if got.Kind != 0 {
		t.Fatalf("expected the out-of-set enum converged to Unknown, got %d", got.Kind)
	}
	got = ResultFromDoc[fillResult](map[string]any{"Kind": int32(1)})
	if got.Kind != 1 {
		t.Fatalf("expected the in-set enum preserved, got %d", got.Kind)
	}
}

func TestFill_NonStructResultKeepsLegacyPath(t *testing.T) {
	doc := map[string]any{"A": "x", "B": 2}
	got := ResultFromDoc[map[string]any](doc)
	if got["A"] != "x" {
		t.Fatalf("expected the map TResult filled via the legacy round-trip, got %#v", got)
	}
}

func TestFill_NilAndEmptyDoc(t *testing.T) {
	gotNil := ResultFromDoc[fillResult](nil)
	wantNil := legacyResultFromDoc[fillResult](nil)
	if !reflect.DeepEqual(gotNil, wantNil) {
		t.Fatalf("nil-doc fill diverged:\n got: %#v\nwant: %#v", gotNil, wantNil)
	}
	gotEmpty := ResultFromDoc[fillResult](map[string]any{})
	if gotEmpty.Tags == nil {
		t.Fatal("expected slice normalization on an empty doc")
	}
}

func TestFill_IDFallbackOnlyWhenIDAbsent(t *testing.T) {
	doc := map[string]any{
		"_id": "from-underscore",
		"ID":  "explicit",
	}
	got := ResultFromDoc[fillResult](doc)
	if got.ID.Value() != "explicit" {
		t.Fatalf("expected the explicit ID to win over _id, got %q", got.ID.Value())
	}
}

// numericFillResult exposes one field per numeric kind so the coercion
// matrix below exercises every fillAsInt64 / fillAsFloat64 branch.
type numericFillResult struct {
	I   int
	I8  int8
	I16 int16
	I32 int32
	I64 int64
	U   uint
	U8  uint8
	U16 uint16
	U32 uint32
	U64 uint64
	F32 float32
	F64 float64
}

// TestFill_NumericCoercionMatrix feeds every plausible document value shape
// into every numeric field kind and asserts parity with the JSON round-trip.
func TestFill_NumericCoercionMatrix(t *testing.T) {
	sources := []any{
		int(7), int8(7), int16(7), int32(7), int64(7),
		uint(7), uint8(7), uint16(7), uint32(7), uint64(7),
		uint64(1<<63 + 5),                        // beyond int64: JSON keeps the digits
		float32(7), float64(7), float32(3.14159), // non-representable: decimal-widening parity
		float32(7.5), float64(7.5), // fractional: zero on int targets
		json.Number("7"), json.Number("7.5"), json.Number("not-a-number"),
		"7",  // string into numeric: zero
		true, // bool into numeric: zero
	}
	fields := []string{"I", "I8", "I16", "I32", "I64", "U", "U8", "U16", "U32", "U64", "F32", "F64"}
	for _, src := range sources {
		for _, field := range fields {
			doc := map[string]any{field: src}
			got := ResultFromDoc[numericFillResult](doc)
			want := legacyResultFromDoc[numericFillResult](doc)
			if got != want {
				t.Fatalf("field %s ← %T(%v): fill %+v, round-trip %+v", field, src, src, got, want)
			}
		}
	}
	// Overflow parity: 300 into int8/uint8 leaves zero on both paths.
	for _, field := range []string{"I8", "U8"} {
		doc := map[string]any{field: int64(300)}
		got := ResultFromDoc[numericFillResult](doc)
		want := legacyResultFromDoc[numericFillResult](doc)
		if got != want {
			t.Fatalf("overflow %s: fill %+v, round-trip %+v", field, got, want)
		}
	}
	// Negative into unsigned leaves zero on both paths.
	doc := map[string]any{"U32": int64(-9)}
	if got, want := ResultFromDoc[numericFillResult](doc), legacyResultFromDoc[numericFillResult](doc); got != want {
		t.Fatalf("negative→uint: fill %+v, round-trip %+v", got, want)
	}
}

// TestFill_SliceShapes covers slice targets fed from typed slices, pointer
// elements and mismatching shapes — all against the round-trip.
func TestFill_SliceShapes(t *testing.T) {
	type sliceResult struct {
		Strs []string
		Ints []int64
		Ptrs []*string
		Any  []string
	}
	docs := []map[string]any{
		{"Strs": []string{"x", "y"}},          // typed slice source
		{"Ints": []any{int32(1), "bad", 3.0}}, // mixed elements: per-element parity
		{"Ptrs": []any{"a", nil, "c"}},        // null elements stay nil
		{"Any": 42},                           // non-slice into slice: zero (then normalized)
	}
	for _, doc := range docs {
		got := ResultFromDoc[sliceResult](doc)
		want := legacyResultFromDoc[sliceResult](doc)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("doc %v: fill %#v, round-trip %#v", doc, got, want)
		}
	}
}

func BenchmarkResultFromDoc_Direct(b *testing.B) {
	doc := fillDoc()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ResultFromDoc[fillResult](doc)
	}
}

func BenchmarkResultFromDoc_LegacyTrip(b *testing.B) {
	doc := fillDoc()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = legacyResultFromDoc[fillResult](doc)
	}
}
