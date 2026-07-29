//go:build mysql

package mysql

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNormalizeMySQLValue covers the read-path value codec QueryMaps drives (the
// dynamic, column-keyed read the composer uses). database/sql hands a BINARY(16)
// uuid back as 16 raw bytes; without this rewrite the composer would join Mongo
// documents on a garbage string. The PG mirror is normalizeSQLValue (tested in
// the postgres package); this is the MySQL twin.
func TestNormalizeMySQLValue(t *testing.T) {
	u := uuid.MustParse("105663e1-dbb8-4ae9-a60b-0b2b66ac5c2a")

	t.Run("BINARY(16) → canonical uuid string", func(t *testing.T) {
		got, err := normalizeMySQLValue(u[:], "BINARY")
		if err != nil {
			t.Fatalf("normalizeMySQLValue: %v", err)
		}
		if got != u.String() {
			t.Errorf("got %v (%T), want canonical %v", got, got, u.String())
		}
	})

	t.Run("BINARY but not 16 bytes → plain string (no misread)", func(t *testing.T) {
		// A short BINARY value is NOT a uuid — it must stringify, never panic or
		// reinterpret as a uuid.
		got, err := normalizeMySQLValue([]byte{0x01, 0x02, 0x03}, "BINARY")
		if err != nil || got != string([]byte{0x01, 0x02, 0x03}) {
			t.Fatalf("got (%v,%v), want the raw bytes as string", got, err)
		}
	})

	t.Run("non-BINARY []byte (text/decimal) → string", func(t *testing.T) {
		// A 16-char VARCHAR coincidentally the same length as a uuid must NOT be
		// decoded as BINARY(16) — the dbType guard is what prevents the misread.
		got, err := normalizeMySQLValue([]byte("sixteen-char-txt"), "VARCHAR")
		if err != nil || got != "sixteen-char-txt" {
			t.Fatalf("got (%v,%v), want the text verbatim", got, err)
		}
	})

	t.Run("non-[]byte scalars pass through untouched", func(t *testing.T) {
		now := time.Now()
		for _, c := range []any{"already a string", 42, int64(99), 3.14, true, now, nil} {
			got, err := normalizeMySQLValue(c, "INT")
			if err != nil || got != c {
				t.Errorf("scalar %v (%T) drifted to %v (%v)", c, c, got, err)
			}
		}
	})
}

// TestDecodeID covers the leading-key decoder the aggregate loader uses to turn a
// scanned ID/ParentID back into the canonical UUID string. A BINARY(16) column scans
// into a 16-byte string (→ decode); anything else (a CHAR id, an already-decoded
// value) passes through defensively. The PG DecodeID is a trivial passthrough;
// the MySQL one carries real branching, so it earns its own test.
func TestDecodeID(t *testing.T) {
	d := mysqlDialect{}
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
