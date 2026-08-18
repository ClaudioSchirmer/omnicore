package responses

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// legacyMap is the retired JSON render→remap→decode trip, kept as the
// reference implementation the compiled pair copier must match.
func legacyMap[TResp any](result any) TResp {
	var out TResp
	doc := goDocOf(result)
	plan := planFor(reflect.TypeOf(out))
	renamed := remapDoc(doc, plan)
	if raw, err := json.Marshal(renamed); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	normalizeSlices(reflect.ValueOf(&out).Elem(), plan)
	convergeEnums(reflect.ValueOf(&out).Elem(), plan)
	return out
}

type dChildResult struct {
	ID    string
	Label *string
	Rank  int32
}

type dEmbedResult struct{ Origin string }

type dResult struct {
	dEmbedResult

	ID       domain.ID
	Name     string
	Alias    *string
	Age      int64
	Count    uint32
	Score    float64
	Active   bool
	When     time.Time
	WhenPtr  *time.Time
	Tags     []string
	Children []dChildResult
	Nested   dChildResult
	Ptr      *dChildResult
	Tier     wTier
	Extra    string // absent from the Response: must be dropped
}

type dChildResp struct {
	ID    *string `json:"id,omitempty"`
	Label *string `json:"label,omitempty"`
	Rank  *int64  `json:"rank,omitempty"` // widened + pointer-wrapped
}

type dEmbedResp struct {
	Origin *string `json:"origin,omitempty"`
}

type dResp struct {
	Auto
	dEmbedResp

	ID       string       `json:"id"` // domain.ID → string
	Name     *string      `json:"name,omitempty"`
	Alias    *string      `json:"alias,omitempty"`
	Age      int64        `json:"age"`
	Count    uint32       `json:"count"`
	Score    *float64     `json:"score,omitempty"`
	Active   bool         `json:"active"`
	When     time.Time    `json:"when"`
	WhenPtr  *time.Time   `json:"whenPtr,omitempty"`
	Tags     []string     `json:"tags"`
	Children []dChildResp `json:"children"`
	Nested   *dChildResp  `json:"nested,omitempty"`
	Ptr      *dChildResp  `json:"ptr,omitempty"`
	Tier     wTier        `json:"tier"`
	Skipped  string       `json:"-"`
}

func dFixture() dResult {
	alias := "ali"
	lbl := "l1"
	when := time.Date(2026, 8, 17, 10, 30, 0, 0, time.UTC)
	return dResult{
		dEmbedResult: dEmbedResult{Origin: "emb"},
		ID:           domain.NewID("7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"),
		Name:         "Alice",
		Alias:        &alias,
		Age:          1<<62 + 3,
		Count:        99,
		Score:        12.5,
		Active:       true,
		When:         when,
		WhenPtr:      &when,
		Tags:         []string{"a", "b"},
		Children: []dChildResult{
			{ID: "c1", Label: &lbl, Rank: 1},
			{ID: "c2", Rank: 2},
		},
		Nested: dChildResult{ID: "n1", Rank: 5},
		Ptr:    &dChildResult{ID: "p1", Rank: 6},
		Tier:   wTierGold,
		Extra:  "must-not-surface",
	}
}

func TestDirectMap_ParityWithLegacyTrip(t *testing.T) {
	in := dFixture()
	got := AutoFromResult[dResp](in)
	want := legacyMap[dResp](in)
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("direct copy diverged from the JSON trip:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
	if got.ID != "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51" {
		t.Fatalf("expected the domain.ID rendered as its string value, got %q", got.ID)
	}
	if got.Origin == nil || *got.Origin != "emb" {
		t.Fatalf("expected the promoted embedded field mapped, got %#v", got.Origin)
	}
	if got.Age != 1<<62+3 {
		t.Fatalf("expected 64-bit precision preserved, got %d", got.Age)
	}
	if len(got.Children) != 2 || got.Children[0].Rank == nil || *got.Children[0].Rank != 1 {
		t.Fatalf("expected widened pointer-wrapped child ranks, got %#v", got.Children)
	}
	if got.Skipped != "" {
		t.Fatal("json:\"-\" field must stay zero")
	}
}

func TestDirectMap_ParityOnZeroAndNilShapes(t *testing.T) {
	in := dResult{} // everything zero: nil slices, nil pointers, zero scalars
	got := AutoFromResult[dResp](in)
	want := legacyMap[dResp](in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zero-value copy diverged:\n got: %#v\nwant: %#v", got, want)
	}
	if got.Tags == nil || got.Children == nil {
		t.Fatal("expected nil slices normalized to empty on both paths")
	}
	if got.WhenPtr != nil || got.Alias != nil {
		t.Fatal("expected nil pointers to stay nil")
	}
}

// A Response whose field narrows the Result's shape (int64 source into an
// int8 slot) keeps the round-trip's zero-on-overflow behavior.
func TestDirectMap_OverflowLeavesZero(t *testing.T) {
	type nRes struct{ N int64 }
	type nResp struct {
		Auto
		N int8 `json:"n"`
	}
	got := AutoFromResult[nResp](nRes{N: 300})
	want := legacyMap[nResp](nRes{N: 300})
	if got.N != want.N || got.N != 0 {
		t.Fatalf("expected zero on overflow (legacy parity), got %d want %d", got.N, want.N)
	}
	ok := AutoFromResult[nResp](nRes{N: 42})
	if ok.N != 42 {
		t.Fatalf("expected the in-range value copied, got %d", ok.N)
	}
}

// A pair the copier cannot prove equivalent (time.Time into a string — only
// the type's own JSON codec knows the RFC 3339 rendering) is REFUSED. There is
// no serialization fallback: a Response that declared Auto over a field it
// cannot receive is a contract violation, surfaced with the field named.
func TestDirectMap_UnsupportedPairIsRefused(t *testing.T) {
	type tRes struct{ When time.Time }
	type tResp struct {
		Auto
		When string `json:"when"`
	}
	reason := AutoFromResultReason(reflect.TypeOf(tRes{}), reflect.TypeOf(tResp{}))
	if reason == "" {
		t.Fatal("time.Time -> string must not be auto-mappable")
	}
	if !strings.Contains(reason, "When") || !strings.Contains(reason, "codec") {
		t.Fatalf("reason must name the field and the cause, got %q", reason)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Map must refuse the pair instead of degrading silently")
		}
	}()
	_ = AutoFromResult[tResp](tRes{When: time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC)})
}

func TestDirectMap_StringToDomainID(t *testing.T) {
	type sRes struct{ ID string }
	type sResp struct {
		Auto
		ID domain.ID `json:"id"`
	}
	got := AutoFromResult[sResp](sRes{ID: "abc"})
	want := legacyMap[sResp](sRes{ID: "abc"})
	if got.ID.Value() != "abc" || want.ID.Value() != "abc" {
		t.Fatalf("expected the string promoted into domain.ID on both paths, got %q / %q", got.ID.Value(), want.ID.Value())
	}
}

func TestDirectMap_PairDecisionIsCached(t *testing.T) {
	a := pairCopierFor(reflect.TypeOf(dResult{}), reflect.TypeOf(dResp{}))
	b := pairCopierFor(reflect.TypeOf(dResult{}), reflect.TypeOf(dResp{}))
	if a == nil || b == nil {
		t.Fatal("expected the supported pair compiled")
	}
	// Same cached entry answers both calls — pointer equality of the closure
	// is not observable, but the cache must hold exactly one entry per pair.
	n := 0
	pairCache.Range(func(k, _ any) bool {
		if k == (pairKey{reflect.TypeOf(dResult{}), reflect.TypeOf(dResp{})}) {
			n++
		}
		return true
	})
	if n != 1 {
		t.Fatalf("expected one cache entry for the pair, got %d", n)
	}
}

// TestDirectMap_NumericPairMatrix exercises every scalar conversion branch of
// buildValueCopier against the legacy JSON trip.
func TestDirectMap_NumericPairMatrix(t *testing.T) {
	type srcAll struct {
		A int8
		B int64
		C uint16
		D uint64
		E float32
		F float64
		G int32
	}
	type dstAll struct {
		Auto
		A int64   `json:"a"` // widening int
		B int32   `json:"b"` // narrowing int (overflow → zero)
		C uint64  `json:"c"` // widening uint
		D int64   `json:"d"` // uint → int
		E float64 `json:"e"` // widening float
		F float32 `json:"f"` // narrowing float
		G uint8   `json:"g"` // int → uint (negative → zero)
	}
	cases := []srcAll{
		{A: 7, B: 9, C: 700, D: 12345, E: 0.25, F: 2.5, G: 42},
		{A: -7, B: 1 << 40, C: 0, D: 1<<63 + 9, E: -1, F: 3.14159, G: -1},
		{E: 3.14159, F: 2.718281828459045},
		{B: -3, G: 300},
	}
	for _, in := range cases {
		got := AutoFromResult[dstAll](in)
		want := legacyMap[dstAll](in)
		if got != want {
			t.Fatalf("src %+v: direct %+v, legacy %+v", in, got, want)
		}
	}
}

// Each unsupported shape reports a reason (and therefore boot-fails when the
// Response declares Auto): a map whose value type changes, an interface source
// into a concrete field, and float -> int (whose fraction handling belongs to
// the codec, not to a structural copy).
func TestDirectMap_UnsupportedShapesReportReasons(t *testing.T) {
	type mSrc struct{ M map[string]int }
	type mDst struct {
		Auto
		M map[string]int64 `json:"m"`
	}
	type iSrc struct{ V any }
	type iDst struct {
		Auto
		V string `json:"v"`
	}
	type fSrc struct{ F float64 }
	type fDst struct {
		Auto
		F int64 `json:"f"`
	}
	cases := []struct {
		name     string
		src, dst reflect.Type
	}{
		{"map value type changes", reflect.TypeOf(mSrc{}), reflect.TypeOf(mDst{})},
		{"interface source", reflect.TypeOf(iSrc{}), reflect.TypeOf(iDst{})},
		{"float -> int", reflect.TypeOf(fSrc{}), reflect.TypeOf(fDst{})},
	}
	for _, c := range cases {
		if reason := AutoFromResultReason(c.src, c.dst); reason == "" {
			t.Errorf("%s: expected the pair refused, got no reason", c.name)
		}
	}
}

// TestDirectMap_IdenticalMapsAndInterfaces copies identical exotic types
// directly (same type ⇒ wire-identical).
func TestDirectMap_IdenticalMapsAndInterfaces(t *testing.T) {
	type xSrc struct {
		M map[string]int
		V any
	}
	type xDst struct {
		Auto
		M map[string]int `json:"m"`
		V any            `json:"v"`
	}
	if pairCopierFor(reflect.TypeOf(xSrc{}), reflect.TypeOf(xDst{})) == nil {
		t.Fatal("identical-typed fields must compile a copier")
	}
	got := AutoFromResult[xDst](xSrc{M: map[string]int{"k": 1}, V: "s"})
	if got.M["k"] != 1 || got.V != "s" {
		t.Fatalf("identical types must copy through, got %#v", got)
	}
}

// TestDirectMap_PointerChains covers **T sources and pointer destinations.
func TestDirectMap_PointerChains(t *testing.T) {
	type pSrc struct {
		A **string
		B *int32
	}
	type pDst struct {
		Auto
		A *string `json:"a,omitempty"`
		B int64   `json:"b"`
	}
	s := "deep"
	sp := &s
	in := pSrc{A: &sp, B: nil}
	got := AutoFromResult[pDst](in)
	want := legacyMap[pDst](in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pointer-chain parity: direct %#v, legacy %#v", got, want)
	}
	if got.A == nil || *got.A != "deep" || got.B != 0 {
		t.Fatalf("expected the double pointer deref'd and the nil source left zero, got %#v", got)
	}
}

func BenchmarkMap_Direct(b *testing.B) {
	in := dFixture()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = AutoFromResult[dResp](in)
	}
}

func BenchmarkMap_LegacyTrip(b *testing.B) {
	in := dFixture()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = legacyMap[dResp](in)
	}
}

// TestDirectMap_WireNamesComeFromTags mirrors the example service's
// FindGadgets pair (the shape behind /qa/gadgets and /qa/gadgets-raw): both
// paths must emit the json wire names, never the Go field names. The wire
// vocabulary is decided by the Response's tags at marshal time, so neither
// mapping path can rename a key.
func TestDirectMap_WireNamesComeFromTags(t *testing.T) {
	type gadgetResult struct {
		ID, Code, Name, Category, Status *string
	}
	type gadgetResp struct {
		Auto
		ID       *string `json:"id,omitempty"`
		Code     *string `json:"code,omitempty"`
		Name     *string `json:"name,omitempty"`
		Category *string `json:"category,omitempty"`
		Status   *string `json:"status,omitempty"`
	}
	s := func(v string) *string { return &v }
	in := gadgetResult{ID: s("g1"), Code: s("RX-1"), Name: s("Read Extra 1"), Category: s("rx"), Status: s("active")}

	directJSON, _ := json.Marshal(AutoFromResult[gadgetResp](in))
	legacyJSON, _ := json.Marshal(legacyMap[gadgetResp](in))
	if string(directJSON) != string(legacyJSON) {
		t.Fatalf("paths disagree:\n direct: %s\n legacy: %s", directJSON, legacyJSON)
	}
	want := `{"id":"g1","code":"RX-1","name":"Read Extra 1","category":"rx","status":"active"}`
	if string(directJSON) != want {
		t.Fatalf("wire shape changed:\n got: %s\nwant: %s", directJSON, want)
	}
}

// TestAutoFromResultReason_NamesTheBlockingField proves the diagnostic seat
// the boot advisory consults: "" when the pair is optimized, and otherwise a
// reason naming the Response field that forced the JSON fallback.
func TestAutoFromResultReason_NamesTheBlockingField(t *testing.T) {
	if r := AutoFromResultReason(reflect.TypeOf(dResult{}), reflect.TypeOf(dResp{})); r != "" {
		t.Fatalf("the supported pair must report no reason, got %q", r)
	}
	// Pointers on either side deref to the same verdict.
	if r := AutoFromResultReason(reflect.TypeOf(&dResult{}), reflect.TypeOf(&dResp{})); r != "" {
		t.Fatalf("pointer types must deref to the same verdict, got %q", r)
	}

	type badResult struct {
		ID   string
		When time.Time
	}
	type badResp struct {
		ID   string `json:"id"`
		When string `json:"when"`
	}
	r := AutoFromResultReason(reflect.TypeOf(badResult{}), reflect.TypeOf(badResp{}))
	if !strings.Contains(r, "When") || !strings.Contains(r, "codec") {
		t.Fatalf("reason must name the field and the cause, got %q", r)
	}

	// A nested miss reads as the Response field that carries it.
	type innerResult struct{ Score float64 }
	type outerResult struct{ Inner innerResult }
	type innerResp struct {
		Score int64 `json:"score"`
	}
	type outerResp struct {
		Inner innerResp `json:"inner"`
	}
	nested := AutoFromResultReason(reflect.TypeOf(outerResult{}), reflect.TypeOf(outerResp{}))
	if !strings.Contains(nested, "Inner") || !strings.Contains(nested, "Score") {
		t.Fatalf("nested reason must carry the path, got %q", nested)
	}
}

func TestAutoFromResultReason_NonStructShapes(t *testing.T) {
	if r := AutoFromResultReason(reflect.TypeOf(map[string]any{}), reflect.TypeOf(dResp{})); !strings.Contains(r, "not a struct") {
		t.Fatalf("a map Result must be reported, got %q", r)
	}
	if r := AutoFromResultReason(reflect.TypeOf(dResult{}), reflect.TypeOf("")); !strings.Contains(r, "not a struct") {
		t.Fatalf("a non-struct Response must be reported, got %q", r)
	}
	if r := AutoFromResultReason(nil, nil); !strings.Contains(r, "not a struct") {
		t.Fatalf("nil types must be reported, got %q", r)
	}
}
