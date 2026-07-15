//go:build sqlserver

package sqlserver

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

// TestUUIDBytes proves the UUID→BINARY(16) encoder the value codec
// (sqlserverDialect.EncodeArg) binds through: a canonical UUID string becomes
// its exact 16-byte form, and a non-UUID id surfaces a wiring error instead of
// silently writing garbage into a BINARY(16) column. Storing the canonical
// bytes verbatim is the load-bearing decision here: BINARY(16) compares
// bytewise, so the UUIDv7 time order survives in the clustered PK —
// UNIQUEIDENTIFIER would reorder the comparison and fragment it.
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
			t.Errorf("BINARY(16) form must be 16 bytes, got %d", len(b))
		}
	})

	t.Run("non-UUID id errors (wiring bug, not silent)", func(t *testing.T) {
		if _, err := uuidBytes("not-a-uuid"); err == nil {
			t.Fatal("expected an error for a non-UUID id")
		}
	})
}

// TestQuoteIdent locks SQL Server identifier quoting: a safe identifier is
// wrapped in brackets (the T-SQL flavor, where MySQL uses backticks and
// Postgres bare/validated), and an unsafe identifier panics loudly — the same
// SQL-injection defense as the other engines, since identifiers come from
// schema declarations, never user input.
func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("user_id"); got != "[user_id]" {
		t.Errorf("quoteIdent(user_id) = %q, want [user_id]", got)
	}

	t.Run("invalid identifier panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("quoteIdent did not panic on an unsafe identifier")
			}
		}()
		_ = quoteIdent("name]; DROP TABLE users;--")
	})
}
