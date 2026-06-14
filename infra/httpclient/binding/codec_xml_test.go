package binding

import (
	"encoding/xml"
	"strings"
	"testing"
)

type xmlPayload struct {
	XMLName xml.Name `xml:"Customer"`
	ID      string   `xml:"id,attr"`
	Name    string   `xml:"Name"`
}

func TestXmlCodec_RoundTrip(t *testing.T) {
	in := xmlPayload{ID: "42", Name: "Ada"}
	data, ct, err := (xmlCodec{}).Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if ct != "application/xml" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(string(data), `id="42"`) || !strings.Contains(string(data), "<Name>Ada</Name>") {
		t.Errorf("serialized XML missing fields: %s", data)
	}
	var out xmlPayload
	if err := (xmlCodec{}).Decode(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != "42" || out.Name != "Ada" {
		t.Errorf("round-trip: %+v", out)
	}
}

func TestXmlCodec_EmptyDecode_NoError(t *testing.T) {
	var out xmlPayload
	if err := (xmlCodec{}).Decode(nil, &out); err != nil {
		t.Errorf("empty decode: %v", err)
	}
}

func TestXmlCodec_BadXML_Errors(t *testing.T) {
	var out xmlPayload
	if err := (xmlCodec{}).Decode([]byte("<bad><unclosed"), &out); err == nil {
		t.Error("expected error for malformed XML")
	}
}

func TestXmlCodec_EncodeNil_Empty(t *testing.T) {
	data, ct, err := (xmlCodec{}).Encode(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if data != nil || ct != "" {
		t.Errorf("nil encode should yield empty; got (%q, %q)", data, ct)
	}
}
