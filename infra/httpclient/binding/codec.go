package binding

import (
	"encoding/json"
	"fmt"
)

// Codec is the request/response body encoder pair. JSON is the only
// implementation today; XML and form-urlencoded join the registry in the
// codec expansion phase. The registry is intentionally package-private — no
// consumer-facing RegisterCodec exists, so codecs cannot be added without a
// framework change.
type Codec interface {
	// Encode marshals v into a byte slice and reports the Content-Type the
	// request must advertise. nil v yields an empty body and empty
	// Content-Type so the caller can omit the body and the header.
	Encode(v any) (body []byte, contentType string, err error)

	// Decode unmarshals data into the value pointed to by v.
	Decode(data []byte, v any) error
}

// jsonCodec is the default body codec. Backed by encoding/json so behavior
// matches the rest of the framework's wire surfaces (RespondFromResult,
// audit serialization, etc.).
type jsonCodec struct{}

func (jsonCodec) Encode(v any) ([]byte, string, error) {
	if v == nil {
		return nil, "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, "", fmt.Errorf("encode json: %w", err)
	}
	return b, "application/json", nil
}

func (jsonCodec) Decode(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	return nil
}

// codecByName resolves a codec name (as declared in YAML or on a body tag)
// to a Codec implementation. Unknown names produce errors mentioning the
// phase that will introduce them, so a stray "xml" in YAML fails fast at
// inspection time with a message the operator can act on.
func codecByName(name string) (Codec, error) {
	switch name {
	case "json", "":
		return jsonCodec{}, nil
	case "xml":
		return xmlCodec{}, nil
	case "form-urlencoded":
		return formCodec{}, nil
	default:
		return nil, fmt.Errorf("codec %q is not registered", name)
	}
}
