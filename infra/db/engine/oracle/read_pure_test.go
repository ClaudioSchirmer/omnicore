//go:build oracle

package oracle

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNormalizeOracleValue covers the read-path value codec QueryMaps drives
// (the dynamic, column-keyed read the composer uses). A RAW(16) uuid comes
// back as 16 raw bytes; without this rewrite the composer would join Mongo
// documents on a garbage string. The Oracle-only branch is NUMBER: go-ora
// yields EVERY numeric value as a string and its metadata cannot tell an
// integer column from a BOOLEAN or a COUNT (all report NUMBER), so a
// whole-number text is parsed to int64 — restoring the int64 the other
// engines yield natively (and letting the composer's schema-driven bool
// coercion restore a native-BOOLEAN "1"/"0") — while a decimal text keeps the
// canonical string form MySQL/SQL Server yield for DECIMAL.
func TestNormalizeOracleValue(t *testing.T) {
	u := uuid.MustParse("105663e1-dbb8-4ae9-a60b-0b2b66ac5c2a")

	t.Run("RAW(16) → canonical uuid string", func(t *testing.T) {
		got, err := normalizeOracleValue(u[:], "RAW")
		if err != nil {
			t.Fatalf("normalizeOracleValue: %v", err)
		}
		if got != u.String() {
			t.Errorf("got %v (%T), want canonical %v", got, got, u.String())
		}
	})

	t.Run("RAW but not 16 bytes → plain string (no misread)", func(t *testing.T) {
		got, err := normalizeOracleValue([]byte{0x01, 0x02, 0x03}, "RAW")
		if err != nil || got != string([]byte{0x01, 0x02, 0x03}) {
			t.Fatalf("got (%v,%v), want the raw bytes as string", got, err)
		}
	})

	t.Run("non-RAW []byte (native JSON / BLOB locator) → string", func(t *testing.T) {
		got, err := normalizeOracleValue([]byte(`{"a":1}`), "OCIBlobLocator")
		if err != nil || got != `{"a":1}` {
			t.Fatalf("got (%v,%v), want the text verbatim", got, err)
		}
	})

	t.Run("whole-number NUMBER text → int64 (integer/COUNT/BOOLEAN parity)", func(t *testing.T) {
		got, err := normalizeOracleValue("42", "NUMBER")
		if err != nil || got != int64(42) {
			t.Fatalf("got (%v %T,%v), want int64(42)", got, got, err)
		}
	})

	t.Run("decimal NUMBER text stays the canonical string (DECIMAL/AVG parity)", func(t *testing.T) {
		got, err := normalizeOracleValue("123.45", "NUMBER")
		if err != nil || got != "123.45" {
			t.Fatalf("got (%v,%v), want the text verbatim", got, err)
		}
	})

	t.Run("NUMBER beyond int64 range stays text (graceful)", func(t *testing.T) {
		huge := "99999999999999999999999999999999999999"
		got, err := normalizeOracleValue(huge, "NUMBER")
		if err != nil || got != huge {
			t.Fatalf("got (%v,%v), want the text verbatim", got, err)
		}
	})

	t.Run("non-NUMBER text passes through un-parsed", func(t *testing.T) {
		// A VARCHAR2 holding digits must NOT become an int64.
		got, err := normalizeOracleValue("42", "NCHAR")
		if err != nil || got != "42" {
			t.Fatalf("got (%v %T,%v), want the string untouched", got, got, err)
		}
	})

	t.Run("non-[]byte non-string scalars pass through untouched", func(t *testing.T) {
		now := time.Now()
		for _, c := range []any{int64(99), 3.14, true, now, nil} {
			got, err := normalizeOracleValue(c, "NUMBER")
			if err != nil || got != c {
				t.Errorf("scalar %v (%T) drifted to %v (%v)", c, c, got, err)
			}
		}
	})
}

// TestDecodeID covers the leading-key decoder the aggregate loader uses to turn
// a scanned ID/ParentID back into the canonical UUID string. A RAW(16) column scans
// into a 16-byte string (→ decode); anything else (a VARCHAR2 id, an
// already-decoded value) passes through defensively — the same real branching
// the other engines' DecodeID carries.
func TestDecodeID(t *testing.T) {
	d := oracleDialect{}
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
