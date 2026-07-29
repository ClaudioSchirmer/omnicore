//go:build oracle

package oracle

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

// TestUUIDBytes proves the UUID→RAW(16) encoder the value codec
// (oracleDialect.EncodeArg) binds through: a canonical UUID string becomes its
// exact 16-byte form, and a non-UUID id surfaces a wiring error instead of
// silently writing garbage into a RAW(16) column. Storing the canonical bytes
// verbatim is the load-bearing decision here: RAW compares bytewise, so the
// UUIDv7 time order survives in the ID index (the BINARY(16) rationale,
// verified against a live Oracle Free 23ai).
func TestUUIDBytes(t *testing.T) {
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")

	t.Run("canonical UUID string → its 16 bytes", func(t *testing.T) {
		b, err := uuidBytes(u.String())
		if err != nil {
			t.Fatalf("uuidBytes: %v", err)
		}
		if !bytes.Equal(b, u[:]) {
			t.Errorf("uuidBytes = %x, want %x", b, u[:])
		}
		if len(b) != 16 {
			t.Errorf("RAW(16) form must be 16 bytes, got %d", len(b))
		}
	})

	t.Run("non-UUID id errors (wiring bug, not silent)", func(t *testing.T) {
		if _, err := uuidBytes("not-a-uuid"); err == nil {
			t.Fatal("expected an error for a non-UUID id")
		}
	})
}

// TestQuoteIdent locks Oracle identifier rendering: QUOTED-UPPERCASE — the
// form equivalent by construction to the platform's unquoted resolution (an
// unquoted identifier folds to uppercase in the catalog, which is exactly the
// name the quoted form addresses), so manual unquoted queries keep matching
// while reserved-word collisions (a `number` column) work with NO
// reserved-word list — one total rule, the MySQL-backtick/T-SQL-bracket
// philosophy. An unsafe identifier panics loudly, the same SQL-injection
// defense as the other engines, since identifiers come from schema
// declarations, never user input.
func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("user_id"); got != `"USER_ID"` {
		t.Errorf("quoteIdent(user_id) = %q, want the quoted-uppercase form", got)
	}
	if got := quoteIdent("number"); got != `"NUMBER"` {
		t.Errorf("quoteIdent(number) = %q, want the reserved word usable as %q", got, `"NUMBER"`)
	}

	t.Run("invalid identifier panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("quoteIdent did not panic on an unsafe identifier")
			}
		}()
		_ = quoteIdent("name; DROP TABLE users;--")
	})
}
