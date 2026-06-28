package mongo

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// validIdentifier guards a table/column name before it is concatenated into a
// rebuild/drift SQL string. The Mongo-view control plane is Postgres-only and
// builds these statements directly (the engine seam covers parameterized
// values, not identifiers), so it shares the framework's identifier policy via
// db.SafeIdentifier. Identifiers originate in declared view schemas, never user
// input — a miss is a programming error and panics (SQL-injection defense).
func validIdentifier(name string) string {
	if !db.SafeIdentifier(name) {
		panic(fmt.Sprintf("infra: invalid SQL identifier %q", name))
	}
	return name
}
