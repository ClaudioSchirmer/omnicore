package openapi

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// ─── Scalars ──────────────────────────────────────────────────────────────

func TestParseExampleTag_String(t *testing.T) {
	got, err := parseExampleTag(reflect.TypeOf(""), "Alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Alice" {
		t.Fatalf("got %v, want %q", got, "Alice")
	}
}

func TestParseExampleTag_Bool(t *testing.T) {
	got, err := parseExampleTag(reflect.TypeOf(false), "true")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != true {
		t.Fatalf("got %v, want true", got)
	}
}

func TestParseExampleTag_IntFamily(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		raw  string
		want any
	}{
		{"int", reflect.TypeOf(int(0)), "42", int32(42)},
		{"int8", reflect.TypeOf(int8(0)), "-5", int32(-5)},
		{"int32", reflect.TypeOf(int32(0)), "100", int32(100)},
		{"int64", reflect.TypeOf(int64(0)), "9000000000", int64(9000000000)},
		{"uint", reflect.TypeOf(uint(0)), "7", uint32(7)},
		{"uint8", reflect.TypeOf(uint8(0)), "200", uint32(200)},
		{"uint64", reflect.TypeOf(uint64(0)), "18000000000", uint64(18000000000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseExampleTag(c.typ, c.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %v (%T), want %v (%T)", got, got, c.want, c.want)
			}
		})
	}
}

func TestParseExampleTag_FloatFamily(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		raw  string
		want any
	}{
		{"float32", reflect.TypeOf(float32(0)), "3.14", float32(3.14)},
		{"float64", reflect.TypeOf(float64(0)), "2.718281828", float64(2.718281828)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseExampleTag(c.typ, c.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %v (%T), want %v (%T)", got, got, c.want, c.want)
			}
		})
	}
}

// ─── Well-known types ─────────────────────────────────────────────────────

func TestParseExampleTag_UUID(t *testing.T) {
	raw := "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"
	got, err := parseExampleTag(reflect.TypeOf(uuid.UUID{}), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := got.(string)
	if !ok || s != raw {
		t.Fatalf("got %v (%T), want canonical uuid string", got, got)
	}
}

func TestParseExampleTag_DomainID(t *testing.T) {
	raw := "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"
	got, err := parseExampleTag(reflect.TypeOf(domain.ID{}), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := got.(string)
	if !ok || s != raw {
		t.Fatalf("got %v (%T), want canonical uuid string", got, got)
	}
}

func TestParseExampleTag_Time(t *testing.T) {
	raw := "2026-06-08T13:45:00Z"
	got, err := parseExampleTag(reflect.TypeOf(time.Time{}), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != raw {
		t.Fatalf("got %v, want %q (RFC3339 string preserved verbatim)", got, raw)
	}
}

// ─── Pointer recursion ────────────────────────────────────────────────────

func TestParseExampleTag_PointerUnwraps(t *testing.T) {
	got, err := parseExampleTag(reflect.TypeOf((*string)(nil)), "Bob")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Bob" {
		t.Fatalf("got %v, want %q", got, "Bob")
	}
}

func TestParseExampleTag_PointerToInt(t *testing.T) {
	got, err := parseExampleTag(reflect.TypeOf((*int32)(nil)), "12")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != int32(12) {
		t.Fatalf("got %v (%T), want int32(12)", got, got)
	}
}

// ─── Failure modes ────────────────────────────────────────────────────────

func TestParseExampleTag_BadInt(t *testing.T) {
	_, err := parseExampleTag(reflect.TypeOf(int(0)), "not-a-number")
	if err == nil {
		t.Fatal("expected error parsing non-numeric int")
	}
}

func TestParseExampleTag_BadBool(t *testing.T) {
	_, err := parseExampleTag(reflect.TypeOf(false), "maybe")
	if err == nil {
		t.Fatal("expected error parsing non-bool")
	}
}

func TestParseExampleTag_BadFloat(t *testing.T) {
	_, err := parseExampleTag(reflect.TypeOf(float64(0)), "pi")
	if err == nil {
		t.Fatal("expected error parsing non-float")
	}
}

func TestParseExampleTag_BadUUID(t *testing.T) {
	_, err := parseExampleTag(reflect.TypeOf(uuid.UUID{}), "not-a-uuid")
	if err == nil {
		t.Fatal("expected error parsing non-uuid")
	}
}

func TestParseExampleTag_BadTime(t *testing.T) {
	_, err := parseExampleTag(reflect.TypeOf(time.Time{}), "yesterday")
	if err == nil {
		t.Fatal("expected error parsing non-RFC3339 timestamp")
	}
}

func TestParseExampleTag_CompositeRejected(t *testing.T) {
	type inner struct {
		X int
	}
	cases := []struct {
		name string
		typ  reflect.Type
	}{
		{"struct", reflect.TypeOf(inner{})},
		{"slice", reflect.TypeOf([]string{})},
		{"map", reflect.TypeOf(map[string]int{})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseExampleTag(c.typ, "anything")
			if err == nil {
				t.Fatalf("%s should be rejected with a guidance message", c.name)
			}
			if !strings.Contains(err.Error(), "Doc.RequestExamples") {
				t.Fatalf("error should point the consumer to the map-based path, got: %v", err)
			}
		})
	}
}

func TestParseExampleTag_NilType(t *testing.T) {
	_, err := parseExampleTag(nil, "x")
	if err == nil {
		t.Fatal("nil type should error")
	}
}
