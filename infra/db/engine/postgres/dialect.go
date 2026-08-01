//go:build postgres

package postgres

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The Postgres core.Dialect implementation — every engine-specific statement
// fragment (read and write), split out of read.go once the seam grew write
// duties (upsert, savepoints, the now expression).

// Dialect returns the Postgres statement flavor.
func (p *Postgres) Dialect() core.Dialect { return pgDialect{} }

// pgDialect renders Postgres SQL: $n placeholders, bare (validated) identifiers,
// uuid args as text, and a native ILIKE.
type pgDialect struct{}

func (pgDialect) Placeholder(n int) string            { return fmt.Sprintf("$%d", n) }
func (pgDialect) QuoteIdent(name string) string       { return validIdentifier(name) }
func (pgDialect) EncodeArg(val any) any               { return normalizeArg(val) }
func (pgDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (pgDialect) ILikeClause(col, ph string) string   { return col + " ILIKE " + ph }
func (pgDialect) LikeClause(col, ph string) string    { return col + " LIKE " + ph }
func (pgDialect) NowExpr() string                     { return "NOW()" }

// ApplyLimit caps a complete SELECT at n rows — the native tail clause on
// Postgres.
func (pgDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}

// ApplyLimitOffset renders a windowed page onto a complete SELECT — the native
// `LIMIT n OFFSET m` tail on Postgres. Offset windowing is only defined over a
// deterministic ORDER BY; the caller guarantees it (see Dialect.ApplyLimitOffset).
func (pgDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", sql, limit, offset)
}

func (pgDialect) Savepoint(name string) string           { return "SAVEPOINT " + name }
func (pgDialect) RollbackToSavepoint(name string) string { return "ROLLBACK TO SAVEPOINT " + name }
func (pgDialect) ReleaseSavepoint(name string) string    { return "RELEASE SAVEPOINT " + name }

// IsUniqueViolation reads PG SQLSTATE 23505 and returns the violated
// constraint/index name.
func (pgDialect) IsUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// IsForeignKeyViolation reads PG SQLSTATE 23503 (foreign_key_violation) and
// returns the violated constraint name.
func (pgDialect) IsForeignKeyViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// BuildUpsert renders the Postgres upsert: `INSERT … VALUES … ON CONFLICT
// (conflictCols) DO UPDATE SET …` (or `DO NOTHING` when sets is empty). The
// proposed value for an core.UpsertSetNew assignment is `EXCLUDED.col`; a
// core.UpsertSetBump reads the existing row as `<table>.col` — inside DO UPDATE
// a bare column is ambiguous against EXCLUDED (SQLSTATE 42702), so the table
// qualifier is mandatory here.
func (d pgDialect) BuildUpsert(table string, cols, conflictCols []string, sets []core.UpsertSet) string {
	var b strings.Builder
	writeInsertHead(&b, d, table, cols)
	b.WriteString(" ON CONFLICT (")
	for i, c := range conflictCols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(")")
	if len(sets) == 0 {
		b.WriteString(" DO NOTHING")
		return b.String()
	}
	b.WriteString(" DO UPDATE SET ")
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(s.Col))
		b.WriteString(" = ")
		switch s.Mode {
		case core.UpsertSetNew:
			b.WriteString("EXCLUDED.")
			b.WriteString(d.QuoteIdent(s.Col))
		case core.UpsertSetBump:
			b.WriteString(d.QuoteIdent(table))
			b.WriteString(".")
			b.WriteString(d.QuoteIdent(s.Col))
			b.WriteString(" + 1")
		default:
			b.WriteString(s.Expr)
		}
	}
	return b.String()
}

// writeInsertHead renders the dialect-neutral `INSERT INTO t (cols) VALUES (ph…)`
// prefix shared by every upsert (the placeholders come from the dialect, so it
// reads `$n` on PG and `?` on MySQL). Exported-package-local — the MySQL engine
// builds its own head in its own package (the mirror stays self-contained).
func writeInsertHead(b *strings.Builder, d core.Dialect, table string, cols []string) {
	b.WriteString("INSERT INTO ")
	b.WriteString(d.QuoteIdent(table))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(") VALUES (")
	for i := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Placeholder(i + 1))
	}
	b.WriteString(")")
}
