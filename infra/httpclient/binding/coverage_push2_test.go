package binding

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// errBody is an io.ReadCloser whose Read always fails — used to drive the
// DecodeResponse read-body error branch.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errBody) Close() error             { return nil }

func pushResp(body io.ReadCloser) *http.Response {
	return &http.Response{Header: http.Header{}, Body: body}
}

func TestDecodeResponse_OutNotStruct(t *testing.T) {
	var n int
	err := DecodeResponse(pushResp(io.NopCloser(strings.NewReader("1"))), EndpointMeta{ResponseCodec: "json"}, &n)
	if err == nil {
		t.Fatal("expected error when out points at a non-struct")
	}
}

func TestDecodeResponse_BadTagSurfaces(t *testing.T) {
	out := &struct {
		X string `http:"query"`
	}{}
	err := DecodeResponse(pushResp(io.NopCloser(strings.NewReader(`{}`))), EndpointMeta{ResponseCodec: "json"}, out)
	if err == nil {
		t.Fatal("expected inspect error for malformed http tag on response struct")
	}
}

func TestDecodeResponse_ReadBodyError(t *testing.T) {
	out := &struct {
		Name string `json:"name"`
	}{}
	err := DecodeResponse(pushResp(errBody{}), EndpointMeta{ResponseCodec: "json"}, out)
	if err == nil {
		t.Fatal("expected read-body error")
	}
}

// --- parseHTTPTag: empty leading token --------------------------------------

func TestParseHTTPTag_EmptyLeadingToken(t *testing.T) {
	_, present, err := parseHTTPTag(",name")
	if !present || err == nil || !strings.Contains(err.Error(), "empty http tag") {
		t.Fatalf("expected empty-tag error, got present=%v err=%v", present, err)
	}
}

// --- inspectType: nil + pointer deref ---------------------------------------

func TestInspectType_NilType(t *testing.T) {
	if _, err := inspectResponseType(nil); err == nil {
		t.Fatal("expected error for nil type")
	}
}

type tinyReqInspect struct {
	ID string `http:"path,id"`
}

func TestInspectType_PointerTypeDerefs(t *testing.T) {
	// A pointer-to-struct must deref and inspect the underlying struct.
	if _, err := inspectRequestType(reflect.PointerTo(reflect.TypeOf(tinyReqInspect{})), "/users/{id}"); err != nil {
		t.Fatalf("pointer struct must inspect cleanly, got %v", err)
	}
}

// --- buildPlan: unexported skip + bad tag -----------------------------------

type unexportedFieldResp struct {
	hidden string //nolint:unused // unexported field must be skipped by buildPlan
	Body   string `http:"body,json"`
}

func TestBuildPlan_SkipsUnexportedField(t *testing.T) {
	if _, err := inspectResponseType(reflect.TypeOf(unexportedFieldResp{})); err != nil {
		t.Fatalf("unexported field must be skipped, got %v", err)
	}
}

type badTagResp struct {
	X string `http:"query"`
}

func TestBuildPlan_BadTagSurfaces(t *testing.T) {
	if _, err := inspectResponseType(reflect.TypeOf(badTagResp{})); err == nil {
		t.Fatal("expected error for malformed http tag")
	}
}

// --- validateFieldType: each kind's rejection -------------------------------

type pathNonScalarResp struct {
	Bad struct{ A int } `http:"path,id"`
}

type csvNonSliceResp struct {
	Bad string `http:"query,tags,csv"`
}

type headersBadResp struct {
	Bad map[string]int `http:"headers"`
}

type streamNonReaderResp struct {
	Bad string `http:"body,stream"`
}

type multipartBadResp struct {
	Bad string `http:"body,multipart"`
}

func TestValidateFieldType_Rejections(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
	}{
		{"path-non-scalar", reflect.TypeOf(pathNonScalarResp{})},
		{"csv-non-slice", reflect.TypeOf(csvNonSliceResp{})},
		{"headers-bad-map", reflect.TypeOf(headersBadResp{})},
		{"stream-non-reader", reflect.TypeOf(streamNonReaderResp{})},
		{"multipart-bad", reflect.TypeOf(multipartBadResp{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := inspectResponseType(tc.typ); err == nil {
				t.Fatalf("expected validateFieldType rejection for %s", tc.name)
			}
		})
	}
}

// --- form codec: toValues / fromValues / scalarToFormString -----------------

type formStruct struct {
	Name    string  `form:"name"`
	Ptr     *string `form:"ptr"`
	Skip    string  `form:"-"`
	Omitted string  `form:"omitted,omitempty"`
	hidden  string  //nolint:unused // unexported — must be skipped
}

func TestToValues_StructWithPointerUnexportedAndSkip(t *testing.T) {
	s := "deref-me"
	vals, err := toValues(formStruct{Name: "bob", Ptr: &s, Skip: "ignored", Omitted: ""})
	if err != nil {
		t.Fatalf("toValues: %v", err)
	}
	if vals.Get("name") != "bob" {
		t.Errorf("name = %q", vals.Get("name"))
	}
	if vals.Get("ptr") != "deref-me" {
		t.Errorf("ptr deref = %q, want deref-me", vals.Get("ptr"))
	}
	if _, ok := vals["-"]; ok {
		t.Error("form:\"-\" field must be skipped")
	}
	if _, ok := vals["omitted"]; ok {
		t.Error("empty omitempty field must be skipped")
	}
}

func TestToValues_UnsupportedTypeErrors(t *testing.T) {
	if _, err := toValues(42); err == nil {
		t.Fatal("expected error for non-struct, non-map input")
	}
}

func TestToValues_NilPointerYieldsEmpty(t *testing.T) {
	var p *formStruct
	vals, err := toValues(p)
	if err != nil || len(vals) != 0 {
		t.Fatalf("nil pointer must yield empty values, got vals=%v err=%v", vals, err)
	}
}

func TestFromValues_StructSkipsUnexportedAndEmpty(t *testing.T) {
	vals := url.Values{"name": {"alice"}}
	var out formStruct
	if err := fromValues(vals, &out); err != nil {
		t.Fatalf("fromValues: %v", err)
	}
	if out.Name != "alice" {
		t.Errorf("Name = %q, want alice", out.Name)
	}
	// ptr/omitted absent from values → fields skipped (raw == "").
	if out.Ptr != nil {
		t.Errorf("absent field must stay zero, got %v", out.Ptr)
	}
}

func TestFromValues_NonPointerErrors(t *testing.T) {
	if err := fromValues(url.Values{}, formStruct{}); err == nil {
		t.Fatal("expected error when out is not a pointer")
	}
}
