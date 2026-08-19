package domain

import "time"

// Managed is the embedded carrier for the columns the FRAMEWORK owns on an
// entity or an aggregate child: the id, the revision watermark, and the managed
// timestamps. Embed it (root via BaseEntity, children directly) to surface them.
//
// Access model: the id is settable (SetID / ClearID) — identity flows in from
// the persister and, on some surfaces, from the caller. The revision and the
// three timestamps are READ-ONLY: the write path stamps created_at/updated_at
// with the app clock, the framework bumps revision, and deleted_at moves only
// through Archive/Unarchive — the dev never sets them, they surface after a load
// via the getters. The relational loader populates all of it through
// SetManagedColumns.
//
// The fields are unexported: they are framework-managed and never part of
// business identity (IsSameByBusinessFields skips this carrier) or the audit
// delta. The root Old() ghost is a JSON snapshot, so the unexported carrier does
// not survive into it; an aggregate child's Old() copy is a whole-value copy and
// DOES retain the carrier — harmless, since it stays invisible to identity and
// audit either way.
type Managed struct {
	id        *ID
	revision  int64
	createdAt *time.Time
	updatedAt *time.Time
	deletedAt *time.Time
}

// GetID returns the id as a value — an empty ID (IsEmpty) when the row is not
// persisted yet. This is the AggregateValueObject.GetID() ID contract, promoted
// to every embedding child. BaseEntity shadows it with GetID() *ID for the
// Entity contract (which needs the nullable form).
func (m Managed) GetID() ID {
	if m.id == nil {
		return ID{}
	}
	return *m.id
}

// SetID stamps the id. Used by the persister's minted-id write-back and by
// caller surfaces that address a child by id.
func (m *Managed) SetID(id ID) { m.id = &id }

// ClearID drops the id back to "not persisted".
func (m *Managed) ClearID() { m.id = nil }

// idPtr exposes the raw nullable pointer for BaseEntity's GetID() *ID shadow
// (same package only).
func (m *Managed) idPtr() *ID { return m.id }

// GetRevision returns the row's own revision — the commit-order token the
// relational load stamps from the schema's revision column, incremented by the
// framework in the same statement as every UPDATE.
//
// It is also the OPTIMISTIC-CONCURRENCY token: the write path guards its UPDATE
// on the value the entity was loaded with, so a write built on a stale read is
// refused instead of reverting whatever landed in between. A persisted row is
// always >= 1 (an INSERT initializes it to 1), so 0 means exactly one thing —
// this entity never came from the loader — and the guard degrades to an
// unguarded write for it.
func (m Managed) GetRevision() int64 { return m.revision }

// GetCreatedAt / GetUpdatedAt / GetDeletedAt return the managed timestamps, each
// nil when absent: nil created/updated means the row is not loaded/persisted yet
// (never a misleading zero time), nil deleted means a live row.
func (m Managed) GetCreatedAt() *time.Time { return m.createdAt }
func (m Managed) GetUpdatedAt() *time.Time { return m.updatedAt }
func (m Managed) GetDeletedAt() *time.Time { return m.deletedAt }

// setManagedColumns is the framework-only populate hook, reached from
// SetManagedColumns. Unexported: there is no public setter for managed data.
func (m *Managed) setManagedColumns(revision int64, createdAt, updatedAt, deletedAt *time.Time) {
	m.revision = revision
	m.createdAt = createdAt
	m.updatedAt = updatedAt
	m.deletedAt = deletedAt
}

// WithID returns a copy of an aggregate child (or any value embedding Managed)
// with its id set — the one-expression form of "construct then SetID", handy in
// slice/table literals: domain.WithID(Address{Street: "x"}, domain.NewID("a1")).
// A no-op (returns item unchanged) on a value with no settable id.
func WithID[T any](item T, id ID) T {
	if s, ok := any(&item).(interface{ SetID(ID) }); ok {
		s.SetID(id)
	}
	return item
}

type managedColumnWriter interface {
	setManagedColumns(revision int64, createdAt, updatedAt, deletedAt *time.Time)
}

// SetManagedColumns populates the framework-managed revision + timestamps on an
// entity or aggregate child that embeds Managed. The relational loader calls it
// after a scan (the id is set separately via SetID). It is a no-op returning
// false on a target that does not embed Managed. Pass a POINTER — the carrier is
// mutated in place.
func SetManagedColumns(target any, revision int64, createdAt, updatedAt, deletedAt *time.Time) bool {
	w, ok := target.(managedColumnWriter)
	if !ok {
		return false
	}
	w.setManagedColumns(revision, createdAt, updatedAt, deletedAt)
	return true
}
