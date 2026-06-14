package binding

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func newResp(status int, headers map[string]string, body string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestDecodeResponse_BodyOnly(t *testing.T) {
	type out struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	resp := newResp(200, nil, `{"name":"Ada","age":36}`)
	var got out
	if err := DecodeResponse(resp, meta("GET", "/x", nil), &got); err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("got %+v", got)
	}
}

func TestDecodeResponse_BodyTagAndHeaderTag(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	type out struct {
		ETag string `http:"header,ETag"`
		Body body   `http:"body,json"`
	}
	resp := newResp(200, map[string]string{"ETag": "v1"}, `{"name":"Ada"}`)
	var got out
	if err := DecodeResponse(resp, meta("GET", "/x", nil), &got); err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.ETag != "v1" {
		t.Errorf("ETag = %q", got.ETag)
	}
	if got.Body.Name != "Ada" {
		t.Errorf("body lost: %+v", got.Body)
	}
}

func TestDecodeResponse_HeaderOnly_BodyIgnored(t *testing.T) {
	type out struct {
		Location string `http:"header,Location"`
	}
	resp := newResp(201, map[string]string{"Location": "/users/1"}, "ignored body")
	var got out
	if err := DecodeResponse(resp, meta("POST", "/x", nil), &got); err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.Location != "/users/1" {
		t.Errorf("Location = %q", got.Location)
	}
}

func TestDecodeResponse_EmptyStruct_DiscardsBody(t *testing.T) {
	var got struct{}
	resp := newResp(204, nil, `garbage that should not be parsed`)
	if err := DecodeResponse(resp, meta("DELETE", "/x", nil), &got); err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
}

func TestDecodeResponse_NilResponse(t *testing.T) {
	var got struct{}
	if err := DecodeResponse(nil, meta("GET", "/x", nil), &got); err == nil {
		t.Fatal("expected error for nil response")
	}
}

func TestDecodeResponse_NotPointer(t *testing.T) {
	var got struct{}
	resp := newResp(200, nil, "")
	if err := DecodeResponse(resp, meta("GET", "/x", nil), got); err == nil {
		t.Fatal("expected error for non-pointer out")
	}
}

func TestDecodeResponse_BadJSON(t *testing.T) {
	type out struct {
		Name string `json:"name"`
	}
	resp := newResp(200, nil, `not json`)
	var got out
	if err := DecodeResponse(resp, meta("GET", "/x", nil), &got); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestDecodeResponse_BodyClosedAlways(t *testing.T) {
	type out struct {
		Name string `json:"name"`
	}
	closer := &countingCloser{Reader: bytes.NewReader([]byte(`{"name":"Ada"}`))}
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       closer,
	}
	var got out
	if err := DecodeResponse(resp, meta("GET", "/x", nil), &got); err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if closer.closes != 1 {
		t.Errorf("body closed %d times, want 1", closer.closes)
	}
}

type countingCloser struct {
	io.Reader
	closes int
}

func (c *countingCloser) Close() error {
	c.closes++
	return nil
}

func TestDecodeResponse_NilOut(t *testing.T) {
	resp := newResp(204, nil, "")
	if err := DecodeResponse(resp, meta("GET", "/x", nil), nil); err != nil {
		t.Fatalf("DecodeResponse(nil out) should succeed and discard: %v", err)
	}
}
