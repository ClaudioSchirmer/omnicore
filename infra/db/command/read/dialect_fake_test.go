package read

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// testPGDialect is a Postgres-flavored core.Dialect used by this package's
// white-box tests (loader, aggregate DSL, joins), which assert the Postgres
// rendering — "$n" placeholders, ILIKE — without importing a concrete engine
// (the read layer must not depend on a backend).
//
// The criteria compiler carries its own copy of this fake in package core,
// where the compiler now lives. Go cannot share a _test.go symbol across
// packages, so the two fixtures are deliberate twins rather than one import:
// the alternative would be shipping a test fake inside production code.
type testPGDialect struct{}

func (testPGDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (testPGDialect) QuoteIdent(name string) string {
	if !SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return name
}
func (testPGDialect) EncodeArg(val any) any {
	switch v := val.(type) {
	case domain.ID:
		return v.Value()
	case *domain.ID:
		if v == nil {
			return nil
		}
		return v.Value()
	default:
		return val
	}
}
func (testPGDialect) DecodeID(raw string) (string, error) {
	return raw, nil
}
func (testPGDialect) ILikeClause(col, ph string) string { return col + " ILIKE " + ph }
func (testPGDialect) LikeClause(col, ph string) string  { return col + " LIKE " + ph }
func (testPGDialect) NowExpr() string                   { return "NOW()" }
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
func (testPGDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "INSERT " + table
}
