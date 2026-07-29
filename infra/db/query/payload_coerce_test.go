package query

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// coercePayloadValue is the payload-direct path's type bridge: the outbox JSON
// arrives as strings/json.Number and the document must carry the shapes the
// guards and readers compare against. Every arm is a wire contract.
func TestCoercePayloadValue_AllArms(t *testing.T) {
	num := json.Number("42")
	frac := json.Number("4.5")

	if got := coercePayloadValue(nil, reflect.TypeOf("")); got != nil {
		t.Errorf("nil passes through, got %v", got)
	}
	// Schema-unknown: json.Number normalizes int-vs-float.
	if got := coercePayloadValue(num, nil); got != int64(42) {
		t.Errorf("unknown-type integral = %v (%T), want int64", got, got)
	}
	if got := coercePayloadValue(frac, nil); got != 4.5 {
		t.Errorf("unknown-type fractional = %v, want 4.5", got)
	}
	if got := coercePayloadValue("keep", nil); got != "keep" {
		t.Errorf("unknown-type non-number passes through, got %v", got)
	}
	// time.Time: RFC3339Nano strings parse to UTC; junk passes through.
	ts := coercePayloadValue("2026-07-29T10:00:00.5Z", reflect.TypeOf(time.Time{}))
	if parsed, ok := ts.(time.Time); !ok || parsed.Location() != time.UTC {
		t.Errorf("timestamp must parse to UTC time.Time, got %T %v", ts, ts)
	}
	if got := coercePayloadValue("not-a-time", reflect.TypeOf(time.Time{})); got != "not-a-time" {
		t.Errorf("unparseable time passes through, got %v", got)
	}
	// Pointer types coerce through their element type.
	if got := coercePayloadValue(num, reflect.TypeOf((*int64)(nil))); got != int64(42) {
		t.Errorf("*int64 = %v (%T), want int64 42", got, got)
	}
	// Strings and domain.ID stay canonical strings.
	if got := coercePayloadValue("abc", reflect.TypeOf(domain.ID{})); got != "abc" {
		t.Errorf("domain.ID stays the canonical string, got %v", got)
	}
	if got := coercePayloadValue(true, reflect.TypeOf(false)); got != true {
		t.Errorf("bool passthrough, got %v", got)
	}
	// Numeric kinds convert from json.Number; junk passes through.
	if got := coercePayloadValue(num, reflect.TypeOf(int32(0))); got != int64(42) {
		t.Errorf("int kind = %v (%T), want int64", got, got)
	}
	if got := coercePayloadValue("nan", reflect.TypeOf(int32(0))); got != "nan" {
		t.Errorf("non-number into int passes through, got %v", got)
	}
	if got := coercePayloadValue(frac, reflect.TypeOf(float64(0))); got != 4.5 {
		t.Errorf("float kind = %v, want 4.5", got)
	}
	// json.RawMessage re-renders the inline fragment.
	raw := coercePayloadValue(map[string]any{"a": 1}, reflect.TypeOf(json.RawMessage(nil)))
	if b, ok := raw.([]byte); !ok || string(b) != `{"a":1}` {
		t.Errorf("RawMessage must re-render, got %T %v", raw, raw)
	}
	// []byte decodes the base64 wire convention; junk passes through.
	if got := coercePayloadValue("aGk=", reflect.TypeOf([]byte(nil))); string(got.([]byte)) != "hi" {
		t.Errorf("[]byte must base64-decode, got %v", got)
	}
	if got := coercePayloadValue("!!!", reflect.TypeOf([]byte(nil))); got != "!!!" {
		t.Errorf("bad base64 passes through, got %v", got)
	}
	// The default arm: anything else untouched.
	if got := coercePayloadValue("x", reflect.TypeOf(struct{}{})); got != "x" {
		t.Errorf("default arm passthrough, got %v", got)
	}
}

func TestNormalizeNumber(t *testing.T) {
	if got := normalizeNumber(json.Number("7")); got != int64(7) {
		t.Errorf("integral = %v (%T)", got, got)
	}
	if got := normalizeNumber(json.Number("7.5")); got != 7.5 {
		t.Errorf("fractional = %v", got)
	}
	if got := normalizeNumber(json.Number("1e3")); got != 1000.0 {
		t.Errorf("exponent renders as float, got %v", got)
	}
	// Beyond-int64 integral falls to float; unparseable falls to the raw string.
	if got := normalizeNumber(json.Number("92233720368547758080")); got != 9.223372036854776e+19 {
		t.Errorf("overflow integral = %v (%T)", got, got)
	}
	if got := normalizeNumber(json.Number("not-a-number")); got != "not-a-number" {
		t.Errorf("junk falls back to the raw string, got %v", got)
	}
}
