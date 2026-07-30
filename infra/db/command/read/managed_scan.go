package read

import (
	"database/sql"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// managedScan reads the framework-managed columns a schema declares
// (created_at / updated_at / deleted_at / revision) as trailing SELECT columns
// and stamps them onto a freshly-loaded entity or aggregate child via
// domain.SetManagedColumns. Columns the schema does not declare are absent from
// both the SELECT (cols) and the apply — the carrier's slots stay nil/0.
//
// The order of cols and targets() is fixed: created, updated, deleted, revision
// (skipping the absent ones); the caller must append cols to the SELECT in that
// same order.
type managedScan struct {
	cols     []string
	created  *sql.NullTime
	updated  *sql.NullTime
	deleted  *sql.NullTime
	revision *sql.NullInt64
}

func newManagedScan(schema *core.TableSchema) *managedScan {
	m := &managedScan{}
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

// apply stamps the just-scanned managed values onto target (a POINTER to an
// entity or child embedding domain.Managed). A fresh *time.Time per call keeps
// each row's value independent.
func (m *managedScan) apply(target any) {
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
}
