package write

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// testPGDialect is a Postgres-flavored db.Dialect used by the criteria
// translator tests. The translator is backend-neutral (it renders SQL through
// whichever Dialect it is handed); these tests assert the Postgres rendering
// ("$n" placeholders, "ILIKE"), so they drive the translator with this local
// stand-in rather than importing the real engine (db must not depend on a
// concrete backend). It mirrors the PG engine's pgDialect behavior for the
// surface the translator exercises.
type testPGDialect struct{}

func (testPGDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (testPGDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return name
}
func (testPGDialect) EncodeArg(val any) any {
	if id, ok := val.(domain.ID); ok {
		return id.Value()
	}
	return val
}
func (testPGDialect) DecodeID(raw string) (string, error) {
	return raw, nil
}
func (testPGDialect) ILikeClause(col, ph string) string { return col + " ILIKE " + ph }
func (testPGDialect) LikeClause(col, ph string) string  { return col + " LIKE " + ph }
func (testPGDialect) NowExpr() string                   { return "NOW()" }
func (testPGDialect) UTCNowExpr() string                { return "NOW() AT TIME ZONE 'UTC'" }
func (testPGDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}
func (testPGDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", sql, limit, offset)
}
func (testPGDialect) Savepoint(name string) string           { return "SAVEPOINT " + name }
func (testPGDialect) RollbackToSavepoint(name string) string { return "ROLLBACK TO SAVEPOINT " + name }
func (testPGDialect) ReleaseSavepoint(name string) string    { return "RELEASE SAVEPOINT " + name }
func (testPGDialect) IsUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}
func (testPGDialect) IsForeignKeyViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// BuildUpsert renders the Postgres shape faithfully — identifiers unquoted, which
// is this fixture's convention — so a test can assert on the conflict clause the
// write path builds. Mirrors infra/db/engine/postgres; the engine packages import
// this one, so their real dialects cannot be reached from here.
func (d testPGDialect) BuildUpsert(table string, cols, conflictCols []string, sets []UpsertSet) string {
	phs := make([]string, len(cols))
	for i := range cols {
		phs[i] = d.Placeholder(i + 1)
	}
	sql := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(phs, ", ") + ")" +
		" ON CONFLICT (" + strings.Join(conflictCols, ", ") + ")"
	if len(sets) == 0 {
		return sql + " DO NOTHING"
	}
	parts := make([]string, len(sets))
	for i, s := range sets {
		switch s.Mode {
		case core.UpsertSetNew:
			parts[i] = s.Col + " = EXCLUDED." + s.Col
		case core.UpsertSetBump:
			parts[i] = s.Col + " = " + table + "." + s.Col + " + 1"
		default:
			parts[i] = s.Col + " = " + s.Expr
		}
	}
	return sql + " DO UPDATE SET " + strings.Join(parts, ", ")
}
