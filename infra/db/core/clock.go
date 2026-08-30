package core

import (
	"context"
	"fmt"
	"time"
)

// ClockMode selects WHERE the authoritative instant of a write operation comes
// from — the single stamp every managed timestamp column of that operation
// binds (created_at, updated_at, and the archive/unarchive deleted_at stamp).
//
// It exists because the app clock is a per-POD clock. Several replicas of one
// service each carry their own drift, so two rows written seconds apart can be
// stamped out of order, and no amount of care in the write path fixes a wrong
// reading at the source. Pointing the stamp at the database gives every replica
// one clock — the one the rows already live on.
//
// What does NOT change with the mode: the stamp is still MINTED ONCE per
// operation and BOUND as an ordinary argument, never emitted as a NOW()
// expression inside the DML. That is what keeps every statement of one
// operation (root, children, siblings, base cascade) on the same instant, and
// keeps the value known in Go before COMMIT — so the outbox payload, the audit
// event, the lifecycle hooks and the HTTP response all carry it. Only the
// SOURCE of the reading moves.
type ClockMode int

const (
	// ClockApp reads the instant from the writing process: time.Now().UTC().
	// No round-trip, and correct as long as every replica's clock is
	// disciplined.
	ClockApp ClockMode = iota
	// ClockDB reads the instant from the relational backend, once per write
	// transaction, through Dialect.UTCNowExpr. Costs one extra round-trip per
	// write TX and makes the database the single clock for every replica.
	ClockDB
)

// String renders the mode under the same spelling the yaml declares
// (relational.clock), so a diagnostic and the configuration read alike.
func (m ClockMode) String() string {
	if m == ClockDB {
		return "db"
	}
	return "app"
}

// ParseClockMode maps the relational.clock yaml value onto a ClockMode. There is
// NO default: the framework refuses to choose a clock on the operator's behalf,
// exactly as it refuses to guess a dialect or a dsn — an absent value is an
// error the caller reports, not a silent fallback to the process clock.
func ParseClockMode(s string) (ClockMode, error) {
	switch s {
	case "app":
		return ClockApp, nil
	case "db":
		return ClockDB, nil
	case "":
		return 0, fmt.Errorf("relational.clock is not declared — declare \"db\" (the database is the single " +
			"clock for every replica; costs one round-trip per write transaction) or \"app\" (each replica " +
			"stamps from its own clock). There is no default")
	default:
		return 0, fmt.Errorf("relational.clock %q is not a clock source — declare \"db\" or \"app\"", s)
	}
}

// NowFrom mints the authoritative instant of one write operation under mode,
// reading through the OPEN write transaction when the mode is ClockDB so the
// stamp belongs to the same transaction the rows are written in.
//
// UTC, truncated to microseconds — the precision every supported backend stores
// (timestamptz / DATETIME(6) / DATETIME2(6) / TIMESTAMP(6)), so the value bound
// here and the value the composer reads back are identical. Truncating also
// normalizes the backends against each other: SQLite's UTCNowExpr resolves to
// milliseconds, Oracle's and SQL Server's to more digits than the columns hold.
func NowFrom(ctx context.Context, tx Tx, mode ClockMode) (time.Time, error) {
	if mode != ClockDB {
		return time.Now().UTC().Truncate(time.Microsecond), nil
	}
	var raw any
	if err := tx.QueryRow(ctx, "SELECT "+tx.Dialect().UTCNowExpr()).Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("db: reading the database clock (relational.clock: db): %w", err)
	}
	t := CoerceTime(raw)
	if t == nil {
		return time.Time{}, fmt.Errorf(
			"db: the database clock (relational.clock: db) returned %T, which is not a timestamp this "+
				"framework can decode", raw)
	}
	return t.UTC().Truncate(time.Microsecond), nil
}

// timeTextLayouts are the textual timestamp forms a driver may hand back when it
// does NOT decode to time.Time itself: SQLite stores timestamps as TEXT
// (RFC3339Nano for app-clock values, a "YYYY-MM-DD HH:MM:SS.mmm" strftime form
// for the dialect's now expression) and MySQL without parseTime returns
// "YYYY-MM-DD HH:MM:SS" as bytes. Ordered most- to least-specific; the first
// that parses wins.
var timeTextLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// CoerceTime turns a timestamp cell into a *time.Time, tolerating every driver
// representation one can arrive in: a native time.Time / *time.Time (the pgx,
// MySQL parseTime, go-mssqldb, go-ora path) and the textual string/[]byte forms
// SQLite (and MySQL without parseTime) return. Returns nil for an unrecognized
// or unparseable value — the caller reads nil as "absent", never a misleading
// zero time.
//
// It serves both ends of a managed timestamp: the clock reading that mints it
// (NowFrom) and the map-shaped read-back that recovers it.
func CoerceTime(v any) *time.Time {
	switch t := v.(type) {
	case time.Time:
		return &t
	case *time.Time:
		return t
	case string:
		return parseTimeText(t)
	case []byte:
		return parseTimeText(string(t))
	default:
		return nil
	}
}

func parseTimeText(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range timeTextLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}
