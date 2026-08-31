//go:build mysql

package mysql

import (
	"testing"
	"time"
)

// The ZERO instant is what StampEmpty writes into a stamped TIME column, and it
// is the one value go-sql-driver refuses to pass through honestly: it serializes
// a zero time.Time as '0000-00-00', which a server running NO_ZERO_DATE +
// STRICT_TRANS_TABLES (the MySQL 8 default) rejects with Error 1292. Binding it
// as text is what keeps year 1 reaching the column, so this pins the detour.
func TestEncodeArg_ZeroInstantBindsAsText(t *testing.T) {
	got := mysqlDialect{}.EncodeArg(time.Time{})
	want := "0001-01-01 00:00:00.000000"
	if got != want {
		t.Fatalf("a zero instant must bind as its formatted text, got %#v want %q", got, want)
	}
}

// Every other instant keeps the path it always had — the driver formats it, and
// nothing here second-guesses the timezone or the precision.
func TestEncodeArg_OrdinaryInstantIsUntouched(t *testing.T) {
	at := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	got := mysqlDialect{}.EncodeArg(at)
	if back, ok := got.(time.Time); !ok || !back.Equal(at) {
		t.Fatalf("a non-zero instant must pass through, got %#v", got)
	}
}

// A nil *time.Time is SQL NULL; a non-nil one follows the same zero rule as the
// value form, so StampEmpty behaves the same whichever shape reaches the codec.
func TestEncodeArg_PointerInstant(t *testing.T) {
	nilGot := mysqlDialect{}.EncodeArg((*time.Time)(nil))
	if nilGot != nil {
		t.Fatalf("a nil *time.Time is NULL, got %#v", nilGot)
	}
	zero := time.Time{}
	zeroGot := mysqlDialect{}.EncodeArg(&zero)
	if zeroGot != "0001-01-01 00:00:00.000000" {
		t.Fatalf("a zero *time.Time must bind as text too, got %#v", zeroGot)
	}
}
