//go:build integration && oracle

package oracle

import (
	"testing"
	"time"
)

// A row written from a time.Time must be selectable by a predicate carrying
// that SAME time.Time. go-ora renders a comparison bind in the process's local
// zone while an INSERT binds the value's own location, so without
// EncodeArg's compensation the two disagree by the host's offset and an
// equality on a date column matches nothing — the shape a `*time.Time` filter
// leaf takes on every read.
//
//	go test -tags=integration,oracle ./infra/db/engine/oracle/ -run TimeBind -count=1
func TestOracleEncodeArg_TimeBindAgreesWithTheStoredRow(t *testing.T) {
	_, raw := setup(t)

	if _, err := raw.Exec(`BEGIN EXECUTE IMMEDIATE 'DROP TABLE tz_bind_probe'; EXCEPTION WHEN OTHERS THEN NULL; END;`); err != nil {
		t.Log(err)
	}
	if _, err := raw.Exec(`CREATE TABLE tz_bind_probe (ts TIMESTAMP(6))`); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The instant a consumer's ?trialTo=2026-04-06T00:00:00Z produces.
	want := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	if _, err := raw.Exec(`INSERT INTO tz_bind_probe (ts) VALUES (:1)`, want); err != nil {
		t.Fatalf("insert: %v", err)
	}

	d := oracleDialect{}

	var got int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM tz_bind_probe WHERE ts = :1`,
		d.EncodeArg(want)).Scan(&got); err != nil {
		t.Fatalf("equality query: %v", err)
	}
	if got != 1 {
		t.Errorf("equality on the encoded bind matched %d rows, want 1 — "+
			"the predicate disagrees with what the INSERT stored", got)
	}

	// The ordinal operators the same leaf declares have to agree too: a
	// half-corrected bind would leave gte/lt off by the host's offset.
	for _, c := range []struct {
		name string
		sql  string
		arg  time.Time
		want int
	}{
		{"gte on the same instant", `ts >= :1`, want, 1},
		{"lte on the same instant", `ts <= :1`, want, 1},
		{"gt one second earlier", `ts > :1`, want.Add(-time.Second), 1},
		{"lt one second earlier", `ts < :1`, want.Add(-time.Second), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var n int
			if err := raw.QueryRow(`SELECT COUNT(*) FROM tz_bind_probe WHERE `+c.sql,
				d.EncodeArg(c.arg)).Scan(&n); err != nil {
				t.Fatalf("query: %v", err)
			}
			if n != c.want {
				t.Errorf("matched %d, want %d", n, c.want)
			}
		})
	}

	// A nil *time.Time stays a typed nil so it reaches the driver as NULL
	// rather than panicking on the deref.
	if v := d.EncodeArg((*time.Time)(nil)); v == nil {
		t.Error("a nil *time.Time must stay a TYPED nil for the driver")
	}
}
