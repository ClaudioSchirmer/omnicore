package binding

import (
	"net/url"
	"strings"
	"testing"
)

func TestFormCodec_Encode_URLValues(t *testing.T) {
	v := url.Values{}
	v.Set("a", "1")
	v.Set("b", "two words")
	data, ct, err := (formCodec{}).Encode(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
	s := string(data)
	if !strings.Contains(s, "a=1") || !strings.Contains(s, "b=two+words") {
		t.Errorf("encode output: %s", s)
	}
}

type tokenReq struct {
	GrantType    string `form:"grant_type"`
	ClientID     string `form:"client_id"`
	ClientSecret string `form:"client_secret"`
	Scope        string `form:"scope,omitempty"`
}

func TestFormCodec_Encode_StructWithTags(t *testing.T) {
	in := tokenReq{GrantType: "client_credentials", ClientID: "id", ClientSecret: "s"}
	data, _, err := (formCodec{}).Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "grant_type=client_credentials") {
		t.Errorf("grant_type missing: %s", s)
	}
	if strings.Contains(s, "scope") {
		t.Errorf("omitempty failed; got: %s", s)
	}
}

func TestFormCodec_Encode_StructWithoutTags(t *testing.T) {
	type plain struct {
		Name string
		Age  int
	}
	data, _, err := (formCodec{}).Encode(plain{Name: "Ada", Age: 36})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "name=Ada") || !strings.Contains(s, "age=36") {
		t.Errorf("lower-cased field names missing: %s", s)
	}
}

func TestFormCodec_Decode_IntoURLValues(t *testing.T) {
	data := []byte("a=1&b=two")
	var out url.Values
	if err := (formCodec{}).Decode(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Get("a") != "1" || out.Get("b") != "two" {
		t.Errorf("got %v", out)
	}
}

func TestFormCodec_Decode_IntoStruct(t *testing.T) {
	type out struct {
		Grant string `form:"grant_type"`
		ID    string `form:"client_id"`
	}
	var got out
	if err := (formCodec{}).Decode([]byte("grant_type=cc&client_id=x"), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Grant != "cc" || got.ID != "x" {
		t.Errorf("got %+v", got)
	}
}

func TestFormCodec_EmptyDecode_NoError(t *testing.T) {
	var v url.Values
	if err := (formCodec{}).Decode(nil, &v); err != nil {
		t.Errorf("empty decode: %v", err)
	}
}

func TestFormCodec_EncodeNil_Empty(t *testing.T) {
	data, ct, err := (formCodec{}).Encode(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if data != nil || ct != "" {
		t.Errorf("nil encode should yield empty")
	}
}

func TestFormCodec_DashTagSkipped(t *testing.T) {
	type x struct {
		Public  string `form:"public"`
		Skipped string `form:"-"`
	}
	data, _, err := (formCodec{}).Encode(x{Public: "v", Skipped: "secret"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "public=v") {
		t.Errorf("public missing: %s", s)
	}
	if strings.Contains(s, "secret") {
		t.Errorf("- tag should skip: %s", s)
	}
}
