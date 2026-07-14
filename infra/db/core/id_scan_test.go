package core

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The identity scan proxies: the auto-scan detects domain.ID / *domain.ID
// fields by their reflected type (the type IS the declaration) and substitutes
// a sql.Scanner target that owns the decode — 16 raw bytes (MySQL BINARY(16))
// and text forms (pgx uuid / CHAR(36)) both restore to the canonical string,
// and SQL NULL resolves explicitly: nil for the pointer field, a loud error
// for the value field.

type idScanFixture struct {
	Ref  domain.ID
	Opt  *domain.ID
	Name string
}

func TestScanTargetFor_Substitution(t *testing.T) {
	v := reflect.ValueOf(&idScanFixture{}).Elem()

	if _, ok := scanTargetFor(v.Field(0)).(sql.Scanner); !ok {
		t.Errorf("domain.ID field target = %T, want a sql.Scanner proxy", scanTargetFor(v.Field(0)))
	}
	if _, ok := scanTargetFor(v.Field(1)).(sql.Scanner); !ok {
		t.Errorf("*domain.ID field target = %T, want a sql.Scanner proxy", scanTargetFor(v.Field(1)))
	}
	if _, ok := scanTargetFor(v.Field(2)).(*string); !ok {
		t.Errorf("string field target = %T, want the plain *string address", scanTargetFor(v.Field(2)))
	}
}

func TestIDScanTarget(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")

	scan := func(t *testing.T, src any) domain.ID {
		t.Helper()
		var dst domain.ID
		if err := (&idScanTarget{dst: &dst}).Scan(src); err != nil {
			t.Fatalf("Scan(%T): %v", src, err)
		}
		return dst
	}

	t.Run("16 raw bytes restore the canonical uuid (MySQL BINARY(16))", func(t *testing.T) {
		if got := scan(t, u[:]); got.Value() != u.String() {
			t.Errorf("got %q, want %q", got.Value(), u.String())
		}
	})

	t.Run("16-byte array restores the canonical uuid", func(t *testing.T) {
		var a [16]byte
		copy(a[:], u[:])
		if got := scan(t, a); got.Value() != u.String() {
			t.Errorf("got %q, want %q", got.Value(), u.String())
		}
	})

	t.Run("text []byte passes through (CHAR(36) on MySQL)", func(t *testing.T) {
		if got := scan(t, []byte(u.String())); got.Value() != u.String() {
			t.Errorf("got %q, want %q", got.Value(), u.String())
		}
	})

	t.Run("string passes through (pgx uuid text)", func(t *testing.T) {
		if got := scan(t, u.String()); got.Value() != u.String() {
			t.Errorf("got %q, want %q", got.Value(), u.String())
		}
	})

	t.Run("SQL NULL is a loud error on the non-nullable field", func(t *testing.T) {
		var dst domain.ID
		if err := (&idScanTarget{dst: &dst}).Scan(nil); err == nil {
			t.Fatal("expected an error for NULL into domain.ID — the field should be *domain.ID")
		}
	})

	t.Run("unsupported driver type errors", func(t *testing.T) {
		var dst domain.ID
		if err := (&idScanTarget{dst: &dst}).Scan(42); err == nil {
			t.Fatal("expected an error for an int driver value")
		}
	})
}

func TestNullableIDScanTarget(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")

	t.Run("SQL NULL restores nil", func(t *testing.T) {
		pre := domain.NewID("stale")
		dst := &pre
		if err := (&nullableIDScanTarget{dst: &dst}).Scan(nil); err != nil {
			t.Fatalf("Scan(nil): %v", err)
		}
		if dst != nil {
			t.Errorf("dst = %v, want nil", dst.Value())
		}
	})

	t.Run("16 raw bytes restore &canonical", func(t *testing.T) {
		var dst *domain.ID
		if err := (&nullableIDScanTarget{dst: &dst}).Scan(u[:]); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if dst == nil || dst.Value() != u.String() {
			t.Errorf("dst = %v, want &%q", dst, u.String())
		}
	})

	t.Run("text restores &value", func(t *testing.T) {
		var dst *domain.ID
		if err := (&nullableIDScanTarget{dst: &dst}).Scan(u.String()); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if dst == nil || dst.Value() != u.String() {
			t.Errorf("dst = %v, want &%q", dst, u.String())
		}
	})

	t.Run("unsupported driver type errors", func(t *testing.T) {
		var dst *domain.ID
		if err := (&nullableIDScanTarget{dst: &dst}).Scan(3.14); err == nil {
			t.Fatal("expected an error for a float driver value")
		}
	})
}

// IDKindOf derives the identity typing from the Go struct: domain.ID →
// IDValue, *domain.ID → IDPointer, anything else → IDNone — and the managed PK
// slot ("ID") is ALWAYS IDValue (framework-stored in the dialect's native id
// form), so bare-string PK probes bind like the typed ByID does.
func TestIDKindOf(t *testing.T) {
	s := NewTableSchema[*idScanFixture]("t").
		PK("id").
		Field("Ref", "ref").
		Field("Opt", "opt").
		Field("Name", "name")

	cases := []struct {
		field string
		want  IDKind
	}{
		{"Ref", IDValue},
		{"Opt", IDPointer},
		{"Name", IDNone},
		{"ID", IDValue},  // managed PK slot — always identity
		{"Nope", IDNone}, // unknown — the translator rejects it separately
	}
	for _, c := range cases {
		if got := s.IDKindOf(c.field); got != c.want {
			t.Errorf("IDKindOf(%q) = %v, want %v", c.field, got, c.want)
		}
	}
}
