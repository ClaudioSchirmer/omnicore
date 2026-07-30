package write

import (
	"context"
	"reflect"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Sibling write helpers — the partition of one logical row across an owner table
// and its declared sibling tables (NewSiblingSchema), all sharing the owner's
// primary key (1:1). A sibling carries a disjoint subset of the SAME entity's
// fields, read from the same source struct via the sibling's own WriteFields.
//
// Materialization is conditional: a sibling whose fields are all nil is not
// written (the slice is absent for this row). On UPDATE the verb decides the
// all-nil case — a full replace (PUT) that cleared every sibling field removes
// the row; a partial update (PATCH) leaves it untouched. Siblings have no
// DeletedAt (the owner controls archive/unarchive), so only Insert/Update and
// hard delete touch them.
//
// These cover OWNER-level siblings (the aggregate root or a flat entity). A
// sibling declared on an aggregate child is validated at boot but its write
// wiring is a separate step.

// isNilValue reports whether a bound field value is a nil pointer/interface/
// map/slice (a genuinely-absent value). Value types (string, int, bool) are
// never nil — they are always "present".
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// allNilFields reports whether every value in fields is nil — the signal that a
// sibling slice is absent for this row.
func allNilFields(fields domain.Fields) bool {
	for _, v := range fields {
		if !isNilValue(v) {
			return false
		}
	}
	return true
}

// insertSiblings INSERTs each materialized sibling row, sharing the owner's ID
// (owner.IDColumn() = id). A sibling whose fields are all nil is skipped. src is
// the owner value the sibling fields are read from — a root Entity OR an
// aggregate child (AggregateValueObject), so it is typed `any`.
func insertSiblings(ctx context.Context, tx WriteTx, d Dialect, owner *TableSchema, src any, id string, now time.Time) error {
	for _, sib := range owner.Siblings() {
		fields := sib.WriteFields(src)
		if allNilFields(fields) {
			continue
		}
		sql, args := buildInsert(d, sib.Table(), owner.IDColumn(), id, fields, sib.InsertNowColumns(), now, "")
		if err := tx.Exec(ctx, sql, args...); err != nil {
			return err
		}
	}
	return nil
}

// applySiblingUpdates UPSERTs each sibling by the shared ID. The all-nil case is
// verb-driven: PATCH (partial) leaves the row untouched; PUT (full replace)
// removes it, because a full body that cleared every sibling field means the
// slice is gone.
func applySiblingUpdates(ctx context.Context, tx WriteTx, d Dialect, owner *TableSchema, src any, id string, partial bool) error {
	for _, sib := range owner.Siblings() {
		fields := sib.WriteFields(src)
		if allNilFields(fields) {
			if partial {
				continue
			}
			if err := tx.Exec(ctx, deleteSQL(d, sib.Table(), owner.IDColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
				return err
			}
			continue
		}
		sql, args := buildSiblingUpsert(d, sib, owner.IDColumn(), id, fields)
		if err := tx.Exec(ctx, sql, args...); err != nil {
			return err
		}
	}
	return nil
}

// deleteSiblings hard-deletes every sibling row by the shared ID (id), before
// the owner row is deleted, in the same TX.
func deleteSiblings(ctx context.Context, tx WriteTx, d Dialect, owner *TableSchema, id string) error {
	for _, sib := range owner.Siblings() {
		if err := tx.Exec(ctx, deleteSQL(d, sib.Table(), owner.IDColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
			return err
		}
	}
	return nil
}
