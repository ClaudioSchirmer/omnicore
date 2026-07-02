package query

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// validIdentifier guards a table/column name before it is concatenated into a
// rebuild/drift SQL string. The Mongo-view control plane is relational and
// backend-neutral — it builds these statements on the configured engine and
// renders the dialect-divergent bits via core.Dialect, but the engine seam
// parameterizes values, not identifiers, so it shares the framework's identifier
// policy via core.SafeIdentifier. Identifiers originate in declared view schemas,
// never user input — a miss is a programming error and panics (SQL-injection defense).
func validIdentifier(name string) string {
	if !core.SafeIdentifier(name) {
		panic(fmt.Sprintf("infra: invalid SQL identifier %q", name))
	}
	return name
}
