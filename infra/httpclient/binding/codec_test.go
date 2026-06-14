package binding

import (
	"strings"
	"testing"
)

type sampleBody struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestJsonCodec_RoundTrip(t *testing.T) {
	c := jsonCodec{}
	in := sampleBody{Name: "Ada", Age: 36}
	data, ct, err := c.Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var out sampleBody
	if err := c.Decode(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Errorf("round-trip lost data: got %+v, want %+v", out, in)
	}
}

func TestJsonCodec_EncodeNil_EmptyBody(t *testing.T) {
	data, ct, err := (jsonCodec{}).Encode(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if data != nil {
		t.Errorf("nil encode should yield nil bytes; got %q", data)
	}
	if ct != "" {
		t.Errorf("nil encode should yield empty content-type; got %q", ct)
	}
}

func TestJsonCodec_DecodeEmpty_NoError(t *testing.T) {
	var out sampleBody
	if err := (jsonCodec{}).Decode(nil, &out); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
}

func TestJsonCodec_DecodeBad_Error(t *testing.T) {
	var out sampleBody
	if err := (jsonCodec{}).Decode([]byte("not json"), &out); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestCodecByName_JSON(t *testing.T) {
	c, err := codecByName("json")
	if err != nil {
		t.Fatalf("codecByName(json): %v", err)
	}
	if _, ok := c.(jsonCodec); !ok {
		t.Errorf("codecByName(json) returned %T, want jsonCodec", c)
	}
}

func TestCodecByName_EmptyDefaultsToJSON(t *testing.T) {
	c, err := codecByName("")
	if err != nil {
		t.Fatalf("codecByName(\"\"): %v", err)
	}
	if _, ok := c.(jsonCodec); !ok {
		t.Errorf("default codec = %T, want jsonCodec", c)
	}
}

func TestCodecByName_XML(t *testing.T) {
	c, err := codecByName("xml")
	if err != nil {
		t.Fatalf("xml: %v", err)
	}
	if _, ok := c.(xmlCodec); !ok {
		t.Errorf("got %T, want xmlCodec", c)
	}
}

func TestCodecByName_FormURLEncoded(t *testing.T) {
	c, err := codecByName("form-urlencoded")
	if err != nil {
		t.Fatalf("form-urlencoded: %v", err)
	}
	if _, ok := c.(formCodec); !ok {
		t.Errorf("got %T, want formCodec", c)
	}
}

func TestCodecByName_Unknown(t *testing.T) {
	_, err := codecByName("yaml")
	if err == nil {
		t.Fatal("expected error for unknown codec")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention not registered; got %v", err)
	}
}
