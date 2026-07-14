package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The closed persistable-type set — the boot guard behind every Field(...):
// every type the framework composes on every supported engine passes;
// anything else is
// a construction panic that teaches the fix, with the dedicated domain.ID
// hint for google/uuid fields and a pointer to table-schema.html (the
// canonical home of the supported set) for everything else.

type allSupportedFields struct {
	Ref   domain.ID
	OptR  *domain.ID
	S     string
	OptS  *string
	B     bool
	OptB  *bool
	I     int
	OptI  *int
	I16   int16
	OptI6 *int16
	I32   int32
	OptI3 *int32
	I64   int64
	OptI4 *int64
	F32   float32
	OptF2 *float32
	F64   float64
	OptF4 *float64
	T     time.Time
	OptT  *time.Time
	Raw   []byte
	JSON  json.RawMessage
}

func TestFieldTypes_ClosedSetAccepts(t *testing.T) {
	s := NewTableSchema[*allSupportedFields]("t").PK("id")
	cols := []struct{ g, c string }{
		{"Ref", "ref"}, {"OptR", "opt_r"}, {"S", "s"}, {"OptS", "opt_s"},
		{"B", "b"}, {"OptB", "opt_b"}, {"I", "i"}, {"OptI", "opt_i"},
		{"I16", "i16"}, {"OptI6", "opt_i6"}, {"I32", "i32"}, {"OptI3", "opt_i3"},
		{"I64", "i64"}, {"OptI4", "opt_i4"}, {"F32", "f32"}, {"OptF2", "opt_f2"},
		{"F64", "f64"}, {"OptF4", "opt_f4"}, {"T", "ts"}, {"OptT", "opt_ts"},
		{"Raw", "raw"}, {"JSON", "payload"},
	}
	for _, c := range cols {
		s.Field(c.g, c.c) // any panic fails the test
	}
}

func mustPanicContaining(t *testing.T, name, fragment string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("%s: expected panic", name)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, fragment) {
			t.Fatalf("%s: panic %q, want it to mention %q", name, msg, fragment)
		}
	}()
	fn()
}

func TestFieldTypes_GoogleUUIDHintsDomainID(t *testing.T) {
	type withUUID struct{ Ref uuid.UUID }
	type withUUIDPtr struct{ Ref *uuid.UUID }

	mustPanicContaining(t, "uuid.UUID", "domain.ID", func() {
		NewTableSchema[*withUUID]("t").PK("id").Field("Ref", "ref")
	})
	mustPanicContaining(t, "*uuid.UUID", "domain.ID", func() {
		NewTableSchema[*withUUIDPtr]("t").PK("id").Field("Ref", "ref")
	})
}

func TestFieldTypes_UnknownTypesPointAtTheDocs(t *testing.T) {
	type status string
	type withEnum struct{ Status status }
	type withSlice struct{ Tags []string }
	type withStruct struct{ Nested struct{ X int } }
	type withUint struct{ N uint64 }

	cases := []struct {
		name string
		fn   func()
	}{
		{"named string (enum)", func() { NewTableSchema[*withEnum]("t").PK("id").Field("Status", "status") }},
		{"slice (PG-only array)", func() { NewTableSchema[*withSlice]("t").PK("id").Field("Tags", "tags") }},
		{"nested struct", func() { NewTableSchema[*withStruct]("t").PK("id").Field("Nested", "nested") }},
		{"uint64", func() { NewTableSchema[*withUint]("t").PK("id").Field("N", "n") }},
	}
	for _, c := range cases {
		mustPanicContaining(t, c.name, "table-schema.html", c.fn)
	}
}

// The shared base is type-less at declaration — its fields' Go types resolve
// when a ROLE anchors them (.SharedBase), so the closed-set guard fires there.
func TestFieldTypes_SharedBaseFieldsValidatedAtRoleAnchor(t *testing.T) {
	type badRole struct {
		Email uuid.UUID // shared-base field carried by the role, wrong type
		Name  string
	}
	base := NewSharedBase("persons").PK("id").Field("Email", "email").NaturalKey("email")
	mustPanicContaining(t, "shared-base field via role anchor", "domain.ID", func() {
		NewTableSchema[*badRole]("students").
			PK("id").
			Field("Name", "name").
			SharedBase(base, "person_id")
	})
}
