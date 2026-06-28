//go:build mysql

package mysql

import (
	"bytes"
	"errors"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestIsUniqueViolation(t *testing.T) {
	d := mysqlDialect{}

	t.Run("plain key with table prefix", func(t *testing.T) {
		err := &driver.MySQLError{Number: 1062, Message: "Duplicate entry 'a@b.com' for key 'flat_persons.uniq_email'"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("value contains the for-key marker (injection)", func(t *testing.T) {
		// A duplicated value of `a' for key 'b` is printed unescaped, so the
		// message carries two "for key '" segments. The real index is the LAST
		// one; strings.Index would have locked onto the value's fake "b".
		err := &driver.MySQLError{Number: 1062, Message: "Duplicate entry 'a' for key 'b' for key 'uniq_email'"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "uniq_email" {
			t.Fatalf("got (%q,%v), want (uniq_email,true)", key, ok)
		}
	})

	t.Run("1062 without a parseable key", func(t *testing.T) {
		err := &driver.MySQLError{Number: 1062, Message: "Duplicate entry — no key clause"}
		key, ok := d.IsUniqueViolation(err)
		if !ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',true)", key, ok)
		}
	})

	t.Run("non-1062 mysql error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(&driver.MySQLError{Number: 1045, Message: "access denied"}); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})

	t.Run("non-mysql error", func(t *testing.T) {
		if key, ok := d.IsUniqueViolation(errors.New("plain")); ok || key != "" {
			t.Fatalf("got (%q,%v), want ('',false)", key, ok)
		}
	})
}

// TestILikeClause proves the case-insensitive LIKE renders LOWER on both sides so
// criteria.ILike/Contains/StartsWith/EndsWith are case-insensitive on ANY column
// collation (Postgres ILIKE parity), not only under a CI collation.
func TestILikeClause(t *testing.T) {
	got := mysqlDialect{}.ILikeClause("`name`", "?")
	if want := "LOWER(`name`) LIKE LOWER(?)"; got != want {
		t.Fatalf("ILikeClause = %q, want %q", got, want)
	}
}

// TestEncodeArg covers the value codec the criteria translator binds through:
// every UUID-shaped value must reach a BINARY(16) column as its 16-byte form
// (including a raw canonical-form string, the regression the review caught),
// while non-uuid values pass through untouched.
func TestEncodeArg(t *testing.T) {
	d := mysqlDialect{}
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")
	want := u[:]

	t.Run("domain.ID encodes to 16 bytes", func(t *testing.T) {
		got, ok := d.EncodeArg(domain.NewID(u.String())).([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("EncodeArg(domain.ID) = %v (ok=%v), want %x", got, ok, want)
		}
	})

	t.Run("uuid.UUID encodes to 16 bytes", func(t *testing.T) {
		got, ok := d.EncodeArg(u).([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("EncodeArg(uuid.UUID) = %v (ok=%v), want %x", got, ok, want)
		}
	})

	t.Run("canonical UUID string encodes to 16 bytes", func(t *testing.T) {
		got, ok := d.EncodeArg(u.String()).([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("EncodeArg(string) = %v (ok=%v), want %x", got, ok, want)
		}
	})

	t.Run("non-uuid string passes through", func(t *testing.T) {
		if got := d.EncodeArg("bob@example.com"); got != "bob@example.com" {
			t.Fatalf("EncodeArg(plain string) = %v, want it untouched", got)
		}
	})

	t.Run("36-char non-uuid string passes through", func(t *testing.T) {
		s := "not-a-uuid-but-exactly-36-chars-long" // len 36, fails uuid.Parse
		if len(s) != 36 {
			t.Fatalf("test fixture is %d chars, must be 36", len(s))
		}
		if got := d.EncodeArg(s); got != s {
			t.Fatalf("EncodeArg(36-char non-uuid) = %v, want it untouched", got)
		}
	})

	t.Run("int passes through", func(t *testing.T) {
		if got := d.EncodeArg(42); got != 42 {
			t.Fatalf("EncodeArg(int) = %v, want 42", got)
		}
	})
}
