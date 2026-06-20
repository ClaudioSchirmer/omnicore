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
