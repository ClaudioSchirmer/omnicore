package binding

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

// --- HasStreamingBody ------------------------------------------------------

type streamReq struct {
	Body io.Reader `http:"body,stream"`
	Mime string    `http:"header,Content-Type"`
}

type multipartReq struct {
	Body Multipart `http:"body,multipart"`
}

type jsonReq struct {
	Body struct {
		A string `json:"a"`
	} `http:"body,json"`
}

type noBodyReq struct {
	Tag string `http:"query,tag"`
}

func TestHasStreamingBody_StreamTag(t *testing.T) {
	if !HasStreamingBody(streamReq{}, "/x") {
		t.Error("expected HasStreamingBody=true for body,stream")
	}
}

func TestHasStreamingBody_MultipartTag(t *testing.T) {
	if !HasStreamingBody(multipartReq{}, "/x") {
		t.Error("expected HasStreamingBody=true for body,multipart")
	}
}

func TestHasStreamingBody_JSONIsFalse(t *testing.T) {
	if HasStreamingBody(jsonReq{}, "/x") {
		t.Error("body,json must NOT be streaming")
	}
}

func TestHasStreamingBody_NoBodyIsFalse(t *testing.T) {
	if HasStreamingBody(noBodyReq{}, "/x") {
		t.Error("absent body tag → not streaming")
	}
}

func TestHasStreamingBody_NilReqType(t *testing.T) {
	if HasStreamingBody(nil, "/x") {
		t.Error("nil type should be reported as non-streaming")
	}
}

func TestHasStreamingBody_NonStructIsFalse(t *testing.T) {
	if HasStreamingBody("not a struct", "/x") {
		t.Error("non-struct type → false")
	}
}

func TestHasStreamingBody_PointerArgs(t *testing.T) {
	v := &streamReq{}
	if !HasStreamingBody(v, "/x") {
		t.Error("non-nil pointer to streaming type should report true")
	}
}

// TestHasStreamingBody_NilPointerResolvesToStructZeroValue locks the
// typed-nil branch of HasStreamingBody: a `var p *streamReq` value goes
// through `reflect.New(rv.Type().Elem()).Elem()` so the inspector sees the
// struct zero value of the element type (not the freshly-allocated pointer).
// Asserting true here proves the recovery path lands on the right Kind.
// The non-typed nil case (`reqType == nil`) is covered by
// TestHasStreamingBody_NilReqType above.
func TestHasStreamingBody_NilPointerResolvesToStructZeroValue(t *testing.T) {
	var nilPtr *streamReq
	if !HasStreamingBody(nilPtr, "/x") {
		t.Error("typed-nil pointer should recover via reflect.New().Elem() and report streaming=true")
	}
}

type invalidStreamReq struct {
	// body,stream needs an io.Reader. Wrong type → inspection error.
	Body string `http:"body,stream"`
}

func TestHasStreamingBody_InspectionErrorReturnsFalse(t *testing.T) {
	if HasStreamingBody(invalidStreamReq{}, "/x") {
		t.Error("inspection error path should return false, not panic")
	}
}

// --- encodeMultipartBody / newFilePartHeader -----------------------------

func TestEncodeMultipartBody_WritesFieldsAndFiles(t *testing.T) {
	m := Multipart{
		Fields: []MultipartField{
			{Name: "category", Value: "id-proof"},
		},
		Files: []MultipartFile{
			{
				Name:     "file",
				Filename: "passport.pdf",
				MimeType: "application/pdf",
				Content:  strings.NewReader("PDF-DATA"),
			},
		},
	}

	body, ct, length, err := encodeMultipartBody(m)
	if err != nil {
		t.Fatalf("encodeMultipartBody err = %v", err)
	}
	if length != -1 {
		t.Errorf("Content-Length should be -1 (chunked), got %d", length)
	}
	if !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Errorf("Content-Type = %q, want multipart/form-data; boundary=...", ct)
	}

	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read multipart body: %v", err)
	}
	s := string(raw)
	for _, want := range []string{`name="category"`, "id-proof", `name="file"`, `filename="passport.pdf"`, "application/pdf", "PDF-DATA"} {
		if !strings.Contains(s, want) {
			t.Errorf("multipart body missing %q\nfull body: %s", want, s)
		}
	}
}

func TestEncodeMultipartBody_RejectsWrongType(t *testing.T) {
	_, _, _, err := encodeMultipartBody("not-multipart")
	if err == nil {
		t.Error("expected error on non-Multipart input")
	}
}

func TestEncodeMultipartBody_FileWithoutContent(t *testing.T) {
	m := Multipart{
		Files: []MultipartFile{
			{Name: "f", Filename: "empty.bin"}, // Content == nil
		},
	}
	body, _, _, err := encodeMultipartBody(m)
	if err != nil {
		t.Fatalf("encodeMultipartBody err = %v", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Errorf("draining body failed: %v", err)
	}
}

func TestEncodeMultipartBody_FileContentReadError(t *testing.T) {
	// errReader always returns an error on Read — exercises the io.Copy
	// failure path that closes the pipe with an error so the consumer sees it.
	m := Multipart{
		Files: []MultipartFile{
			{Name: "f", Filename: "broken.bin", Content: errReader{}},
		},
	}
	body, _, _, err := encodeMultipartBody(m)
	if err != nil {
		t.Fatalf("encodeMultipartBody err = %v", err)
	}
	if _, err := io.Copy(io.Discard, body); err == nil {
		t.Error("expected the pipe to surface the Content read error")
	}
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestNewFilePartHeader_WithFilenameAndMime(t *testing.T) {
	h := newFilePartHeader(MultipartFile{Name: "file", Filename: "p.pdf", MimeType: "application/pdf"})
	disp := h["Content-Disposition"][0]
	if !strings.Contains(disp, `name="file"`) {
		t.Errorf("disp = %q", disp)
	}
	if !strings.Contains(disp, `filename="p.pdf"`) {
		t.Errorf("disp = %q (filename missing)", disp)
	}
	if ct := h["Content-Type"][0]; ct != "application/pdf" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestNewFilePartHeader_NoFilenameNoMime(t *testing.T) {
	h := newFilePartHeader(MultipartFile{Name: "field"})
	disp := h["Content-Disposition"][0]
	if !strings.Contains(disp, `name="field"`) {
		t.Errorf("disp = %q", disp)
	}
	if strings.Contains(disp, "filename") {
		t.Errorf("disp should not contain filename when Filename empty: %q", disp)
	}
	if _, ok := h["Content-Type"]; ok {
		t.Errorf("Content-Type header should be absent when MimeType empty")
	}
}

// --- kindLabel cases ------------------------------------------------------

func TestKindLabel_AllKinds(t *testing.T) {
	cases := []struct {
		fb   fieldBinding
		want string
	}{
		{fieldBinding{kind: bindPath, name: "id"}, "path,id"},
		{fieldBinding{kind: bindQuerySingle, name: "verbose"}, "query,verbose"},
		{fieldBinding{kind: bindQueryCSV, name: "tags"}, "query,tags,csv"},
		{fieldBinding{kind: bindQueryMulti, name: "tags"}, "query,tags,multi"},
		{fieldBinding{kind: bindHeader, name: "X-Tenant"}, "header,X-Tenant"},
		{fieldBinding{kind: bindHeadersMap}, "headers"},
		{fieldBinding{kind: bindBody, codec: "json"}, "body,json"},
		{fieldBinding{kind: bindKind(999)}, ""},
	}
	for _, tc := range cases {
		if got := kindLabel(tc.fb); got != tc.want {
			t.Errorf("kindLabel(%+v) = %q, want %q", tc.fb, got, tc.want)
		}
	}
}

// --- scalarToString branches ---------------------------------------------

func TestScalarToString_AllKindsAndPointers(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"hi", "hi"},
		{true, "true"},
		{int8(-1), "-1"},
		{int32(-200), "-200"},
		{int64(-3_000_000_000), "-3000000000"},
		{uint8(7), "7"},
		{uint32(123456), "123456"},
		{float32(1.5), "1.5"},
		{float64(2.25), "2.25"},
	}
	for _, tc := range cases {
		got, err := scalarToString(reflect.ValueOf(tc.in))
		if err != nil {
			t.Errorf("scalarToString(%v) err = %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("scalarToString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestScalarToString_NilPointerYieldsEmpty(t *testing.T) {
	var s *string
	got, err := scalarToString(reflect.ValueOf(s))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "" {
		t.Errorf("nil pointer should yield empty string, got %q", got)
	}
}

func TestScalarToString_NonNilPointerDereferences(t *testing.T) {
	s := "hi"
	got, err := scalarToString(reflect.ValueOf(&s))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "hi" {
		t.Errorf("dereferenced pointer = %q, want hi", got)
	}
}

func TestScalarToString_UnsupportedKindErrors(t *testing.T) {
	_, err := scalarToString(reflect.ValueOf([]int{1, 2, 3}))
	if err == nil {
		t.Error("expected unsupported kind error for slice")
	}
}

// --- scalarToFormString --------------------------------------------------

func TestScalarToFormString_StringSliceJoined(t *testing.T) {
	got, err := scalarToFormString(reflect.ValueOf([]string{"a", "b", "c"}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "a b c" {
		t.Errorf("string slice = %q, want \"a b c\"", got)
	}
}

func TestScalarToFormString_NonStringSliceErrors(t *testing.T) {
	_, err := scalarToFormString(reflect.ValueOf([]int{1, 2, 3}))
	if err == nil {
		t.Error("expected error for non-string slice")
	}
}

func TestScalarToFormString_AllScalars(t *testing.T) {
	cases := []any{"hi", true, int(-1), uint(7), float64(1.5)}
	for _, in := range cases {
		if _, err := scalarToFormString(reflect.ValueOf(in)); err != nil {
			t.Errorf("scalarToFormString(%v) err = %v", in, err)
		}
	}
}

func TestScalarToFormString_NilPointerEmpty(t *testing.T) {
	var p *string
	got, err := scalarToFormString(reflect.ValueOf(p))
	if err != nil || got != "" {
		t.Errorf("nil pointer = (%q, %v)", got, err)
	}
}

// --- setFormField --------------------------------------------------------

func TestSetFormField_AllKinds(t *testing.T) {
	type all struct {
		S  string
		B  bool
		I  int64
		U  uint32
		F  float32
		Bp *bool
	}
	target := &all{}
	v := reflect.ValueOf(target).Elem()

	check := func(field string, raw string) {
		t.Helper()
		if err := setFormField(v.FieldByName(field), raw); err != nil {
			t.Errorf("setFormField(%s,%q) err = %v", field, raw, err)
		}
	}
	check("S", "hello")
	check("B", "true")
	check("I", "-42")
	check("U", "100")
	check("F", "1.5")
	check("Bp", "false") // covers the pointer-allocates branch

	if target.S != "hello" || !target.B || target.I != -42 || target.U != 100 || target.F != 1.5 {
		t.Errorf("populated struct: %+v", target)
	}
	if target.Bp == nil || *target.Bp {
		t.Errorf("Bp not populated correctly: %+v", target.Bp)
	}
}

func TestSetFormField_InvalidBool(t *testing.T) {
	type s struct{ B bool }
	target := &s{}
	v := reflect.ValueOf(target).Elem().Field(0)
	if err := setFormField(v, "notbool"); err == nil {
		t.Error("expected ParseBool error")
	}
}

func TestSetFormField_InvalidInt(t *testing.T) {
	type s struct{ I int }
	target := &s{}
	v := reflect.ValueOf(target).Elem().Field(0)
	if err := setFormField(v, "abc"); err == nil {
		t.Error("expected ParseInt error")
	}
}

func TestSetFormField_InvalidUint(t *testing.T) {
	type s struct{ U uint }
	target := &s{}
	v := reflect.ValueOf(target).Elem().Field(0)
	if err := setFormField(v, "-1"); err == nil {
		t.Error("expected ParseUint error")
	}
}

func TestSetFormField_InvalidFloat(t *testing.T) {
	type s struct{ F float64 }
	target := &s{}
	v := reflect.ValueOf(target).Elem().Field(0)
	if err := setFormField(v, "nope"); err == nil {
		t.Error("expected ParseFloat error")
	}
}

func TestSetFormField_UnsupportedKind(t *testing.T) {
	type s struct{ M map[string]string }
	target := &s{}
	v := reflect.ValueOf(target).Elem().Field(0)
	if err := setFormField(v, "irrelevant"); err == nil {
		t.Error("expected unsupported field kind error")
	}
}

// Lock the io.Reader and Multipart sentinel types are the ones inspect.go
// caches — guard against accidental package-level removal.
var _ = bytes.Buffer{}
