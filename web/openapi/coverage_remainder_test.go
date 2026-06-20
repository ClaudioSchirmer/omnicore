package openapi

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ─── errorEnvelopeExample — fallback for an out-of-registry status ──────────

func TestErrorEnvelopeExample_UnknownStatusFallback(t *testing.T) {
	// 418 has no DefaultErrorExample → the fallback envelope is built with the
	// Internal Server Error notification key.
	env := errorEnvelopeExample(418)
	if env == nil {
		t.Fatal("expected a fallback envelope for an unknown status")
	}
	errsAny, ok := env["errors"]
	if !ok {
		t.Fatalf("fallback envelope must carry an errors block, got %#v", env)
	}
	_ = errsAny
}

// ─── isResponseNone — a foreign "None" type is NOT the framework's ──────────

type None struct{} // same name, wrong package path

func TestIsResponseNone_ForeignNoneRejected(t *testing.T) {
	if isResponseNone(reflect.TypeOf(None{})) {
		t.Error("a consumer-defined None must NOT trigger the data-omission branch")
	}
	if isResponseNone(nil) != true {
		t.Error("nil response type should be treated as None")
	}
}

// ─── successWrapper — None (no data) + Paged (pagination) branches ──────────

func TestSuccessWrapper_NoneOmitsData(t *testing.T) {
	wrap := successWrapper(201, RouteSpec{ResponseType: nil})
	out, _ := wrap(json.RawMessage(`{"id":"x"}`)).(map[string]any)
	if _, hasData := out["data"]; hasData {
		t.Errorf("None response must omit data, got %#v", out)
	}
	if out["success"] != true || out["status"] != 201 {
		t.Errorf("envelope scalars wrong: %#v", out)
	}
}

func TestSuccessWrapper_PagedAddsPagination(t *testing.T) {
	wrap := successWrapper(200, RouteSpec{ResponseType: reflect.TypeOf(struct {
		ID string `json:"id"`
	}{}), Paged: true})
	out, _ := wrap(json.RawMessage(`[{"id":"x"}]`)).(map[string]any)
	if _, hasData := out["data"]; !hasData {
		t.Errorf("paged response must carry data, got %#v", out)
	}
	if _, hasPag := out["pagination"]; !hasPag {
		t.Errorf("paged response must carry pagination, got %#v", out)
	}
}

// ─── buildExamplesMap — wrap-nil path, summary/desc, nil-Raw skip ───────────

func TestBuildExamplesMap_WrapNilAndSkips(t *testing.T) {
	declared := map[string]rawExample{
		"full":    {Summary: "s", Description: "d", Raw: json.RawMessage(`{"a":1}`)},
		"removed": {Raw: nil}, // consumer-removal entry → skipped
	}
	out := buildExamplesMap(declared, nil)
	entry, ok := out["full"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'full' entry, got %#v", out)
	}
	if entry["summary"] != "s" || entry["description"] != "d" {
		t.Errorf("summary/description not carried: %#v", entry)
	}
	if entry["value"] == nil {
		t.Errorf("expected decoded value, got nil")
	}
	if _, present := out["removed"]; present {
		t.Errorf("nil-Raw entry must be skipped, got %#v", out)
	}
}

func TestBuildExamplesMap_EmptyReturnsNil(t *testing.T) {
	if got := buildExamplesMap(nil, nil); got != nil {
		t.Errorf("empty declared map should yield nil, got %#v", got)
	}
	// All entries removed → nil too.
	allRemoved := map[string]rawExample{"x": {Raw: nil}}
	if got := buildExamplesMap(allRemoved, nil); got != nil {
		t.Errorf("all-removed map should yield nil, got %#v", got)
	}
}

// ─── validateExample — []byte branch + invalid-JSON branch ──────────────────

func TestValidateExample_ByteSliceBranch(t *testing.T) {
	raw, err := validateExample([]byte(`{"a":1}`), nil, false)
	if err != nil {
		t.Fatalf("[]byte value should validate: %v", err)
	}
	if string(raw) != `{"a":1}` {
		t.Errorf("expected verbatim bytes, got %s", raw)
	}
}

func TestValidateExample_InvalidJSONRejected(t *testing.T) {
	_, err := validateExample(json.RawMessage(`{not valid`), nil, false)
	if err == nil {
		t.Fatal("expected error for structurally invalid JSON")
	}
}

func TestValidateExample_NilValueIsNoOp(t *testing.T) {
	raw, err := validateExample(nil, nil, false)
	if err != nil || raw != nil {
		t.Fatalf("nil value should yield (nil, nil), got (%s, %v)", raw, err)
	}
}

// ─── jsonValue — best-effort fallback on malformed bytes ────────────────────

func TestJSONValue_FallbackOnMalformed(t *testing.T) {
	// validateExample normally guarantees valid JSON; calling jsonValue
	// directly with garbage exercises the defensive string fallback.
	got := jsonValue(json.RawMessage("not-json"))
	if got != "not-json" {
		t.Errorf("expected raw-string fallback, got %v", got)
	}
}

func TestJSONValue_DecodesObject(t *testing.T) {
	got := jsonValue(json.RawMessage(`{"k":"v"}`))
	m, ok := got.(map[string]any)
	if !ok || m["k"] != "v" {
		t.Errorf("expected decoded object, got %#v", got)
	}
}

// ─── typeLabel — named-without-pkg + anonymous-kind branches ────────────────

func TestTypeLabel_Branches(t *testing.T) {
	// predeclared `int` — has a Name, no PkgPath → returns the bare name.
	if got := typeLabel(reflect.TypeOf(0)); got != "int" {
		t.Errorf("predeclared int → %q, want int", got)
	}
	// anonymous struct — no Name → falls back to the Kind.
	if got := typeLabel(reflect.TypeOf(struct{ X int }{})); got != "struct" {
		t.Errorf("anonymous struct → %q, want struct", got)
	}
}

// ─── schema build — Array + bare Interface arms ─────────────────────────────

func TestGenerate_FixedArrayIsArraySchema(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf([3]int{}))
	if s.Type != "array" {
		t.Fatalf("fixed array → type %q, want array", s.Type)
	}
	if s.Items == nil || s.Items.Type != "integer" {
		t.Errorf("array items should be integer, got %#v", s.Items)
	}
}

func TestGenerate_BareInterfaceIsEmptySchema(t *testing.T) {
	g := NewGenerator(nil)
	var iface any
	s := g.Generate(reflect.TypeOf(&iface).Elem())
	if s.Type != "" || s.Ref != "" {
		t.Errorf("bare interface should be an empty/any schema, got %#v", s)
	}
}

// ─── parseExampleTag — remaining bad-conversion arms ────────────────────────

func TestParseExampleTag_RemainingBadConversions(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		raw  string
	}{
		{"int64", reflect.TypeOf(int64(0)), "x"},
		{"uint", reflect.TypeOf(uint(0)), "x"},
		{"uint64", reflect.TypeOf(uint64(0)), "x"},
		{"float32", reflect.TypeOf(float32(0)), "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseExampleTag(tc.typ, tc.raw); err == nil {
				t.Fatalf("expected conversion error for %s value %q", tc.name, tc.raw)
			}
		})
	}
}
