package write

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// newWriteID mints the framework-authoritative id for a new row: a UUID v7
// (time-ordered, so a clustered PK stays local) generated in Go on EVERY
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
// Managed columns (created_at/updated_at and the soft-delete stamp) are
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
// columns (bound to the operation stamp `now`), with the Go-generated PK
// prepended. Placeholders, identifier quoting and value encoding all come from
// the Dialect, so the one statement serves both backends: EncodeArg renders the
// PK (and any UUID-shaped value) in the dialect's wire form — uuid text on
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
// columns (bound to the operation stamp `now`), keyed on the PK. Existence is
// checked by the caller via the rows-affected count (no RETURNING) — uniform
// across dialects.
func buildUpdate(d Dialect, table, pk, id string, fields domain.Fields, nowCols []string, now time.Time, revCol string) (string, []any) {
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
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s",
		d.QuoteIdent(table), strings.Join(sets, ", "), d.QuoteIdent(pk), d.Placeholder(n))
	args = append(args, d.EncodeArg(domain.NewID(id)))
	return sql, args
}

// archiveSQL soft-deletes one row: the archive stamp binds as the FIRST arg
// (the operation's writeNow() value), the PK as the second — the same
// app-clock stamp every other statement of the operation carries.
func archiveSQL(d Dialect, table, sdCol, pk, revCol string) string {
	bump := ""
	if revCol != "" {
		rc := d.QuoteIdent(revCol)
		bump = ", " + rc + " = " + rc + " + 1"
	}
	return fmt.Sprintf("UPDATE %s SET %s = %s%s WHERE %s = %s",
		d.QuoteIdent(table), d.QuoteIdent(sdCol), d.Placeholder(1), bump, d.QuoteIdent(pk), d.Placeholder(2))
}

func unarchiveSQL(d Dialect, table, sdCol, pk, revCol string) string {
	bump := ""
	if revCol != "" {
		rc := d.QuoteIdent(revCol)
		bump = ", " + rc + " = " + rc + " + 1"
	}
	return fmt.Sprintf("UPDATE %s SET %s = NULL%s WHERE %s = %s",
		d.QuoteIdent(table), d.QuoteIdent(sdCol), bump, d.QuoteIdent(pk), d.Placeholder(1))
}

// nullSetExpr is the unarchive assignment of the symmetric cascade (SQL NULL —
// no bound value). The archive direction binds the operation stamp instead
// (archiveCascadeSQL); nowSetExpr remains only as the dialect-NOW counterpart
// for callers outside the data write path.
func nowSetExpr(d Dialect) string { return d.NowExpr() }

func nullSetExpr(Dialect) string { return "NULL" }

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

// childCascadeSQL renders the UNARCHIVE direction of the symmetric cascade on a
// child table: set the soft-delete column (setExpr, "NULL" on this path) for
// children of the root whose state matches the gate (" IS NOT NULL" =
// archived). The single arg is the root id. The archive direction binds the
// operation stamp and lives in archiveCascadeSQL.
func childCascadeSQL(d Dialect, childTable, childSd, fkCol, setExpr, gate string) string {
	return fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = %s AND %s%s",
		d.QuoteIdent(childTable), d.QuoteIdent(childSd), setExpr,
		d.QuoteIdent(fkCol), d.Placeholder(1), d.QuoteIdent(childSd), gate)
}

// archiveCascadeSQL renders the ARCHIVE direction of the symmetric cascade: set
// the soft-delete column to the operation stamp (bound as the FIRST arg) for
// the ACTIVE children of the root (second arg). Gated on `IS NULL` so it is
// idempotent and never re-stamps an already-archived child.
func archiveCascadeSQL(d Dialect, childTable, childSd, fkCol string) string {
	return fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = %s AND %s IS NULL",
		d.QuoteIdent(childTable), d.QuoteIdent(childSd), d.Placeholder(1),
		d.QuoteIdent(fkCol), d.Placeholder(2), d.QuoteIdent(childSd))
}

// childSiblingDeleteSQL hard-deletes a child's sibling rows when the root is
// deleted. The children are removed in bulk by their FK to the root (no per-row
// ids), so a child's siblings — keyed on the child's shared PK — are removed via
// a subquery over the soon-to-be-deleted child rows. Must run BEFORE the child
// rows are deleted (the subquery reads them). The single arg is the root id.
func childSiblingDeleteSQL(d Dialect, sibTable, childPKCol, childTable, childFKCol string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s IN (SELECT %s FROM %s WHERE %s = %s)",
		d.QuoteIdent(sibTable), d.QuoteIdent(childPKCol),
		d.QuoteIdent(childPKCol), d.QuoteIdent(childTable),
		d.QuoteIdent(childFKCol), d.Placeholder(1))
}

// buildSiblingUpsert renders the INSERT-or-update of one sibling row keyed on
// the shared PK: INSERT (pk + fields) ON CONFLICT(pk) DO UPDATE each field to the
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

// requireSoftDelete is the runtime backstop for the boot-time Modes() ⟺
// SoftDelete check: a write path needing the soft-delete column on a schema that
// did not declare it fails loudly instead of emitting broken SQL.
func requireSoftDelete(s *TableSchema, entityName string) (string, error) {
	col, ok := s.SoftDeleteColumn()
	if !ok {
		return "", fmt.Errorf(
			"db: %s did not declare SoftDelete in its TableSchema — archive/unarchive is unavailable",
			entityName,
		)
	}
	return col, nil
}
