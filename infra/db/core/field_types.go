package core

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The CLOSED set of persistable field types — exactly what the framework
// composes on EVERY supported relational engine (write bind, criteria bind, the
// schema-driven scan; see table-schema.html "Supported column shapes"). Declaring a
// Field whose Go type is outside this set is a BOOT FAIL at the declaration
// call: with the type-driven identity contract the field's type IS its
// storage declaration, so an unknown type would mean a silent driver
// lottery on some engine — the panic converts that into a
// deterministic construction error that teaches the fix.
//
// Matching is by IDENTICAL type, never by kind — drivers diverge on named
// types, so the closed set stays literal. A named type is accepted ONLY as a
// value object (Field() detects a ValueObject/EnumValueObject and validates its
// UNDERLYING against this set, since the write path unwraps it to the underlying
// and the read path reconstructs it — the driver never binds the named type); a
// non-VO named type is still rejected. json.RawMessage is the one sanctioned
// alias (a JSON payload column, text on MySQL / json(b) on Postgres).
var supportedFieldTypes = func() map[reflect.Type]struct{} {
	types := []reflect.Type{
		reflect.TypeOf(domain.ID{}), reflect.TypeOf((*domain.ID)(nil)),
		reflect.TypeOf(""), reflect.TypeOf((*string)(nil)),
		reflect.TypeOf(false), reflect.TypeOf((*bool)(nil)),
		reflect.TypeOf(int(0)), reflect.TypeOf((*int)(nil)),
		reflect.TypeOf(int16(0)), reflect.TypeOf((*int16)(nil)),
		reflect.TypeOf(int32(0)), reflect.TypeOf((*int32)(nil)),
		reflect.TypeOf(int64(0)), reflect.TypeOf((*int64)(nil)),
		reflect.TypeOf(float32(0)), reflect.TypeOf((*float32)(nil)),
		reflect.TypeOf(float64(0)), reflect.TypeOf((*float64)(nil)),
		reflect.TypeOf(time.Time{}), reflect.TypeOf((*time.Time)(nil)),
		reflect.TypeOf([]byte(nil)),
		reflect.TypeOf(json.RawMessage(nil)),
	}
	m := make(map[reflect.Type]struct{}, len(types))
	for _, t := range types {
		m[t] = struct{}{}
	}
	return m
}()

var (
	uuidType    = reflect.TypeOf(uuid.UUID{})
	uuidPtrType = reflect.TypeOf((*uuid.UUID)(nil))
)

// supportedFieldTypeNames renders the closed set for the panic message,
// deterministically ordered.
func supportedFieldTypeNames() string {
	names := make([]string, 0, len(supportedFieldTypes))
	for t := range supportedFieldTypes {
		names = append(names, t.String())
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// mustSupportedFieldType is the boot guard behind every Field(...) on a
// type-anchored schema: the declared Go type must belong to the closed
// portable set above. A google/uuid.UUID field gets the dedicated hint —
// identity fields are domain.ID (google/uuid is the framework's internal
// engine, never a field type).
func mustSupportedFieldType(table, goName string, ft reflect.Type) {
	if _, ok := supportedFieldTypes[ft]; ok {
		return
	}
	if ft == uuidType || ft == uuidPtrType {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): field %q is typed %s — identity fields are declared domain.ID "+
				"(*domain.ID when nullable): the domain.ID type drives each dialect's native id form "+
				"(e.g. UUID on Postgres, BINARY(16) on MySQL); google/uuid is the framework's "+
				"internal engine, never a persisted field type",
			table, goName, ft,
		))
	}
	panic(fmt.Sprintf(
		"infra.TableSchema(%s): field %q is typed %s — not in the closed set of persistable field types "+
			"the framework composes on every supported engine (see table-schema.html \"Supported column shapes\"). "+
			"Supported: %s. Map the field to one of these (an enum to its underlying string/int, a JSON "+
			"payload to []byte/json.RawMessage, an id to domain.ID); nullable ⇒ the pointer form.",
		table, goName, ft, supportedFieldTypeNames(),
	))
}
