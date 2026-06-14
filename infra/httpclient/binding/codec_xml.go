package binding

import (
	"encoding/xml"
	"fmt"
)

// xmlCodec encodes/decodes XML bodies via encoding/xml. SOAP-style endpoints
// declare `requestCodec: xml` and `responseCodec: xml`; the resulting
// Content-Type is application/xml.
type xmlCodec struct{}

func (xmlCodec) Encode(v any) ([]byte, string, error) {
	if v == nil {
		return nil, "", nil
	}
	b, err := xml.Marshal(v)
	if err != nil {
		return nil, "", fmt.Errorf("encode xml: %w", err)
	}
	return b, "application/xml", nil
}

func (xmlCodec) Decode(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := xml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode xml: %w", err)
	}
	return nil
}
