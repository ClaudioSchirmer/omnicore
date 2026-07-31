package read

import (
	"database/sql"
	"reflect"
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

// applyManagedFromMap stamps the framework-managed timestamps + revision onto
// target (a POINTER embedding domain.Managed) from a column-keyed row map — the
// manual-scanner parity path. A manual RootScanner/ChildScanner controls the
// business-field decode and (for the root) the SetID, but the map QueryMaps
// returns already carries the managed columns under their declared names
// (schema.ReadColumns includes them), so the framework fills the carrier the same
// way the auto scan does. Timestamp cells arrive as time.Time (nil when NULL),
// the revision as an integer; the id, when a manual scanner did not set it, is
// left to the scanner's own SetID (the root contract).
func applyManagedFromMap(target any, schema *core.TableSchema, m map[string]any) {
	toTimePtr := func(col string) *time.Time {
		if col == "" {
			return nil
		}
		if v, ok := m[col]; ok && v != nil {
			if t, ok := v.(time.Time); ok {
				return &t
			}
		}
		return nil
	}
	created := toTimePtr(schema.CreatedAtColumn())
	updated := toTimePtr(schema.UpdatedAtColumn())
	deletedCol, _ := schema.DeletedAtColumn()
	deleted := toTimePtr(deletedCol)
	var revision int64
	if rc := schema.RevisionColumn(); rc != "" {
		if v, ok := m[rc]; ok && v != nil {
			revision = toInt64(v)
		}
	}
	domain.SetManagedColumns(target, revision, created, updated, deleted)
}

// withManagedFromMap returns avo with its managed carrier filled from a row map —
// the manual ChildScanner parity path. A manual child scanner owns the business
// fields and the id (it calls SetID), the framework fills the managed columns.
// AVOs are value types, so the carrier is filled on an addressable copy; a
// pointer AVO is mutated in place.
func withManagedFromMap(avo domain.AggregateValueObject, schema *core.TableSchema, m map[string]any) domain.AggregateValueObject {
	rv := reflect.ValueOf(avo)
	if rv.Kind() == reflect.Pointer {
		applyManagedFromMap(avo, schema, m)
		return avo
	}
	p := reflect.New(rv.Type())
	p.Elem().Set(rv)
	applyManagedFromMap(p.Interface(), schema, m)
	return p.Elem().Interface().(domain.AggregateValueObject)
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
