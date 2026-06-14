package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// validateExample runs the boot-time shape check on one Example. The
// contract is two-pronged:
//
//   - Marshal the Value into JSON. Anything not json-marshalable
//     (channels, functions, cyclic graphs) returns an error here.
//   - When strictTyped && wantType != nil, unmarshal the bytes back
//     into a freshly allocated instance of wantType with
//     DisallowUnknownFields. Catches typos like a wrong field name
//     silently producing zero-value output.
//
// json.RawMessage is treated specially: the Marshal step would
// re-encode the bytes (escaping the embedded JSON), so we pass them
// through verbatim and only check that the payload is structurally
// valid JSON.
//
// Returns the resulting JSON bytes so the caller can hand them
// straight to the renderer without a second Marshal pass. Mount /
// MountRaw turn any returned error into a panic — every diagnostic
// threads the route + example identifier so the maintainer sees
// exactly which declaration is broken.
func validateExample(value any, wantType reflect.Type, strictTyped bool) (json.RawMessage, error) {
	if value == nil {
		return nil, nil // empty example → remove canonical default (consumer opt-out)
	}
	var raw json.RawMessage
	switch v := value.(type) {
	case json.RawMessage:
		raw = append(json.RawMessage(nil), v...)
	case []byte:
		raw = append(json.RawMessage(nil), v...)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("not JSON-marshalable: %w", err)
		}
		raw = encoded
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("value is not valid JSON")
	}
	if strictTyped && wantType != nil {
		instance := reflect.New(wantType).Interface()
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(instance); err != nil {
			return nil, fmt.Errorf("does not match declared type %s: %w", typeLabel(wantType), err)
		}
	}
	return raw, nil
}

// validateExampleMap applies validateExample to every entry of m,
// returning the raw-bytes form (so the renderer skips a second Marshal)
// alongside the original Example fields preserved verbatim. Panics
// produced by the boot path carry the routeID + statusOrSlot pair so
// the diagnostic is unambiguous in a service with many routes.
//
// strictTyped controls whether the decode step runs (true for
// requests + success-status responses; false for error-status
// responses and for raw routes whose Type is nil — these only need
// JSON-validity).
func validateExampleMap(
	m map[string]Example,
	wantType reflect.Type,
	strictTyped bool,
	routeID, slotID string,
) map[string]rawExample {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]rawExample, len(m))
	for name, ex := range m {
		raw, err := validateExample(ex.Value, wantType, strictTyped)
		if err != nil {
			panic(fmt.Sprintf("openapi: example %q on %s %s: %v", name, routeID, slotID, err))
		}
		out[name] = rawExample{
			Summary:     ex.Summary,
			Description: ex.Description,
			Raw:         raw,
		}
	}
	return out
}

// rawExample is the post-validation shape Mount stores alongside the
// route. Raw is nil exclusively when the consumer declared an example
// with a nil Value — the renderer's contract is to interpret that as
// "remove the framework's canonical entry from this slot", which is
// only meaningful for the auto-merged `"default"` key.
type rawExample struct {
	Summary     string
	Description string
	Raw         json.RawMessage
}

// renderExample produces the map shape OpenAPI 3.x consumes — entry of
// the `examples` plural map. Empty summary/description fields are
// omitted so the rendered spec stays compact for the common case of
// "value-only" declarations.
func renderExample(ex rawExample) map[string]any {
	out := map[string]any{}
	if ex.Summary != "" {
		out["summary"] = ex.Summary
	}
	if ex.Description != "" {
		out["description"] = ex.Description
	}
	if ex.Raw != nil {
		out["value"] = jsonValue(ex.Raw)
	}
	return out
}

// jsonValue decodes raw bytes back into an interface{} so the spec
// assembler can emit it as native JSON instead of a base64 string (the
// default encoding/json round-trip when a []byte appears in the
// output). Decoding is best-effort: malformed JSON would have been
// rejected by validateExample, so this branch only fails on a
// programmer error.
func jsonValue(raw json.RawMessage) any {
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		// Should not happen — validateExample guarantees raw is valid JSON.
		return string(raw)
	}
	return out
}
