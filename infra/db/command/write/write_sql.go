package write

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// newWriteID mints the framework-authoritative id for a new row: a UUID v7
// (time-ordered, so a clustered ID stays local) generated in Go on EVERY
// backend. No dialect generates ids anymore — Postgres' gen_random_uuid()
// DEFAULT is simply overridden and MySQL never had one — so the INSERT is
// byte-identical across dialects and carries no RETURNING.
func newWriteID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("db: uuid v7: %w", err)
	}
	return id.String(), nil
}

// writeNow mints the single authoritative timestamp of one write operation.
// Managed columns (created_at/updated_at and the DeletedAt stamp) are
// application-clock values bound as ordinary arguments — the same move the ids
// made with the Go-minted UUID v7: no dialect NOW() expression in the data DML,
// so every statement of one operation (root, children, siblings, base cascade)
// carries the SAME instant, known in Go before COMMIT (the outbox payload can
// therefore carry it too). UTC, truncated to microseconds — the precision every
// supported backend stores (timestamptz / DATETIME(6) / DATETIME2(6) /
// TIMESTAMP(6)) — so the value bound here and the value the composer reads back
// are identical. Minted once per verb call and threaded down, never per
// statement.
func writeNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// buildInsert renders the INSERT for the bound columns + the managed timestamp
// columns (bound to the operation stamp `now`), with the Go-generated ID
// prepended. Placeholders, identifier quoting and value encoding all come from
// the Dialect, so the one statement serves both backends: EncodeArg renders the
// ID (and any UUID-shaped value) in the dialect's wire form — uuid text on
// Postgres, BINARY(16) on MySQL. No RETURNING: the id AND the timestamps are
// known up front.
func buildInsert(d Dialect, table, pk, id string, fields domain.Fields, nowCols []string, now time.Time, revCol string) (string, []any) {
	keys := SortedKeys(fields)
	cols := make([]string, 0, len(keys)+1+len(nowCols))
	phs := make([]string, 0, len(keys)+1+len(nowCols))
	args := make([]any, 0, len(keys)+1+len(nowCols))

	n := 0
	cols = append(cols, d.QuoteIdent(pk))
	n++
	phs = append(phs, d.Placeholder(n))
	args = append(args, d.EncodeArg(domain.NewID(id)))

	for _, k := range keys {
		cols = append(cols, d.QuoteIdent(k))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, d.EncodeArg(fields[k]))
	}
	for _, nc := range nowCols {
		cols = append(cols, d.QuoteIdent(nc))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, d.EncodeArg(now))
	}
	if revCol != "" {
		// A fresh row starts its commit-order token at 1 — appended here so the
		// caller's fields map stays untouched (it flows into the outbox payload
		// and WriteResult.Fields; the physical revision column must never leak
		// there — the document form is the _revision watermark).
		cols = append(cols, d.QuoteIdent(revCol))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, int64(1))
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		d.QuoteIdent(table), strings.Join(cols, ", "), strings.Join(phs, ", "))
	return sql, args
}

// buildUpdate renders the UPDATE for the bound columns + the managed timestamp
// columns (bound to the operation stamp `now`), keyed on the ID. Existence is
// checked by the caller via the rows-affected count (no RETURNING) — uniform
// across dialects.
//
// expectedRevision is the
// OPTIMISTIC-CONCURRENCY guard: when it is > 0 (the entity came from a load,
// see domain.Managed.GetRevision) and the schema declares a revision column,
// the WHERE also pins that revision, so the statement matches only while the
// row still holds the value the caller read. A stale write then matches zero
// rows instead of reverting whatever landed in between — execExpectingRow turns
// that into ConcurrentModificationNotification (409).
//
// Pass 0 to write unguarded: an aggregate CHILD (guarded by its owner's token,
// it declares no revision of its own), a shared BASE (converged last-write-wins
// on purpose, since several roles write it), or an entity that never came from
// the loader.
func buildUpdate(d Dialect, table, pk, id string, fields domain.Fields, nowCols []string, now time.Time, revCol string, expectedRevision int64) (string, []any) {
	keys := SortedKeys(fields)
	sets := make([]string, 0, len(keys)+len(nowCols)+1)
	args := make([]any, 0, len(keys)+len(nowCols)+1)
	n := 0
	for _, k := range keys {
		n++
		sets = append(sets, d.QuoteIdent(k)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(fields[k]))
	}
	for _, nc := range nowCols {
		n++
		sets = append(sets, d.QuoteIdent(nc)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(now))
	}
	if revCol != "" {
		rc := d.QuoteIdent(revCol)
		sets = append(sets, rc+" = "+rc+" + 1")
	}
	n++
	where := d.QuoteIdent(pk) + " = " + d.Placeholder(n)
	args = append(args, d.EncodeArg(domain.NewID(id)))
	if revCol != "" && expectedRevision > 0 {
		n++
		where += " AND " + d.QuoteIdent(revCol) + " = " + d.Placeholder(n)
		args = append(args, d.EncodeArg(expectedRevision))
	}
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		d.QuoteIdent(table), strings.Join(sets, ", "), where)
	return sql, args
}

// rowExistsSQL renders the probe that splits a zero-row guarded UPDATE into its
// two causes: the row is gone (404) or it moved past the caller's revision
// (409). It runs only on that failure path, so the happy path pays nothing.
func rowExistsSQL(d Dialect, table, pk string) string {
	return d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(table)+
		" WHERE "+d.QuoteIdent(pk)+" = "+d.Placeholder(1), 1)
}

// archiveSQL archives ONE row by id: the archive stamp binds as the first arg
// (the operation's writeNow() value), the ID as the second — the same app-clock
// stamp every other statement of the operation carries.
//
// Its one caller is archiveChild: a child the aggregate marked Removed during an
// UPDATE. The ROOT verbs do not come through here — Archive/Unarchive write the
// entity's full field set with the DeletedAt transition as one more column (see
// softWrite), so the row's business state and the event that announces it can
// never disagree.
func archiveSQL(d Dialect, table, sdCol, pk, revCol string) string {
	bump := ""
	if revCol != "" {
		rc := d.QuoteIdent(revCol)
		bump = ", " + rc + " = " + rc + " + 1"
	}
	return fmt.Sprintf("UPDATE %s SET %s = %s%s WHERE %s = %s",
		d.QuoteIdent(table), d.QuoteIdent(sdCol), d.Placeholder(1), bump, d.QuoteIdent(pk), d.Placeholder(2))
}

func deleteSQL(d Dialect, table, pk string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s = %s",
		d.QuoteIdent(table), d.QuoteIdent(pk), d.Placeholder(1))
}

// childDeleteSQL renders the hard-delete of every child row belonging to a root:
// DELETE FROM child WHERE fk = $1. The aggregate delete path issues one per
// declared child table (before the root DELETE, same TX), so the framework owns
// the cascade in Go instead of depending on a database ON DELETE CASCADE. The
// single arg is the root id.
func childDeleteSQL(d Dialect, childTable, fkCol string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s = %s",
		d.QuoteIdent(childTable), d.QuoteIdent(fkCol), d.Placeholder(1))
}

// archiveCascadeSQL renders the ARCHIVE direction of the symmetric cascade: set
// the DeletedAt column to the operation stamp (bound as the FIRST arg) for
// the ACTIVE children of the root (second arg). Gated on `IS NULL` so it is
// idempotent, never re-stamps an already-archived child — and so the stamp it
// writes is, for every row it touches, the SAME instant the root row carries:
// one writeNow() per operation, bound here and by the root UPDATE alike. That
// equality is not a coincidence to preserve casually — it IS the discriminator
// unarchiveCascadeSQL reads back.
func archiveCascadeSQL(d Dialect, childTable, childSd, fkCol string) string {
	return fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = %s AND %s IS NULL",
		d.QuoteIdent(childTable), d.QuoteIdent(childSd), d.Placeholder(1),
		d.QuoteIdent(fkCol), d.Placeholder(2), d.QuoteIdent(childSd))
}

// unarchiveCascadeSQL renders the UNARCHIVE direction of the symmetric cascade:
// clear the DeletedAt column of the children the ROOT'S OWN archive stamped —
// the root id binds first, the root's archive stamp second.
//
// The gate used to be `IS NOT NULL` ("every archived child"), and that is the
// bug it exists to fix: a child archived on its own two years ago has nothing to
// do with the root's archive, yet the root's unarchive resurrected it along with
// the rest. The two cases are already distinguishable in the data, because the
// archive cascade binds the operation's single writeNow() instant on the root
// row AND on every child row it stamps, while skipping (IS NULL gate) the
// children that were already archived — those keep their own, older stamp. So
// "was archived BY this root's archive" reads exactly as "carries the root's
// archive stamp", with no marker column and no backfill: rows written by earlier
// versions of the framework already carry it.
//
// The comparison is an equality between two stored timestamps, so it is only as
// sharp as the columns' precision: root and child DeletedAt columns must share
// the same type, with sub-second precision (DATETIME(6), TIMESTAMP(6),
// DATETIME2(6), TIMESTAMPTZ — what the generator emits). A second-precision
// column collapses two operations that happened within the same second into one
// stamp; a child column COARSER than the root's truncates the stamp it was
// given and stops matching, which fails safe (nothing is revived) but silently.
func unarchiveCascadeSQL(d Dialect, childTable, childSd, fkCol string) string {
	return fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s = %s AND %s = %s",
		d.QuoteIdent(childTable), d.QuoteIdent(childSd),
		d.QuoteIdent(fkCol), d.Placeholder(1), d.QuoteIdent(childSd), d.Placeholder(2))
}

// childSiblingDeleteSQL hard-deletes a child's sibling rows when the root is
// deleted. The children are removed in bulk by their ParentID to the root (no per-row
// ids), so a child's siblings — keyed on the child's shared ID — are removed via
// a subquery over the soon-to-be-deleted child rows. Must run BEFORE the child
// rows are deleted (the subquery reads them). The single arg is the root id.
func childSiblingDeleteSQL(d Dialect, sibTable, childPKCol, childTable, childFKCol string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s IN (SELECT %s FROM %s WHERE %s = %s)",
		d.QuoteIdent(sibTable), d.QuoteIdent(childPKCol),
		d.QuoteIdent(childPKCol), d.QuoteIdent(childTable),
		d.QuoteIdent(childFKCol), d.Placeholder(1))
}

// buildSiblingUpsert renders the INSERT-or-update of one sibling row keyed on
// the shared ID: INSERT (pk + fields) ON CONFLICT(pk) DO UPDATE each field to the
// proposed value. Dialect-agnostic via Dialect.BuildUpsert (PG ON CONFLICT ⟷
// MySQL ON DUPLICATE KEY). Args bind in cols order (pk first, then SortedKeys);
// managed timestamp columns are not handled here (siblings carry plain fields).
func buildSiblingUpsert(d Dialect, sib *TableSchema, pkCol, id string, fields domain.Fields) (string, []any) {
	keys := SortedKeys(fields)
	cols := make([]string, 0, len(keys)+1)
	args := make([]any, 0, len(keys)+1)
	sets := make([]UpsertSet, 0, len(keys))

	cols = append(cols, pkCol)
	args = append(args, d.EncodeArg(domain.NewID(id)))
	for _, k := range keys {
		cols = append(cols, k)
		args = append(args, d.EncodeArg(fields[k]))
		sets = append(sets, UpsertSet{Col: k, Mode: UpsertSetNew})
	}
	return d.BuildUpsert(sib.Table(), cols, []string{pkCol}, sets), args
}

// requireDeletedAt is the runtime backstop for the boot-time Modes() ⟺
// DeletedAt check: a write path needing the DeletedAt column on a schema that
// did not declare it fails loudly instead of emitting broken SQL.
func requireDeletedAt(s *TableSchema, entityName string) (string, error) {
	col, ok := s.DeletedAtColumn()
	if !ok {
		return "", fmt.Errorf(
			"db: %s did not declare DeletedAt in its TableSchema — archive/unarchive is unavailable",
			entityName,
		)
	}
	return col, nil
}
