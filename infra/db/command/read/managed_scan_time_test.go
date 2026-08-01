package read

import (
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
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

// Fix #8: the manual-scanner parity path must fill the managed timestamps even
// when the driver hands them back as TEXT (SQLite) or bytes (MySQL without
// parseTime), matching the auto scan — not silently leave them nil.
func TestApplyManagedFromMap_TextTimestamps(t *testing.T) {
	schema := core.NewTableSchema[*mgScanTarget]("t").
		ID("id").Revision("revision").
		CreatedAt("created_at").UpdatedAt("updated_at").DeletedAt("deleted_at")

	m := map[string]any{
		"revision":   int64(7),
		"created_at": "2026-08-01T10:00:00Z",            // SQLite RFC3339 TEXT
		"updated_at": []byte("2026-08-01 10:05:00.123"), // strftime-ms bytes
		// deleted_at absent → live row, must stay nil.
	}

	tgt := &mgScanTarget{}
	applyManagedFromMap(tgt, schema, m)

	if tgt.GetRevision() != 7 {
		t.Errorf("revision = %d, want 7", tgt.GetRevision())
	}
	if tgt.GetCreatedAt() == nil {
		t.Error("created_at not parsed from RFC3339 TEXT (manual-scanner parity broken)")
	}
	if tgt.GetUpdatedAt() == nil {
		t.Error("updated_at not parsed from strftime-ms bytes")
	}
	if tgt.GetDeletedAt() != nil {
		t.Errorf("deleted_at absent must be nil, got %v", tgt.GetDeletedAt())
	}
}
