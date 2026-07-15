//go:build sqlserver

package sqlserver

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNormalizeSQLServerValue covers the read-path value codec QueryMaps drives
// (the dynamic, column-keyed read the composer uses). A BINARY(16) uuid comes
// back as 16 raw bytes; without this rewrite the composer would join Mongo
// documents on a garbage string. The MySQL twin is normalizeMySQLValue; the
// same dbType guard prevents a coincidental 16-byte non-BINARY value from
// being misread.
func TestNormalizeSQLServerValue(t *testing.T) {
	u := uuid.MustParse("105663e1-dbb8-4ae9-a60b-0b2b66ac5c2a")

	t.Run("BINARY(16) → canonical uuid string", func(t *testing.T) {
		got, err := normalizeSQLServerValue(u[:], "BINARY")
		if err != nil {
			t.Fatalf("normalizeSQLServerValue: %v", err)
		}
		if got != u.String() {
			t.Errorf("got %v (%T), want canonical %v", got, got, u.String())
		}
	})

	t.Run("BINARY but not 16 bytes → plain string (no misread)", func(t *testing.T) {
		got, err := normalizeSQLServerValue([]byte{0x01, 0x02, 0x03}, "BINARY")
		if err != nil || got != string([]byte{0x01, 0x02, 0x03}) {
			t.Fatalf("got (%v,%v), want the raw bytes as string", got, err)
		}
	})

	t.Run("non-BINARY []byte (e.g. DECIMAL) → string", func(t *testing.T) {
		// go-mssqldb hands DECIMAL back raw; the canonical text form matches
		// what the MySQL engine yields, so the read specs normalize identically.
		got, err := normalizeSQLServerValue([]byte("123.45"), "DECIMAL")
		if err != nil || got != "123.45" {
			t.Fatalf("got (%v,%v), want the text verbatim", got, err)
		}
	})

	t.Run("non-[]byte scalars pass through untouched", func(t *testing.T) {
		now := time.Now()
		for _, c := range []any{"already a string", 42, int64(99), 3.14, true, now, nil} {
			got, err := normalizeSQLServerValue(c, "INT")
			if err != nil || got != c {
				t.Errorf("scalar %v (%T) drifted to %v (%v)", c, c, got, err)
			}
		}
	})
}

// TestDecodeID covers the leading-key decoder the aggregate loader uses to turn
// a scanned PK/FK back into the canonical UUID string. A BINARY(16) column
// scans into a 16-byte string (→ decode); anything else (an NCHAR id, an
// already-decoded value) passes through defensively — the same real branching
// the MySQL DecodeID carries.
func TestDecodeID(t *testing.T) {
	d := sqlserverDialect{}
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")

	t.Run("16-byte raw → canonical uuid string", func(t *testing.T) {
		got, err := d.DecodeID(string(u[:]))
		if err != nil {
			t.Fatalf("DecodeID: %v", err)
		}
		if got != u.String() {
			t.Errorf("DecodeID(raw 16B) = %q, want %q", got, u.String())
		}
	})

	t.Run("already-canonical 36-char id passes through", func(t *testing.T) {
		got, err := d.DecodeID(u.String())
		if err != nil || got != u.String() {
			t.Fatalf("DecodeID(canonical) = (%q,%v), want it unchanged", got, err)
		}
	})

	t.Run("empty id passes through", func(t *testing.T) {
		if got, err := d.DecodeID(""); err != nil || got != "" {
			t.Fatalf("DecodeID(\"\") = (%q,%v), want (\"\",nil)", got, err)
		}
	})
}
