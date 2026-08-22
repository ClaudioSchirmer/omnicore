package query

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// AggregateReader is the type-erased face of the aggregate loader a
// relational-backed read model is declared over. It loads a whole aggregate —
// root plus its own children, siblings, shared base, with the domain.Managed
// carrier populated — from the relational backend through a neutral
// criteria.Query, WITHOUT the caller naming the entity's Go type, and it exposes
// the core.TableSchema it is bound to.
//
// read.AggregateLoader[T] satisfies it structurally (FindAllEntities /
// CountEntities / Schema), so a read model carries its repository's ALREADY-BUILT
// loader — repo.Loader — rather than a second SQL builder over the same schema.
// That is the point: the criteria→SQL translation is subtle (the sibling and
// shared-base LEFT JOINs, the id qualification once a join exists, the archived
// scope gate, the limit/offset window) and it has exactly one implementation. A
// read model declared over the loader inherits every future evolution of it —
// notably whatever joins the TableSchema learns to declare — instead of needing
// the same evolution implemented twice and drifting the first time one side is
// forgotten.
//
// It lives in this package (not read's) so a view definition can hold it without
// the view layer importing the load layer; criteria and core are low-level and
// shared by both, so query -> criteria/core introduces no cycle.
type AggregateReader interface {
	// FindAllEntities loads every aggregate matching q — root plus its closure,
	// managed columns populated — honoring q's order, window (limit/offset) and
	// scope (active / include-archived / only-archived). An empty slice, not an
	// error, when nothing matches.
	FindAllEntities(ctx context.Context, q *criteria.Query) ([]domain.Entity, error)
	// CountEntities returns how many roots match q — COUNT(*) under the same
	// filter and scope, nothing materialized — backing the only-total read.
	CountEntities(ctx context.Context, q *criteria.Query) (int64, error)
	// Schema is the declaration the loader reads through: the Go↔column map, the
	// id, the managed columns, the children, the siblings and the shared base. A
	// read model takes its schema from here, so schema and loader cannot disagree.
	Schema() *core.TableSchema
	// JoinFields names the Go fields a declared READ JOIN adds beyond the schema,
	// keyed by the table they land on — the root's table for a root join, the
	// child's for a child join. They are ordinary fields of the loaded entity, so a
	// read model serves them like any other; they are simply absent from the
	// TableSchema, which is why a reader has to be told about them.
	//
	// Only the ROOT's entry is addressable in a criteria: a child join is
	// load-only, since filtering the root by a field of a 1:N child is a pushdown
	// a single root SELECT cannot express.
	JoinFields() map[string][]string
}
