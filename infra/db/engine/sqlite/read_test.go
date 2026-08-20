//go:build sqlite

package sqlite

import (
	"testing"
	"time"
)

func TestParseSQLiteTime_BothLayouts(t *testing.T) {
	want := time.Date(2026, 7, 31, 21, 10, 0, 123000000, time.UTC)

	cases := map[string]time.Time{
		"2026-07-31T21:10:00.123Z": want,                                           // RFC3339 (app-clock)
		"2026-07-31 21:10:00.123":  want,                                           // strftime ms (NowExpr)
		"2026-07-31 21:10:00":      time.Date(2026, 7, 31, 21, 10, 0, 0, time.UTC), // strftime no ms
		"2026-07-31":               time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),   // date only
	}
	for in, exp := range cases {
		got, err := parseSQLiteTime(in)
		if err != nil {
			t.Errorf("parseSQLiteTime(%q) error: %v", in, err)
			continue
		}
		if !got.Equal(exp) {
			t.Errorf("parseSQLiteTime(%q) = %v, want %v", in, got, exp)
		}
	}
}

func TestParseSQLiteTime_Invalid(t *testing.T) {
	if _, err := parseSQLiteTime("not a timestamp"); err == nil {
		t.Error("expected an error for an unparseable timestamp")
	}
}
