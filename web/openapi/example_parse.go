package openapi

import (
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// parseExampleTag converts the raw `example:"..."` struct-tag string into the
// Go value the schema's Example field should carry. Conversion is strict and
// type-driven: a value the JSON encoder cannot represent in the property's
// declared type fails fast at boot, so a typo never produces a silently
// degraded Swagger UI render.
//
// Supported types follow the schema generator's own coverage of scalar /
// well-known shapes:
//
//	string                      → literal
//	bool                        → strconv.ParseBool
//	int / int8 / int16 / int32  → strconv.ParseInt(_, 10, 32)
//	int64                       → strconv.ParseInt(_, 10, 64)
//	uint / uint8 / uint16 / 32  → strconv.ParseUint(_, 10, 32)
//	uint64                      → strconv.ParseUint(_, 10, 64)
//	float32 / float64           → strconv.ParseFloat
//	uuid.UUID / domain.ID       → uuid.Parse — emits canonical string
//	time.Time                   → time.Parse(time.RFC3339Nano) — emits original string
//	pointer to any of the above → recurse on the element type
//
// Composite types (struct, slice, array, map, interface) deliberately return
// an error. The map-based path (Doc.RequestExamples / Doc.ResponseExamples)
// covers shapes a single tag string cannot express coherently — nested
// arrays of structs, multi-field objects with cross-field meaning, etc.
//
// The emitted value is always the JSON-marshalable form of the parsed input:
// uuids and timestamps surface as their canonical string representation so
// encoding/json round-trips them verbatim into the OpenAPI document.
func parseExampleTag(t reflect.Type, raw string) (any, error) {
	if t == nil {
		return nil, fmt.Errorf("nil type")
	}
	if t.Kind() == reflect.Pointer {
		return parseExampleTag(t.Elem(), raw)
	}
	if t == reflect.TypeOf(uuid.UUID{}) {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid uuid: %w", err)
		}
		return parsed.String(), nil
	}
	if t == reflect.TypeOf(domain.ID{}) {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid uuid: %w", err)
		}
		return parsed.String(), nil
	}
	if t == reflect.TypeOf(time.Time{}) {
		if _, err := time.Parse(time.RFC3339Nano, raw); err != nil {
			return nil, fmt.Errorf("invalid RFC3339 timestamp: %w", err)
		}
		return raw, nil
	}
	switch t.Kind() {
	case reflect.String:
		return raw, nil
	case reflect.Bool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid bool: %w", err)
		}
		return v, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		v, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid int: %w", err)
		}
		return int32(v), nil
	case reflect.Int64:
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid int64: %w", err)
		}
		return v, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		v, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid uint: %w", err)
		}
		return uint32(v), nil
	case reflect.Uint64:
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid uint64: %w", err)
		}
		return v, nil
	case reflect.Float32:
		v, err := strconv.ParseFloat(raw, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid float32: %w", err)
		}
		return float32(v), nil
	case reflect.Float64:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float64: %w", err)
		}
		return v, nil
	}
	return nil, fmt.Errorf("type %s not supported by example: tag — use Doc.RequestExamples / Doc.ResponseExamples for composite shapes", typeLabel(t))
}

// typeLabel formats a reflect.Type for panic diagnostics. Named types
// surface as `<pkg>.<Name>`; anonymous types fall back to their Kind so the
// message stays readable for inline structs / interface{} fields.
func typeLabel(t reflect.Type) string {
	if t.Name() != "" {
		if pkg := t.PkgPath(); pkg != "" {
			return pkg + "." + t.Name()
		}
		return t.Name()
	}
	return t.Kind().String()
}
