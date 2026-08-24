package read

import (
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestCoerceManagedTime(t *testing.T) {
	want := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		in     any
		wantOK bool
	}{
		{"native time.Time", want, true},
		{"pointer time.Time", &want, true},
		{"rfc3339 string (sqlite app-clock)", "2026-08-01T10:00:00Z", true},
		{"rfc3339nano string", "2026-08-01T10:00:00.000000000Z", true},
		{"strftime-ms string (sqlite NowExpr)", "2026-08-01 10:00:00.000", true},
		{"space form bytes (mysql no parseTime)", []byte("2026-08-01 10:00:00"), true},
		{"date only", "2026-08-01", true},
		{"garbage string", "not-a-time", false},
		{"unsupported type", 12345, false},
		{"nil pointer time.Time", (*time.Time)(nil), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := coerceManagedTime(c.in)
			if c.wantOK && got == nil {
				t.Fatalf("coerceManagedTime(%v) = nil, want a parsed time", c.in)
			}
			if !c.wantOK && got != nil {
				t.Fatalf("coerceManagedTime(%v) = %v, want nil", c.in, got)
			}
			if c.wantOK && !got.Equal(want) {
				// date-only differs from `want` on purpose; only assert equality
				// for the full-timestamp cases.
				if c.name != "date only" {
					t.Errorf("coerceManagedTime(%v) = %v, want %v", c.in, got, want)
				}
			}
		})
	}
}

type mgScanTarget struct{ domain.Managed }
