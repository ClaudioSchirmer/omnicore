package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// clockTx is the narrowest Tx a clock reading needs: a scriptable QueryRow plus
// a dialect to take the expression from.
type clockTx struct {
	val     any
	err     error
	lastSQL string
}

func (t *clockTx) Exec(context.Context, string, ...any) error          { return nil }
func (t *clockTx) Query(context.Context, string, ...any) (Rows, error) { return nil, nil }
func (t *clockTx) QueryRow(_ context.Context, sql string, _ ...any) Row {
	t.lastSQL = sql
	return clockRow{val: t.val, err: t.err}
}
func (t *clockTx) Dialect() Dialect { return clockDialect{} }

type clockRow struct {
	val any
	err error
}

func (r clockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 1 {
		if p, ok := dest[0].(*any); ok {
			*p = r.val
		}
	}
	return nil
}

// clockDialect supplies only what NowFrom reaches for; every other method is a
// stub, since the clock reading touches none of them.
type clockDialect struct{ testPGDialect }

func (clockDialect) UTCNowExpr() string { return "UTC_NOW_FOR_TEST()" }

// ParseClockMode refuses to default: an absent value is the operator's missing
// declaration, not a silent fall back to the process clock.
func TestParseClockMode(t *testing.T) {
	if m, err := ParseClockMode("app"); err != nil || m != ClockApp {
		t.Fatalf(`ParseClockMode("app") = %v, %v`, m, err)
	}
	if m, err := ParseClockMode("db"); err != nil || m != ClockDB {
		t.Fatalf(`ParseClockMode("db") = %v, %v`, m, err)
	}
	err := errFrom(ParseClockMode(""))
	if err == nil || !strings.Contains(err.Error(), "no default") {
		t.Fatalf("an absent clock must be refused as having no default, got %v", err)
	}
	err = errFrom(ParseClockMode("database"))
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("an unknown clock must name the offending token, got %v", err)
	}
}

func errFrom(_ ClockMode, err error) error { return err }

func TestClockMode_String(t *testing.T) {
	if got := ClockApp.String(); got != "app" {
		t.Errorf("ClockApp = %q", got)
	}
	if got := ClockDB.String(); got != "db" {
		t.Errorf("ClockDB = %q", got)
	}
}

// The app clock must not touch the transaction — that absent round-trip IS the
// difference between the two modes.
func TestNowFrom_AppClockIssuesNoQuery(t *testing.T) {
	tx := &clockTx{}
	got, err := NowFrom(context.Background(), tx, ClockApp)
	if err != nil {
		t.Fatalf("NowFrom: %v", err)
	}
	if tx.lastSQL != "" {
		t.Errorf("the app clock queried the database: %q", tx.lastSQL)
	}
	if got.Location() != time.UTC || got.Truncate(time.Microsecond) != got {
		t.Errorf("NowFrom = %v, want UTC and microsecond-truncated", got)
	}
}

func TestNowFrom_DBClockReadsTheDialectExpression(t *testing.T) {
	want := time.Date(2026, 3, 4, 5, 6, 7, 891011000, time.UTC)
	tx := &clockTx{val: want}
	got, err := NowFrom(context.Background(), tx, ClockDB)
	if err != nil {
		t.Fatalf("NowFrom: %v", err)
	}
	if tx.lastSQL != "SELECT UTC_NOW_FOR_TEST()" {
		t.Errorf("clock statement = %q", tx.lastSQL)
	}
	if !got.Equal(want) {
		t.Errorf("clock = %v, want %v", got, want)
	}
}

// Every backend is truncated to the precision the columns hold, so a reading
// finer than microseconds (SQL Server, Oracle) cannot make the value bound here
// differ from the value read back.
func TestNowFrom_DBClockTruncatesToMicrosecond(t *testing.T) {
	tx := &clockTx{val: time.Date(2026, 3, 4, 5, 6, 7, 891011999, time.UTC)}
	got, err := NowFrom(context.Background(), tx, ClockDB)
	if err != nil {
		t.Fatalf("NowFrom: %v", err)
	}
	if want := time.Date(2026, 3, 4, 5, 6, 7, 891011000, time.UTC); !got.Equal(want) {
		t.Errorf("clock = %v, want %v", got, want)
	}
}

// A textual reading (SQLite's strftime, MySQL without parseTime) decodes like a
// native one — the clock rides the same coercion every driver form needs.
func TestNowFrom_DBClockTextualValue(t *testing.T) {
	tx := &clockTx{val: []byte("2026-03-04 05:06:07.891")}
	got, err := NowFrom(context.Background(), tx, ClockDB)
	if err != nil {
		t.Fatalf("NowFrom: %v", err)
	}
	if want := time.Date(2026, 3, 4, 5, 6, 7, 891000000, time.UTC); !got.Equal(want) {
		t.Errorf("clock = %v, want %v", got, want)
	}
}

// The clock is load-bearing: a failed or undecodable reading aborts the write
// rather than falling back to the process clock the operator declined.
func TestNowFrom_DBClockErrors(t *testing.T) {
	boom := errors.New("conn reset")
	if _, err := NowFrom(context.Background(), &clockTx{err: boom}, ClockDB); !errors.Is(err, boom) {
		t.Fatalf("a driver error must propagate, got %v", err)
	}
	_, err := NowFrom(context.Background(), &clockTx{val: 42}, ClockDB)
	if err == nil || !strings.Contains(err.Error(), "not a timestamp") {
		t.Fatalf("an undecodable reading must be an error, not a zero time, got %v", err)
	}
}

// CoerceTime is the one decode both ends of a managed timestamp use: the clock
// that mints it and any read-back that recovers it from an untyped cell.
func TestCoerceTime(t *testing.T) {
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
		{"strftime-ms string (sqlite now expression)", "2026-08-01 10:00:00.000", true},
		{"space form bytes (mysql no parseTime)", []byte("2026-08-01 10:00:00"), true},
		{"date only", "2026-08-01", true},
		{"empty string", "", false},
		{"garbage string", "not-a-time", false},
		{"unsupported type", 12345, false},
		{"nil pointer time.Time", (*time.Time)(nil), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CoerceTime(c.in)
			if c.wantOK && got == nil {
				t.Fatalf("CoerceTime(%v) = nil, want a parsed time", c.in)
			}
			if !c.wantOK && got != nil {
				t.Fatalf("CoerceTime(%v) = %v, want nil", c.in, got)
			}
			// The date-only case parses to midnight, which is not `want`; every
			// other accepted form is the same instant.
			if c.wantOK && c.name != "date only" && !got.Equal(want) {
				t.Errorf("CoerceTime(%v) = %v, want %v", c.in, got, want)
			}
		})
	}
}
