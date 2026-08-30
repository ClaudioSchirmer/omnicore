package domain

import "time"

// Managed is the embedded carrier for the columns the FRAMEWORK owns on an
// entity or an aggregate child: the id, the revision watermark, and the managed
// timestamps. Embed it (root via BaseEntity, children directly) to surface them.
//
// Access model: the id is settable (SetID / ClearID) — identity flows in from
// the persister and, on some surfaces, from the caller. The revision and the
// three timestamps are READ-ONLY: the write path stamps created_at/updated_at
// from the operation's own instant (the source declared in relational.clock),
// the framework bumps revision, and deleted_at moves only through
// Archive/Unarchive — the dev never sets them, they surface after a load via the
// getters. The relational loader populates all of it through SetManagedColumns.
//
// Stamp is the one thing on this carrier the DOMAIN drives, and it still sets no
// value: it asks the framework to fill a stamped field on this write, leaving
// the WHEN to a business rule and the value to the same operation instant.
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

	// stamps are the STAMPED fields this write asks the framework to fill —
	// requested by Go field name, never by column (the physical name lives only
	// in the TableSchema). See Stamp.
	stamps []string
}

// Stamp asks the framework to fill a stamped field on THIS write. The domain
// owns the WHEN — a business rule decides that a contract was just signed, an
// order just paid — and the framework owns the VALUE, which is the write
// operation's own authoritative instant (the same one created_at/updated_at
// carry, read from the clock the service declared in relational.clock).
//
// HOW TO USE IT — two halves, in two layers. The infra schema declares WHICH
// columns are stamped; the domain decides WHEN each one happens:
//
//	// infra/persistence — the WHAT
//	core.NewTableSchema[*Order]("orders").
//	    ID("id").
//	    Field("Status", "status").
//	    StampedTimeField("PaidAt", "paid_at").      // *time.Time, nullable column
//	    StampedTimeField("CanceledAt", "canceled_at")
//
//	// domain — the WHEN
//	type Order struct {
//	    domain.BaseEntity                            // brings Stamp along
//	    Status string
//	    PaidAt *time.Time                            // never assigned by hand
//	}
//
//	func (o *Order) Pay() {
//	    o.Status = "PAID"
//	    o.Stamp("PaidAt")                            // ask; do not assign
//	}
//
//	// application — an ordinary write; nothing extra to remember
//	order.Pay()
//	res, err := repo.Update(ctx, order)
//	// order.PaidAt is now filled with the instant the row carries.
//
// The field is addressed by GO FIELD NAME, exactly as a criteria addresses one
// (criteria.Eq("Status", …)) — the physical column never leaves the schema.
// After the write, the value is on the entity too, so the response, the audit
// event and the outbox payload all agree with the row.
//
// A rule inside BuildRules may stamp as freely as a method does: the request is
// read at write time, so wherever the decision is made is where the call goes.
//
// The Direct write path has no entity to carry the request, so it asks through
// its own only channel — the Values map — with the same verb:
//
//	w.Update(ctx, write.Values{"Status": "PAID", "PaidAt": write.Stamp}, q)
//
// WHY IT IS AN ASK AND NOT AN ASSIGNMENT. A timestamp the code writes is a
// timestamp the code can get wrong — the process clock drifts per replica, and
// nothing downstream can tell a skewed reading from a real one. So the field is
// never written from the struct; assigning o.PaidAt by hand does nothing. On a
// write that did NOT request it the column is left out of the statement
// entirely, which is also why an already-stamped row keeps its value with nobody
// having to remember to preserve it.
//
// The request belongs to one write, not to the entity's state: it is not
// persisted, not part of business identity, and does not survive into the Old()
// ghost. Naming a field the schema does not declare as stamped is an error
// raised by the write, not a panic — the domain cannot see the schema.
//
// Requesting the same field twice is the same as once.
func (m *Managed) Stamp(goField string) {
	for _, s := range m.stamps {
		if s == goField {
			return
		}
	}
	m.stamps = append(m.stamps, goField)
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

// stampCarrier is the read seam behind RequestedStamps: promoted from the
// embedded Managed, so any entity or aggregate child satisfies it structurally
// and infra never names the carrier type.
type stampCarrier interface{ requestedStamps() []string }

// requestedStamps takes a VALUE receiver, unlike every mutating method on this
// carrier. That is what lets an aggregate CHILD be read: children travel through
// the aggregate map as AggregateValueObject — an interface holding a struct
// VALUE, which is not addressable and therefore satisfies no pointer-receiver
// interface. Reading does not mutate, so the value receiver costs nothing and
// makes root, child and base-child all answerable through one seam.
func (m Managed) requestedStamps() []string { return m.stamps }

// RequestedStamps returns the stamped fields target asked the framework to fill
// on this write, by Go field name, in request order — the write path's read of
// what Stamp accumulated. Nil for a target that embeds no carrier or requested
// nothing. Pass a POINTER, the same way SetManagedColumns is called.
//
// It is the counterpart of Events(): a write reads the entity's accumulated
// intent, translates it through the schema, and the intent itself is never
// persisted.
func RequestedStamps(target any) []string {
	c, ok := target.(stampCarrier)
	if !ok {
		return nil
	}
	return c.requestedStamps()
}
