package read

import (
	"database/sql"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// managedScan reads the framework-owned carrier columns a schema declares —
// the id (children only), created_at / updated_at / deleted_at and revision — as
// trailing SELECT columns and stamps them onto a freshly-loaded entity or
// aggregate child via SetID + domain.SetManagedColumns. Columns the schema does
// not declare are absent from both the SELECT (cols) and the apply — the
// carrier's slots stay nil/0.
//
// The order of cols and targets() is fixed: id, created, updated, deleted,
// revision (skipping the absent ones); the caller must append cols to the SELECT
// in that same order.
//
// The id is read here only for an aggregate CHILD (newChildManagedScan): the id
// now lives in the unexported domain.Managed carrier, so it is no longer a
// scan-plan struct field and — unlike the root, whose id is the SELECT's leading
// key — must be read as a trailing column. A root scan (newManagedScan) omits the
// id: findRoots recovers it from the leading key and calls SetID itself.
type managedScan struct {
	cols     []string
	idCol    string
	id       *sql.NullString
	created  *sql.NullTime
	updated  *sql.NullTime
	deleted  *sql.NullTime
	revision *sql.NullInt64
}

// newManagedScan builds the trailing-column scan for a ROOT: the id is the
// SELECT's leading key, so only the managed timestamps + revision are read here.
func newManagedScan(schema *core.TableSchema) *managedScan {
	return buildManagedScan(schema, false)
}

// newChildManagedScan builds the trailing-column scan for an aggregate child or
// base-child: the id is read here too (it is not the leading key — the ParentID
// is — and it left the scan plan when it moved into domain.Managed).
func newChildManagedScan(schema *core.TableSchema) *managedScan {
	return buildManagedScan(schema, true)
}

func buildManagedScan(schema *core.TableSchema, withID bool) *managedScan {
	m := &managedScan{}
	if withID {
		if c := schema.IDColumn(); c != "" {
			m.idCol = c
			m.id = &sql.NullString{}
			m.cols = append(m.cols, c)
		}
	}
	if c := schema.CreatedAtColumn(); c != "" {
		m.created = &sql.NullTime{}
		m.cols = append(m.cols, c)
	}
	if c := schema.UpdatedAtColumn(); c != "" {
		m.updated = &sql.NullTime{}
		m.cols = append(m.cols, c)
	}
	if c, ok := schema.DeletedAtColumn(); ok && c != "" {
		m.deleted = &sql.NullTime{}
		m.cols = append(m.cols, c)
	}
	if c := schema.RevisionColumn(); c != "" {
		m.revision = &sql.NullInt64{}
		m.cols = append(m.cols, c)
	}
	return m
}

// has reports whether any managed column is declared (nothing to select/apply).
func (m *managedScan) has() bool { return m != nil && len(m.cols) > 0 }

// targets returns the scan destinations in the same order as cols.
func (m *managedScan) targets() []any {
	out := make([]any, 0, len(m.cols))
	if m.id != nil {
		out = append(out, m.id)
	}
	if m.created != nil {
		out = append(out, m.created)
	}
	if m.updated != nil {
		out = append(out, m.updated)
	}
	if m.deleted != nil {
		out = append(out, m.deleted)
	}
	if m.revision != nil {
		out = append(out, m.revision)
	}
	return out
}

// apply stamps the just-scanned carrier values onto target (a POINTER to an
// entity or child embedding domain.Managed): the id via SetID (decoded through
// the dialect — BINARY(16) bytes and text forms all restore to the canonical
// uuid string), the timestamps + revision via domain.SetManagedColumns. A fresh
// *time.Time per call keeps each row's value independent.
func (m *managedScan) apply(target any, dialect Dialect) error {
	if m.id != nil && m.id.Valid {
		decoded, err := dialect.DecodeID(m.id.String)
		if err != nil {
			return err
		}
		if s, ok := target.(interface{ SetID(domain.ID) }); ok {
			s.SetID(domain.NewID(decoded))
		}
	}
	var created, updated, deleted *time.Time
	var revision int64
	if m.created != nil && m.created.Valid {
		t := m.created.Time
		created = &t
	}
	if m.updated != nil && m.updated.Valid {
		t := m.updated.Time
		updated = &t
	}
	if m.deleted != nil && m.deleted.Valid {
		t := m.deleted.Time
		deleted = &t
	}
	if m.revision != nil && m.revision.Valid {
		revision = m.revision.Int64
	}
	domain.SetManagedColumns(target, revision, created, updated, deleted)
	return nil
}

// managedTimeLayouts are the textual timestamp forms a driver may hand back
// through the column map when it does NOT decode to time.Time itself: SQLite
// stores timestamps as TEXT (RFC3339Nano for app-clock values, a
// "YYYY-MM-DD HH:MM:SS.mmm" strftime form for NowExpr) and MySQL without
// parseTime returns "YYYY-MM-DD HH:MM:SS" as bytes. Ordered most- to
// least-specific; the first that parses wins.
var managedTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// coerceManagedTime turns a managed-timestamp cell from the column map into a
// *time.Time, tolerating the driver representations that reach the MANUAL-scanner
// parity path: a native time.Time / *time.Time (the pgx, MySQL parseTime,
// go-mssqldb, go-ora path) and the textual string/[]byte forms SQLite (and MySQL
// without parseTime) return. This keeps the manual scanner at parity with the
// auto scan, which decodes these through the engine's sql.NullTime targets.
// Returns nil for an unrecognized or unparseable value (the caller reads nil as
// "absent"), never a misleading zero time.
func coerceManagedTime(v any) *time.Time {
	switch t := v.(type) {
	case time.Time:
		return &t
	case *time.Time:
		return t
	case string:
		return parseManagedTimeText(t)
	case []byte:
		return parseManagedTimeText(string(t))
	default:
		return nil
	}
}

func parseManagedTimeText(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range managedTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// toInt64 coerces the integer forms a driver hands a revision column through the
// column map (int64 on most, int32/int on some) to int64. A non-integer value
// yields 0 — a revision the caller reads as "absent".
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case uint64:
		return int64(n)
	default:
		return 0
	}
}
