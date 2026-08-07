package grpc

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
)

// bind_scalars_test.go — the bridge PER PROTO SCALAR KIND, both directions.
//
// protojson and encoding/json speak the same JSON for most kinds and
// disagree on exactly one: the proto3 JSON mapping renders 64-bit integers
// (int64/sint64/sfixed64/uint64/fixed64) as QUOTED strings, while
// encoding/json demands a bare number for a numeric Go field. Without the
// plan's unquote step, a money DTO (`PriceCents int64`) that REST binds fine
// fails EVERY gRPC request with a SchemaViolation, before the command
// handler is ever reached.

// scalarChildDTO deliberately leaves ChildBig untagged: its json key
// ("ChildBig") differs from the wire key ("childBig"), so the child plan
// carries a rename AND an unquote on the same field.
type scalarChildDTO struct {
	ChildBig   int64
	ChildLabel string `json:"childLabel"`
}

type scalarMatrixDTO struct {
	Big              int64            `json:"big"`
	SignedBig        int64            `json:"signedBig"`
	FixedSignedBig   int64            `json:"fixedSignedBig"`
	UnsignedBig      uint64           `json:"unsignedBig"`
	FixedUnsignedBig uint64           `json:"fixedUnsignedBig"`
	Small            int32            `json:"small"`
	UnsignedSmall    uint32           `json:"unsignedSmall"`
	Ratio            float64          `json:"ratio"`
	Fraction         float32          `json:"fraction"`
	Flag             bool             `json:"flag"`
	Label            string           `json:"label"`
	Blob             []byte           `json:"blob"`
	Flavor           string           `json:"flavor"`
	When             time.Time        `json:"when"`
	Bigs             []int64          `json:"bigs"`
	Labels           []string         `json:"labels"`
	Child            scalarChildDTO   `json:"child"`
	Children         []scalarChildDTO `json:"children"`
	MaybeBig         *int64           `json:"maybeBig"`
}

func scalarMatrixPlans(t *testing.T) (req, resp *bindPlan) {
	t.Helper()
	md := (&testpb.ScalarMatrix{}).ProtoReflect().Descriptor()
	dt := reflect.TypeOf(scalarMatrixDTO{})
	req, err := compileBindPlan("t", "request", md, dt, nil, nil)
	if err != nil {
		t.Fatalf("compile request plan: %v", err)
	}
	resp, err = compileBindPlan("t", "response", md, dt, nil, nil)
	if err != nil {
		t.Fatalf("compile response plan: %v", err)
	}
	return req, resp
}

func TestBridge_EveryScalarKind_BothDirections(t *testing.T) {
	when := time.Date(2026, 8, 6, 12, 30, 0, 0, time.UTC)
	maybe := int64(-9007199254740993) // past float64's exact range, negative
	msg := &testpb.ScalarMatrix{
		Big:              1050,
		SignedBig:        -42,
		FixedSignedBig:   -7,
		UnsignedBig:      18446744073709551615, // math.MaxUint64
		FixedUnsignedBig: 4294967296,
		Small:            -3,
		UnsignedSmall:    9,
		Ratio:            2.5,
		Fraction:         0.5,
		Flag:             true,
		Label:            "widget",
		Blob:             []byte{0xfb, 0xff, 0xbf, 0x3e, 0x7d}, // base64 with + and /
		Flavor:           testpb.Flavor_FLAVOR_SALTY,
		When:             timestamppb.New(when),
		Bigs:             []int64{1, math.MaxInt64, math.MinInt64},
		Labels:           []string{"a", "b"},
		Child:            &testpb.ScalarChild{ChildBig: 9007199254740993, ChildLabel: "kid"},
		Children: []*testpb.ScalarChild{
			{ChildBig: 1, ChildLabel: "one"},
			{ChildBig: math.MaxInt64, ChildLabel: "two"},
		},
		MaybeBig: &maybe,
	}

	reqPlan, respPlan := scalarMatrixPlans(t)

	// pb → DTO: the direction the 64-bit quoting used to break.
	got, err := pbToDTO[scalarMatrixDTO](reqPlan, msg)
	if err != nil {
		t.Fatalf("pbToDTO: %v", err)
	}
	want := scalarMatrixDTO{
		Big:              1050,
		SignedBig:        -42,
		FixedSignedBig:   -7,
		UnsignedBig:      math.MaxUint64,
		FixedUnsignedBig: 4294967296,
		Small:            -3,
		UnsignedSmall:    9,
		Ratio:            2.5,
		Fraction:         0.5,
		Flag:             true,
		Label:            "widget",
		Blob:             []byte{0xfb, 0xff, 0xbf, 0x3e, 0x7d},
		Flavor:           "FLAVOR_SALTY",
		When:             when,
		Bigs:             []int64{1, math.MaxInt64, math.MinInt64},
		Labels:           []string{"a", "b"},
		Child:            scalarChildDTO{ChildBig: 9007199254740993, ChildLabel: "kid"},
		Children: []scalarChildDTO{
			{ChildBig: 1, ChildLabel: "one"},
			{ChildBig: math.MaxInt64, ChildLabel: "two"},
		},
		MaybeBig: &maybe,
	}
	if !got.When.Equal(want.When) {
		t.Errorf("When: got %v, want %v", got.When, want.When)
	}
	got.When, want.When = time.Time{}, time.Time{} // compared above; zero for DeepEqual
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pbToDTO mismatch:\n got %+v\nwant %+v", got, want)
	}

	// DTO → pb: the return leg must reproduce the message it came from.
	want.When = when
	back, err := dtoToPB[testpb.ScalarMatrix](respPlan, want)
	if err != nil {
		t.Fatalf("dtoToPB: %v", err)
	}
	if !proto.Equal(back, msg) {
		t.Errorf("dtoToPB round-trip mismatch:\n got %v\nwant %v", back, msg)
	}
}

// The exact-value guarantee: a 64-bit integer outside float64's exact range
// must survive the intermediate map untouched. The unquote step carries the
// digits as json.RawMessage precisely so nothing is routed through float64.
func TestBridge_Int64_ExactBeyondFloat64(t *testing.T) {
	reqPlan, respPlan := scalarMatrixPlans(t)
	for _, v := range []int64{math.MaxInt64, math.MinInt64, 9007199254740993, -9007199254740993} {
		msg := &testpb.ScalarMatrix{Big: v, Bigs: []int64{v}, Child: &testpb.ScalarChild{ChildBig: v}}
		got, err := pbToDTO[scalarMatrixDTO](reqPlan, msg)
		if err != nil {
			t.Fatalf("%d: pbToDTO: %v", v, err)
		}
		if got.Big != v || got.Bigs[0] != v || got.Child.ChildBig != v {
			t.Errorf("%d: lost precision — root %d, repeated %d, child %d",
				v, got.Big, got.Bigs[0], got.Child.ChildBig)
		}
		back, err := dtoToPB[testpb.ScalarMatrix](respPlan, got)
		if err != nil {
			t.Fatalf("%d: dtoToPB: %v", v, err)
		}
		if back.GetBig() != v || back.GetBigs()[0] != v || back.GetChild().GetChildBig() != v {
			t.Errorf("%d: lost precision on the way out: %v", v, back)
		}
	}
	// uint64 above MaxInt64 — the half a signed round-trip would corrupt.
	const huge = uint64(math.MaxUint64)
	got, err := pbToDTO[scalarMatrixDTO](reqPlan, &testpb.ScalarMatrix{UnsignedBig: huge})
	if err != nil {
		t.Fatalf("uint64: pbToDTO: %v", err)
	}
	if got.UnsignedBig != huge {
		t.Errorf("uint64: got %d, want %d", got.UnsignedBig, huge)
	}
}

// The outbound leg crosses the same intermediate map whenever the plan
// renames a key. Decoded the plain way, every number there becomes a
// float64 and math.MaxInt64 rounds to …776000 — so the rewrite must decode
// with UseNumber and keep the literal.
func TestBridge_Int64_OutboundRewriteKeepsLiteral(t *testing.T) {
	_, respPlan := scalarMatrixPlans(t)
	if !respPlan.rewritesWire() {
		t.Fatal("this plan must exercise the outbound rewrite")
	}
	dto := scalarMatrixDTO{
		Big:         math.MaxInt64,
		UnsignedBig: math.MaxUint64,
		Bigs:        []int64{math.MinInt64},
		Child:       scalarChildDTO{ChildBig: math.MaxInt64},
		Children:    []scalarChildDTO{{ChildBig: math.MinInt64}},
	}
	msg, err := dtoToPB[testpb.ScalarMatrix](respPlan, dto)
	if err != nil {
		t.Fatalf("dtoToPB: %v", err)
	}
	if msg.GetBig() != math.MaxInt64 {
		t.Errorf("root int64: got %d, want %d", msg.GetBig(), int64(math.MaxInt64))
	}
	if msg.GetUnsignedBig() != math.MaxUint64 {
		t.Errorf("root uint64: got %d, want %d", msg.GetUnsignedBig(), uint64(math.MaxUint64))
	}
	if msg.GetBigs()[0] != math.MinInt64 {
		t.Errorf("repeated int64: got %d, want %d", msg.GetBigs()[0], int64(math.MinInt64))
	}
	if msg.GetChild().GetChildBig() != math.MaxInt64 {
		t.Errorf("child int64: got %d", msg.GetChild().GetChildBig())
	}
	if msg.GetChildren()[0].GetChildBig() != math.MinInt64 {
		t.Errorf("repeated child int64: got %d", msg.GetChildren()[0].GetChildBig())
	}
}

// Absent optional stays nil (presence survives the unquote step), and a
// zero-valued 64-bit field — which protojson omits entirely — lands as 0.
func TestBridge_Int64_PresenceAndZero(t *testing.T) {
	reqPlan, _ := scalarMatrixPlans(t)
	got, err := pbToDTO[scalarMatrixDTO](reqPlan, &testpb.ScalarMatrix{Label: "only"})
	if err != nil {
		t.Fatalf("pbToDTO: %v", err)
	}
	if got.MaybeBig != nil {
		t.Errorf("absent optional int64 must stay nil, got %d", *got.MaybeBig)
	}
	if got.Big != 0 || got.Bigs != nil {
		t.Errorf("omitted 64-bit fields must land zero, got %d / %v", got.Big, got.Bigs)
	}
	zero := int64(0)
	got, err = pbToDTO[scalarMatrixDTO](reqPlan, &testpb.ScalarMatrix{MaybeBig: &zero})
	if err != nil {
		t.Fatalf("pbToDTO (explicit zero): %v", err)
	}
	if got.MaybeBig == nil || *got.MaybeBig != 0 {
		t.Errorf("explicit zero must arrive set, got %v", got.MaybeBig)
	}
}

// A DTO seat declared `string` carries the digits AS TEXT — the quoted wire
// form is the right value there, so the plan must not unquote it.
func TestBridge_Int64_IntoStringSeat_KeepsDigitsAsText(t *testing.T) {
	md := (&testpb.ScalarChild{}).ProtoReflect().Descriptor()
	type childStringDTO struct {
		ChildBig   string `json:"childBig"`
		ChildLabel string `json:"childLabel"`
	}
	plan, err := compileBindPlan("t", "request", md, reflect.TypeOf(childStringDTO{}), nil, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := pbToDTO[childStringDTO](plan, &testpb.ScalarChild{ChildBig: 1050, ChildLabel: "x"})
	if err != nil {
		t.Fatalf("pbToDTO: %v", err)
	}
	if got.ChildBig != "1050" {
		t.Errorf("string seat must receive the digits as text, got %q", got.ChildBig)
	}
	// And back out: protojson accepts the quoted form for a 64-bit field.
	respPlan, err := compileBindPlan("t", "response", md, reflect.TypeOf(childStringDTO{}), nil, nil)
	if err != nil {
		t.Fatalf("compile response: %v", err)
	}
	back, err := dtoToPB[testpb.ScalarChild](respPlan, got)
	if err != nil {
		t.Fatalf("dtoToPB: %v", err)
	}
	if back.GetChildBig() != 1050 {
		t.Errorf("string seat must re-emit the number, got %d", back.GetChildBig())
	}
}

// An enum reaches the DTO as its member NAME, so a numeric request seat can
// never receive it — that is a BOOT failure, not a per-request 400.
func TestCompileBindPlan_EnumIntoNumericRequestSeat_BootFails(t *testing.T) {
	md := (&testpb.FlavorCarrier{}).ProtoReflect().Descriptor()
	type numericFlavorDTO struct {
		Flavor int32 `json:"flavor"`
	}
	_, err := compileBindPlan("t", "request", md, reflect.TypeOf(numericFlavorDTO{}), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "member NAME") {
		t.Fatalf("expected the enum-seat boot failure, got %v", err)
	}
	// The response direction is unaffected: protojson ACCEPTS a number for
	// an enum, so an existing numeric response seat keeps working.
	if _, err := compileBindPlan("t", "response", md, reflect.TypeOf(numericFlavorDTO{}), nil, nil); err != nil {
		t.Fatalf("response direction must still compile: %v", err)
	}
	// The string seat is the one the wire form fits, both directions.
	type stringFlavorDTO struct {
		Flavor string `json:"flavor"`
	}
	plan, err := compileBindPlan("t", "request", md, reflect.TypeOf(stringFlavorDTO{}), nil, nil)
	if err != nil {
		t.Fatalf("string seat must compile: %v", err)
	}
	got, err := pbToDTO[stringFlavorDTO](plan, &testpb.FlavorCarrier{Flavor: testpb.Flavor_FLAVOR_SWEET})
	if err != nil {
		t.Fatalf("pbToDTO: %v", err)
	}
	if got.Flavor != "FLAVOR_SWEET" {
		t.Errorf("enum must arrive as its member name, got %q", got.Flavor)
	}
}

// protojson renders a non-finite float as the STRING "NaN"/"Infinity"/
// "-Infinity" (the proto3 JSON mapping) — the same dialect gap as the quoted
// 64-bit integer, but this one is not reconcilable: JSON has no literal for
// them, so encoding/json can neither read one into a float field nor write
// one back. The bridge must say so, naming the field and the value, instead
// of leaking the codec's generic "cannot unmarshal string into …".
func TestBridge_NonFiniteFloat_RejectedByName(t *testing.T) {
	reqPlan, _ := scalarMatrixPlans(t)
	cases := []struct {
		label string
		msg   *testpb.ScalarMatrix
		field string
		want  string
	}{
		{"double NaN", &testpb.ScalarMatrix{Ratio: math.NaN()}, "ratio", "NaN"},
		{"double +Inf", &testpb.ScalarMatrix{Ratio: math.Inf(1)}, "ratio", "Infinity"},
		{"double -Inf", &testpb.ScalarMatrix{Ratio: math.Inf(-1)}, "ratio", "-Infinity"},
		{"float NaN", &testpb.ScalarMatrix{Fraction: float32(math.NaN())}, "fraction", "NaN"},
	}
	for _, c := range cases {
		_, err := pbToDTO[scalarMatrixDTO](reqPlan, c.msg)
		if err == nil {
			t.Errorf("%s: expected a rejection", c.label)
			continue
		}
		if !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: message must name the field and the value, got %v", c.label, err)
		}
		if !strings.Contains(err.Error(), "MountRaw") {
			t.Errorf("%s: message must point at the escape hatch, got %v", c.label, err)
		}
	}
	// Finite floats are untouched by the guard — the ordinary case still binds.
	got, err := pbToDTO[scalarMatrixDTO](reqPlan, &testpb.ScalarMatrix{Ratio: 2.5, Fraction: 0.5})
	if err != nil {
		t.Fatalf("finite floats must bind: %v", err)
	}
	if got.Ratio != 2.5 || got.Fraction != 0.5 {
		t.Errorf("finite floats: got %v / %v", got.Ratio, got.Fraction)
	}
}

// The guard fires ONLY where the plan marked a float seat. A proto STRING
// field whose value happens to be the text "NaN" is ordinary data and must
// cross untouched — otherwise the guard would corrupt legitimate payloads.
func TestBridge_NonFiniteFloat_GuardDoesNotTouchStrings(t *testing.T) {
	reqPlan, _ := scalarMatrixPlans(t)
	got, err := pbToDTO[scalarMatrixDTO](reqPlan, &testpb.ScalarMatrix{
		Label:  "NaN",
		Labels: []string{"Infinity", "-Infinity"},
	})
	if err != nil {
		t.Fatalf("a string field carrying the text %q must bind: %v", "NaN", err)
	}
	if got.Label != "NaN" || len(got.Labels) != 2 || got.Labels[0] != "Infinity" {
		t.Errorf("string payload altered: %+v", got)
	}
}

// guardFloatSpecials in isolation — the repeated form and the values it must
// leave alone. The ScalarMatrix fixture has no repeated float, so the slice
// branch is only reachable here.
func TestGuardFloatSpecials(t *testing.T) {
	for _, v := range []any{"NaN", "Infinity", "-Infinity"} {
		if err := guardFloatSpecials("f", v); err == nil {
			t.Errorf("%v must be rejected", v)
		}
	}
	if err := guardFloatSpecials("f", []any{float64(1), "Infinity"}); err == nil {
		t.Error("a sentinel inside a repeated field must be rejected")
	}
	for _, v := range []any{float64(2.5), "nan", "NAN", "Inf", "", "2.5", []any{float64(1), "ok"}, nil} {
		if err := guardFloatSpecials("f", v); err != nil {
			t.Errorf("%#v must pass untouched, got %v", v, err)
		}
	}
}

// The RESPONSE direction: a handler that produces a non-finite float cannot be
// rendered either — encoding/json refuses to marshal it. That is a server bug,
// so the client correctly sees an opaque INTERNAL; this pins that it fails
// LOUDLY on the server instead of emitting something malformed.
func TestBridge_NonFiniteFloat_ResponseSideFails(t *testing.T) {
	_, respPlan := scalarMatrixPlans(t)
	if _, err := dtoToPB[testpb.ScalarMatrix](respPlan, scalarMatrixDTO{Ratio: math.NaN()}); err == nil {
		t.Fatal("a non-finite float must not render silently")
	} else if !strings.Contains(err.Error(), "NaN") {
		t.Errorf("the server-side error must name the value, got %v", err)
	}
}

// …and the escape hatch the guard's message recommends is real: the LIMIT is
// the JSON bridge, not the transport. The protobuf binary wire carries a
// non-finite float exactly, which is what a MountRaw procedure — a
// hand-written Connect handler with no bridge in the middle — rides on.
func TestNonFiniteFloat_BinaryWireCarriesIt(t *testing.T) {
	in := &testpb.ScalarMatrix{Ratio: math.Inf(1), Fraction: float32(math.NaN())}
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	var out testpb.ScalarMatrix
	if err := proto.Unmarshal(raw, &out); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	if !math.IsInf(out.GetRatio(), 1) {
		t.Errorf("+Inf lost on the binary wire: %v", out.GetRatio())
	}
	if !math.IsNaN(float64(out.GetFraction())) {
		t.Errorf("NaN lost on the binary wire: %v", out.GetFraction())
	}
}

// encoding/json resolves a promoted-name collision by DEPTH: the shallowest
// declaration wins, and two equally deep ones are ambiguous — it fills
// NEITHER. The collector must agree, or the plan binds a seat the codec
// silently leaves at its zero value.
func TestDTOFieldsOf_AmbiguousSiblingEmbeddedIsDropped(t *testing.T) {
	// No json tags: the collision rides the FIELD NAMES, so this stays a
	// genuine ambiguity for both the collector and encoding/json without
	// tripping vet's duplicate-tag check.
	type left struct{ Flavor string }
	type right struct{ Flavor int32 }
	type outer struct {
		left
		right
	}
	if _, ok := dtoFieldsOf(reflect.TypeOf(outer{}))[normalizeName("flavor")]; ok {
		t.Error("an ambiguous promoted name must not be bindable")
	}
	// …which is what encoding/json does with the same payload: neither filled.
	var v outer
	if err := json.Unmarshal([]byte(`{"Flavor":"x"}`), &v); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if v.left.Flavor != "" || v.right.Flavor != 0 {
		t.Errorf("encoding/json filled %q / %d — the premise of this test is wrong",
			v.left.Flavor, v.right.Flavor)
	}
	// A proto field wanting that name now fails the boot check, loudly.
	md := (&testpb.FlavorCarrier{}).ProtoReflect().Descriptor()
	_, err := compileBindPlan("t", "request", md, reflect.TypeOf(outer{}), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no counterpart") {
		t.Fatalf("expected the boot abort, got %v", err)
	}
	// An UNCONTESTED promotion is unaffected, and a shallower declaration
	// still wins over a deeper one.
	type onlyLeft struct {
		left
		Other string `json:"other"`
	}
	_ = right{}
	if _, ok := dtoFieldsOf(reflect.TypeOf(onlyLeft{}))[normalizeName("flavor")]; !ok {
		t.Error("an uncontested promoted field must stay bindable")
	}
}

// The unquote step is deliberately conservative: only a plain integer
// literal is unquoted, so a genuine mismatch still surfaces as the normal
// json.Unmarshal error rather than forging invalid JSON.
func TestUnquoteIntegers_OnlyPlainIntegers(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{"1050", json.RawMessage("1050")},
		{"-1050", json.RawMessage("-1050")},
		{"10.5", "10.5"},
		{"1e3", "1e3"},
		{"", ""},
		{"-", "-"},
		{"abc", "abc"},
		{" 12", " 12"},
		{true, true},
		{float64(3), float64(3)},
	}
	for _, c := range cases {
		if got := unquoteIntegers(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("unquoteIntegers(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
	// Slices are rewritten element-wise, in place, non-integers untouched.
	arr := []any{"7", "x", float64(1)}
	if got := unquoteIntegers(arr); !reflect.DeepEqual(got, []any{json.RawMessage("7"), "x", float64(1)}) {
		t.Errorf("slice rewrite: %#v", got)
	}
}

// On a name collision between an embedded struct's field and the outer
// struct's own, encoding/json binds the SHALLOWER one. The plan must pair
// with the same field, or the bridge would target one the codec never fills.
func TestDTOFieldsOf_OuterFieldWinsOverEmbedded(t *testing.T) {
	type inner struct {
		Flavor string `json:"flavor"`
	}
	type outer struct {
		inner
		Flavor int32 `json:"flavor"`
	}
	df, ok := dtoFieldsOf(reflect.TypeOf(outer{}))[normalizeName("flavor")]
	if !ok {
		t.Fatal("flavor must be bindable")
	}
	if df.typ.Kind() != reflect.Int32 {
		t.Errorf("the outer (shallower) field must win, got %s", df.typ)
	}
	// …which is exactly what encoding/json does with the same payload.
	var got outer
	if err := json.Unmarshal([]byte(`{"flavor":7}`), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Flavor != 7 || got.inner.Flavor != "" {
		t.Errorf("encoding/json filled outer=%d inner=%q", got.Flavor, got.inner.Flavor)
	}
	// A field only the embedded struct declares is still promoted.
	type onlyEmbedded struct {
		inner
		Other string `json:"other"`
	}
	if _, ok := dtoFieldsOf(reflect.TypeOf(onlyEmbedded{}))[normalizeName("flavor")]; !ok {
		t.Error("an uncontested embedded field must stay bindable")
	}
}

// The rejection must propagate from ANY depth — a nested message and each
// element of a repeated one. Driven through hand-built plans because the
// ScalarMatrix fixture's child carries no float field.
func TestRewriteToDTO_GuardPropagatesFromNestedAndRepeated(t *testing.T) {
	child := &bindPlan{nodes: map[string]bindNode{"ratio": {coerce: coerceGuardFloat}}}
	plan := &bindPlan{nodes: map[string]bindNode{
		"child": {child: child},
		"kids":  {child: child},
	}}

	err := plan.rewriteToDTO(map[string]any{"child": map[string]any{"ratio": "NaN"}})
	if err == nil || !strings.Contains(err.Error(), "ratio") {
		t.Errorf("nested message: expected the rejection, got %v", err)
	}

	err = plan.rewriteToDTO(map[string]any{"kids": []any{
		map[string]any{"ratio": float64(1)},
		map[string]any{"ratio": "-Infinity"},
	}})
	if err == nil || !strings.Contains(err.Error(), "ratio") {
		t.Errorf("repeated message: expected the rejection, got %v", err)
	}

	// Clean payloads at the same depths still pass.
	if err := plan.rewriteToDTO(map[string]any{
		"child": map[string]any{"ratio": float64(2.5)},
		"kids":  []any{map[string]any{"ratio": float64(0.5)}},
	}); err != nil {
		t.Errorf("finite nested floats must pass, got %v", err)
	}
}

// Depth resolution across THREE levels: the winner is the shallowest
// declaration, whatever order the walk happens to reach them in, and a
// deeper ambiguity is overruled by a shallower unambiguous one — the exact
// precedence encoding/json applies.
func TestDTOFieldsOf_DepthPrecedenceAcrossLevels(t *testing.T) {
	type deepest struct{ Flavor int64 }
	type middle struct {
		deepest
		Flavor int32 // depth 1 — beats deepest (depth 2)
	}
	type outer struct {
		middle
		Flavor string // depth 0 — beats everything below
	}
	df, ok := dtoFieldsOf(reflect.TypeOf(outer{}))[normalizeName("flavor")]
	if !ok {
		t.Fatal("the shallowest declaration must be bindable")
	}
	if df.typ.Kind() != reflect.String {
		t.Errorf("depth 0 must win, got %s", df.typ)
	}

	// Without the depth-0 declaration, depth 1 wins over depth 2.
	type outerNoOwn struct{ middle }
	df, ok = dtoFieldsOf(reflect.TypeOf(outerNoOwn{}))[normalizeName("flavor")]
	if !ok {
		t.Fatal("the depth-1 declaration must be bindable")
	}
	if df.typ.Kind() != reflect.Int32 {
		t.Errorf("depth 1 must beat depth 2, got %s", df.typ)
	}

	// A tie DEEP down is still ambiguous when nothing shallower resolves it…
	type leftDeep struct{ Tone string }
	type rightDeep struct{ Tone int32 }
	type tie struct {
		leftDeep
		rightDeep
	}
	if _, ok := dtoFieldsOf(reflect.TypeOf(tie{}))[normalizeName("tone")]; ok {
		t.Error("an unresolved deep tie must stay unbindable")
	}
	// …and a shallower declaration resolves it.
	type tieResolved struct {
		leftDeep
		rightDeep
		Tone float64
	}
	df, ok = dtoFieldsOf(reflect.TypeOf(tieResolved{}))[normalizeName("tone")]
	if !ok || df.typ.Kind() != reflect.Float64 {
		t.Errorf("a shallower declaration must resolve the tie, got ok=%v %v", ok, df.typ)
	}
}

// A plan that only unquotes must NOT pay for the outbound map round-trip —
// protojson accepts the bare number encoding/json already emits.
func TestBindPlan_UnquoteOnlyPlanSkipsWireRewrite(t *testing.T) {
	md := (&testpb.ScalarChild{}).ProtoReflect().Descriptor()
	type agreedDTO struct {
		ChildBig   int64  `json:"childBig"`
		ChildLabel string `json:"childLabel"`
	}
	plan, err := compileBindPlan("t", "request", md, reflect.TypeOf(agreedDTO{}), nil, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !plan.hasNodes() {
		t.Fatal("the 64-bit field must produce a node")
	}
	if plan.rewritesWire() {
		t.Error("an unquote-only plan must not trigger the outbound rewrite")
	}
	// The renamed-child plan, by contrast, does rewrite on the way out.
	matrixPlan, _ := scalarMatrixPlans(t)
	if !matrixPlan.rewritesWire() {
		t.Error("a plan with a renamed child key must rewrite on the way out")
	}
}
