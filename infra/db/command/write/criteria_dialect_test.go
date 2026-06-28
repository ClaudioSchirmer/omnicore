package write

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// testPGDialect is a Postgres-flavored db.Dialect used by the criteria
// translator tests. The translator is backend-neutral (it renders SQL through
// whichever Dialect it is handed); these tests assert the Postgres rendering
// ("$n" placeholders, "ILIKE"), so they drive the translator with this local
// stand-in rather than importing the real engine (db must not depend on a
// concrete backend). It mirrors the PG engine's pgDialect behavior for the
// surface the translator exercises.
type testPGDialect struct{}

func (testPGDialect) Placeholder(n int) string      { return fmt.Sprintf("$%d", n) }
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
func (testPGDialect) IsUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}
func (testPGDialect) BuildUpsert(table string, _, _ []string, _ []UpsertSet) string {
	return "INSERT " + table
}
