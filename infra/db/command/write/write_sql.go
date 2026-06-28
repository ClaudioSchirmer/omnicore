package write

import (
	"fmt"
	"strings"

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

// buildInsert renders the INSERT for the bound columns + the managed NOW()
// columns, with the Go-generated PK prepended. Placeholders, identifier quoting
// and value encoding all come from the Dialect, so the one statement serves both
// backends: EncodeArg renders the PK (and any UUID-shaped value) in the dialect's
// wire form — uuid text on Postgres, BINARY(16) on MySQL. No RETURNING: the id
// is known up front.
func buildInsert(d Dialect, table, pk, id string, fields domain.Fields, nowCols []string) (string, []any) {
	keys := SortedKeys(fields)
	cols := make([]string, 0, len(keys)+1+len(nowCols))
	phs := make([]string, 0, len(keys)+1+len(nowCols))
	args := make([]any, 0, len(keys)+1)

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
		phs = append(phs, "NOW()")
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		d.QuoteIdent(table), strings.Join(cols, ", "), strings.Join(phs, ", "))
	return sql, args
}

// buildUpdate renders the UPDATE for the bound columns + the managed NOW()
// columns, keyed on the PK. Existence is checked by the caller via the
// rows-affected count (no RETURNING) — uniform across dialects.
func buildUpdate(d Dialect, table, pk, id string, fields domain.Fields, nowCols []string) (string, []any) {
	keys := SortedKeys(fields)
	sets := make([]string, 0, len(keys)+len(nowCols))
	args := make([]any, 0, len(keys)+1)
	n := 0
	for _, k := range keys {
		n++
		sets = append(sets, d.QuoteIdent(k)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(fields[k]))
	}
	for _, nc := range nowCols {
		sets = append(sets, d.QuoteIdent(nc)+" = NOW()")
	}
	n++
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s",
		d.QuoteIdent(table), strings.Join(sets, ", "), d.QuoteIdent(pk), d.Placeholder(n))
	args = append(args, d.EncodeArg(domain.NewID(id)))
	return sql, args
}

func archiveSQL(d Dialect, table, sdCol, pk string) string {
	return fmt.Sprintf("UPDATE %s SET %s = NOW() WHERE %s = %s",
		d.QuoteIdent(table), d.QuoteIdent(sdCol), d.QuoteIdent(pk), d.Placeholder(1))
}

func unarchiveSQL(d Dialect, table, sdCol, pk string) string {
	return fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s = %s",
		d.QuoteIdent(table), d.QuoteIdent(sdCol), d.QuoteIdent(pk), d.Placeholder(1))
}

func deleteSQL(d Dialect, table, pk string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s = %s",
		d.QuoteIdent(table), d.QuoteIdent(pk), d.Placeholder(1))
}

// childCascadeSQL renders the symmetric archive/unarchive cascade on a child
// table: set the soft-delete column (setExpr = NOW() to archive, NULL to
// unarchive) for children of the root whose state matches the gate (" IS NULL" =
// active, " IS NOT NULL" = archived). The single arg is the root id.
func childCascadeSQL(d Dialect, childTable, childSd, fkCol, setExpr, gate string) string {
	return fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = %s AND %s%s",
		d.QuoteIdent(childTable), d.QuoteIdent(childSd), setExpr,
		d.QuoteIdent(fkCol), d.Placeholder(1), d.QuoteIdent(childSd), gate)
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
