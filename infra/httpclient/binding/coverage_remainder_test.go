package binding

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// ─── codec Encode/Decode error branches ─────────────────────────────────────

func TestJsonCodec_EncodeMarshalError(t *testing.T) {
	// A channel is not JSON-marshalable → Encode must wrap the error.
	_, _, err := (jsonCodec{}).Encode(struct{ Ch chan int }{Ch: make(chan int)})
	if err == nil {
		t.Fatal("expected json marshal error for a channel field")
	}
	if !strings.Contains(err.Error(), "encode json") {
		t.Errorf("expected wrapped 'encode json' error, got %v", err)
	}
}

func TestXmlCodec_EncodeMarshalError(t *testing.T) {
	// encoding/xml cannot marshal a map → Encode must wrap the error.
	_, _, err := (xmlCodec{}).Encode(map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("expected xml marshal error for a map")
	}
	if !strings.Contains(err.Error(), "encode xml") {
		t.Errorf("expected wrapped 'encode xml' error, got %v", err)
	}
}

func TestFormCodec_EncodeUnsupportedInput(t *testing.T) {
	// toValues rejects a non-struct, non-map, non-url.Values input → Encode
	// surfaces the error.
	_, _, err := (formCodec{}).Encode(42)
	if err == nil {
		t.Fatal("expected error encoding an unsupported input type")
	}
}

func TestFormCodec_DecodeMalformedQuery(t *testing.T) {
	// url.ParseQuery rejects an invalid percent-escape.
	var out url.Values
	err := (formCodec{}).Decode([]byte("a=%zz"), &out)
	if err == nil {
		t.Fatal("expected decode error on malformed percent-escape")
	}
	if !strings.Contains(err.Error(), "decode form-urlencoded") {
		t.Errorf("expected wrapped decode error, got %v", err)
	}
}

func TestToValues_MapStringString(t *testing.T) {
	vals, err := toValues(map[string]string{"grant": "cc", "scope": "read"})
	if err != nil {
		t.Fatalf("toValues: %v", err)
	}
	if vals.Get("grant") != "cc" || vals.Get("scope") != "read" {
		t.Errorf("unexpected values: %v", vals)
	}
}

func TestToValues_EmptyTagNameFallsBackToFieldName(t *testing.T) {
	type x struct {
		Token string `form:",omitempty"` // empty name → lower-cased field name "token"
	}
	vals, err := toValues(x{Token: "abc"})
	if err != nil {
		t.Fatalf("toValues: %v", err)
	}
	if vals.Get("token") != "abc" {
		t.Errorf("expected key 'token'=abc, got %v", vals)
	}
}

// ─── validateFieldType — body,stream / body,multipart wrong types ───────────

func TestValidateFieldType_QueryCSVOnScalarRejected(t *testing.T) {
	type bad struct {
		Tags string `http:"query,tags,csv"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/x")
	if err == nil || !strings.Contains(err.Error(), "slice or array") {
		t.Fatalf("expected slice/array rejection for query,csv on a scalar, got %v", err)
	}
}

func TestValidateFieldType_BodyStreamNotReader(t *testing.T) {
	type bad struct {
		Body string `http:"body,stream"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/x")
	if err == nil || !strings.Contains(err.Error(), "io.Reader") {
		t.Fatalf("expected io.Reader rejection, got %v", err)
	}
}

func TestValidateFieldType_BodyMultipartWrongType(t *testing.T) {
	type bad struct {
		Body string `http:"body,multipart"`
	}
	_, err := inspectRequestType(reflect.TypeOf(bad{}), "/x")
	if err == nil || !strings.Contains(err.Error(), "Multipart") {
		t.Fatalf("expected Multipart rejection, got %v", err)
	}
}

// ─── BuildRequest — body,stream and body,multipart paths ────────────────────
// streamReq and multipartReq are declared in extras_test.go.

func TestBuildRequest_BodyStream(t *testing.T) {
	req := streamReq{Body: strings.NewReader("payload")}
	httpReq, err := BuildRequest(context.Background(), "http://x", EndpointMeta{Method: "POST", Path: "/up"}, req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	// Unknown length → chunked (ContentLength 0).
	if httpReq.ContentLength != 0 {
		t.Errorf("expected chunked stream (ContentLength 0), got %d", httpReq.ContentLength)
	}
	got, _ := io.ReadAll(httpReq.Body)
	if string(got) != "payload" {
		t.Errorf("stream body mismatch: %q", got)
	}
}

func TestBuildRequest_BodyStreamNil(t *testing.T) {
	req := streamReq{Body: nil}
	_, err := BuildRequest(context.Background(), "http://x", EndpointMeta{Method: "POST", Path: "/up"}, req)
	if err == nil || !strings.Contains(err.Error(), "body,stream field is nil") {
		t.Fatalf("expected nil-stream error, got %v", err)
	}
}

func TestBuildRequest_BodyMultipart(t *testing.T) {
	req := multipartReq{Body: Multipart{
		Fields: []MultipartField{{Name: "k", Value: "v"}},
		Files:  []MultipartFile{{Name: "f", Filename: "a.txt", Content: strings.NewReader("hi")}},
	}}
	httpReq, err := BuildRequest(context.Background(), "http://x", EndpointMeta{Method: "POST", Path: "/up"}, req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if ct := httpReq.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("expected multipart content-type, got %q", ct)
	}
	if httpReq.ContentLength != 0 {
		t.Errorf("multipart goes out chunked (ContentLength 0), got %d", httpReq.ContentLength)
	}
}

// ─── DecodeResponse — bad ResponseCodec on body and whole-struct paths ──────

type bodyTagResp struct {
	Body map[string]any `http:"body,json"`
}

func TestDecodeResponse_BodyTagBadCodec(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"a":1}`)),
	}
	var out bodyTagResp
	err := DecodeResponse(resp, EndpointMeta{ResponseCodec: "bogus"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected unknown-codec error on body path, got %v", err)
	}
}

type wholeResp struct {
	A int `json:"a"`
}

func TestDecodeResponse_WholeStructBadCodec(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"a":1}`)),
	}
	var out wholeResp
	err := DecodeResponse(resp, EndpointMeta{ResponseCodec: "bogus"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected unknown-codec error on whole-struct path, got %v", err)
	}
}

// ─── validateFieldType — pointer field type is dereferenced before checking ──

func TestValidateFieldType_PointerScalarAccepted(t *testing.T) {
	resetPlanCache()
	type req struct {
		H *string `http:"header,X-Token"` // *string derefs to string scalar — valid
	}
	plan, err := inspectRequestType(reflect.TypeOf(req{}), "/x")
	if err != nil {
		t.Fatalf("expected pointer-to-scalar to validate, got %v", err)
	}
	if len(plan.bindings) != 1 || plan.bindings[0].kind != bindHeader {
		t.Errorf("unexpected plan: %+v", plan.bindings)
	}
}

// ─── validatePathCoverage — empty path short-circuits ───────────────────────

func TestInspectRequestType_EmptyPathNoPlaceholders(t *testing.T) {
	resetPlanCache()
	type req struct {
		Q string `http:"query,q"`
	}
	// Empty path with no path-tagged fields → validatePathCoverage returns nil.
	if _, err := inspectRequestType(reflect.TypeOf(req{}), ""); err != nil {
		t.Fatalf("empty path with no placeholders should be valid, got %v", err)
	}
}

// ─── BuildRequest — error branches ──────────────────────────────────────────

func TestBuildRequest_InspectError(t *testing.T) {
	type req struct {
		A payloadStruct `http:"body,json"`
		B payloadStruct `http:"body,json"`
	}
	_, err := BuildRequest(context.Background(), "http://x",
		EndpointMeta{Method: "POST", Path: "/x"}, req{})
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("expected inspect error to surface from BuildRequest, got %v", err)
	}
}

func TestBuildRequest_QueryCSVElementNotScalar(t *testing.T) {
	type req struct {
		Items []payloadStruct `http:"query,items,csv"`
	}
	_, err := BuildRequest(context.Background(), "http://x",
		EndpointMeta{Method: "GET", Path: "/x"}, req{Items: []payloadStruct{{Name: "a"}}})
	if err == nil || !strings.Contains(err.Error(), "csv") {
		t.Fatalf("expected csv element conversion error, got %v", err)
	}
}

func TestBuildRequest_QueryMultiElementNotScalar(t *testing.T) {
	type req struct {
		Items []payloadStruct `http:"query,items,multi"`
	}
	_, err := BuildRequest(context.Background(), "http://x",
		EndpointMeta{Method: "GET", Path: "/x"}, req{Items: []payloadStruct{{Name: "a"}}})
	if err == nil || !strings.Contains(err.Error(), "multi") {
		t.Fatalf("expected multi element conversion error, got %v", err)
	}
}

func TestBuildRequest_BodyUnknownCodec(t *testing.T) {
	type req struct {
		Body payloadStruct `http:"body,bogus"`
	}
	// The body codec name is validated at tag-parse time, so BuildRequest
	// surfaces the rejection from its inspection step.
	_, err := BuildRequest(context.Background(), "http://x",
		EndpointMeta{Method: "POST", Path: "/x"}, req{Body: payloadStruct{Name: "n"}})
	if err == nil || !strings.Contains(err.Error(), "is not one of") {
		t.Fatalf("expected unknown body codec error, got %v", err)
	}
}

func TestBuildRequest_BodyEncodeError(t *testing.T) {
	type req struct {
		Body chan int `http:"body,json"`
	}
	_, err := BuildRequest(context.Background(), "http://x",
		EndpointMeta{Method: "POST", Path: "/x"}, req{Body: make(chan int)})
	if err == nil {
		t.Fatalf("expected json encode error for a channel body, got nil")
	}
}

func TestBuildRequest_PathAlreadyHasQuery(t *testing.T) {
	type req struct {
		Q string `http:"query,q"`
	}
	httpReq, err := BuildRequest(context.Background(), "http://x",
		EndpointMeta{Method: "GET", Path: "/search?fixed=1"}, req{Q: "go"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	u := httpReq.URL.String()
	if !strings.Contains(u, "fixed=1") || !strings.Contains(u, "q=go") {
		t.Errorf("expected both query params, got %q", u)
	}
	if !strings.Contains(u, "&") {
		t.Errorf("expected '&' joining the existing query, got %q", u)
	}
}

func TestBuildRequest_InvalidMethod(t *testing.T) {
	type req struct {
		Q string `http:"query,q"`
	}
	_, err := BuildRequest(context.Background(), "http://x",
		EndpointMeta{Method: "BAD METHOD", Path: "/x"}, req{Q: "go"})
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("expected NewRequestWithContext error for invalid method, got %v", err)
	}
}

// ─── DecodeResponse — codec decode error on the body-tag path ───────────────

func TestDecodeResponse_BodyTagDecodeError(t *testing.T) {
	type resp struct {
		Body payloadStruct `http:"body,json"`
	}
	httpResp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{not json`)),
	}
	var out resp
	err := DecodeResponse(httpResp, EndpointMeta{ResponseCodec: "json"}, &out)
	if err == nil {
		t.Fatalf("expected decode error on malformed body, got nil")
	}
}
