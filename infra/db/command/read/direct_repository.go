package read

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// DirectRepository[T] is the repository over ONE table with no aggregate and no
// domain entity behind it: a control table the service maintains by hand, or an
// aggregate's child addressed as a plain table to take a fact from it.
//
// It is not a second engine. The reads run through the SAME directCore an
// AggregateLoader runs through — the same criteria compilation, the same
// TableSchema.Resolve, the same declared joins, the same aggregate DSL — and the
// writes through the same statement builders and dialect seam the entity write
// path uses. What it does not carry is what belongs to an aggregate: no children,
// no siblings, no shared base, no outbox row, no audit event, no domain events,
// no revision guard, no old-state snapshot.
//
// Anchor: a schema built with core.NewDirectSchema, and nothing else. A join
// TARGET is unconstrained — a traversal only reads — so a control table joins
// `users` through the very schema the User entity already declares.
//
//	jobs := read.NewDirectRepository[ImportJob](eng, ImportJobTable()).
//	    WithJoins(read.InnerJoin(userinfra.UserSchema()).On("owner_id").Field("OwnerName", "name"))
//
//	id, err := jobs.Insert(ctx, write.Values{"Status": "pending", "RunAt": t})
//	n, err := jobs.Update(ctx, write.Values{"Status": "done"}, criteria.Where(...))
//	rows, err := jobs.FindAll(ctx, criteria.Where(...).OrderBy("RunAt").Limit(50))
type DirectRepository[T any] struct {
	directCore
	*write.DirectWriter
}

// NewDirectRepository binds a Direct schema to an engine. The schema comes in at
// construction rather than through a later WithSchema, and no row factory is
// needed: the schema is already anchored to T, so the type is cross-checked here
// and a row is allocated by reflection. An entity needs its factory because
// BaseEntity has to be initialized; a plain row has nothing to initialize.
//
// Everything that can be known now is checked now — the schema is Direct, it
// declares a primary key, and it is anchored to T — so a miswired repository
// fails at boot, never on the first request.
func NewDirectRepository[T any](eng RelationalEngine, schema *TableSchema) *DirectRepository[T] {
	name := TypeName[T]()
	validateDirectAnchor[T](name, schema)
	return &DirectRepository[T]{
		directCore:   directCore{eng: eng, schema: schema, name: name},
		DirectWriter: write.NewDirectWriter(eng, schema, name),
	}
}

// validateDirectAnchor refuses, at construction, every schema this repository
// cannot honestly anchor on.
func validateDirectAnchor[T any](name string, schema *TableSchema) {
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("read.NewDirectRepository[%s]: ", name) + fmt.Sprintf(format, args...))
	}
	if schema == nil {
		fail("the schema is mandatory")
	}
	if !schema.IsDirect() {
		fail("the schema for %q was not built with core.NewDirectSchema — this repository maps ONE "+
			"table and offers none of what an aggregate's schema declares (children, siblings, a "+
			"shared base, the revision guard). An entity's schema belongs to the aggregate "+
			"repository; it is still usable HERE as a join target, which only reads.",
			schema.Table())
	}
	if !schema.HasPKDeclared() {
		fail("the schema for %q declares no primary key — add .ID(column)", schema.Table())
	}
	want := reflect.TypeOf((*T)(nil)).Elem()
	for want.Kind() == reflect.Pointer {
		want = want.Elem()
	}
	if got := schema.AnchoredType(); got != want {
		fail("the schema for %q is anchored to %s, not %s — one schema, one row type",
			schema.Table(), got, want)
	}
}

// WithJoins declares the READ-ONLY traversals this table may reach across — the
// horizontal reach, on the root, exactly as an aggregate repository declares it.
// The `…InChild` forms do not exist here: there is no child.
//
// It takes the whole set and may be called ONCE; see directCore.declareJoins.
func (r *DirectRepository[T]) WithJoins(bindings ...*JoinBinding) *DirectRepository[T] {
	r.declareJoins(bindings...)
	return r
}

// WithContextName overrides the name this repository raises notifications and
// panics under. It defaults to the row type's name.
func (r *DirectRepository[T]) WithContextName(name string) *DirectRepository[T] {
	r.name = name
	r.SetContextName(name)
	return r
}

// WithConstraints declares the unique-constraint → notification bindings, so a
// violation surfaces as the typed notification instead of a raw driver error —
// the same mapping an entity repository does through its Constraints field.
func (r *DirectRepository[T]) WithConstraints(m map[string]write.ConstraintBinding) *DirectRepository[T] {
	r.SetConstraints(m)
	return r
}

// InTx returns a COPY of this repository bound to the framework's OPEN
// transaction, so its reads see that transaction's uncommitted rows and its
// writes commit — or roll back — with it. Recover the neutral Tx from a
// lifecycle hook's sealed handle via core.UnwrapTx.
//
//	jobs.InTx(core.UnwrapTx(tx)).Insert(ctx, write.Values{…})
//
// The receiver is untouched: a repository built once at boot and shared across
// requests is never mutated by one of them.
func (r *DirectRepository[T]) InTx(tx core.Tx) *DirectRepository[T] {
	cp := *r
	cp.directCore.tx = tx
	cp.DirectWriter = r.DirectWriter.BindTx(tx)
	return &cp
}

// FindOne loads exactly one row matching the criteria. Returns a *DomainError
// carrying RecordNotFound (HTTP 404) when nothing matches, and refuses a second
// match: the contract is "expected one", and >1 is a developer mistake surfaced
// loudly rather than silently resolved by picking a row. A LIMIT 2 probe bounds
// the check, so any Limit set on the Query is overridden.
func (r *DirectRepository[T]) FindOne(ctx context.Context, q *criteria.Query) (T, error) {
	var zero T
	rows, err := r.findRows(ctx, q, 2)
	if err != nil {
		return zero, err
	}
	switch len(rows) {
	case 0:
		return zero, domain.NotFoundError(r.name, r.schema.IDColumn(), "")
	case 1:
		return rows[0], nil
	default:
		return zero, fmt.Errorf(
			"read.FindOne[%s]: the criteria matched more than one row of %q — FindOne expects one; "+
				"use FindAll when several are legitimate", r.name, r.schema.Table())
	}
}

// FindAll loads every row matching the criteria, honoring its order, window and
// archived scope. An empty result is an empty slice, never a not-found error.
func (r *DirectRepository[T]) FindAll(ctx context.Context, q *criteria.Query) ([]T, error) {
	return r.findRows(ctx, q, 0)
}

// findRows is the one read this repository issues: the schema's own columns
// (the id among them — a Direct row declares it as a field), the declared joins'
// columns after them, under the criteria the shared compiler renders.
//
// It is findRoots minus everything that is about an entity: no leading key read
// back, no SetID, no old-state snapshot, no children, siblings or shared base to
// hydrate. What is left is the same statement assembly.
func (r *DirectRepository[T]) findRows(ctx context.Context, q *criteria.Query, limitOverride int64) ([]T, error) {
	if q == nil {
		q = criteria.Where(nil)
	}
	dialect := r.eng.Dialect()
	joins := newJoinedTables(r.joins)
	resolve := r.resolverRecordingJoins(joins)
	qual := core.ColQual{Owner: len(rootJoins(r.joins)) > 0}

	order, err := core.CompileOrderQualified(q.OrderFields(), resolve, dialect, qual)
	if err != nil {
		return nil, err
	}
	fromJoin, clause, args, err := r.compileFilterJoins(q, joins)
	if err != nil {
		return nil, err
	}

	cols, byCol := r.schema.ScanPlan()
	if len(cols) == 0 {
		return nil, fmt.Errorf(
			"read.DirectRepository[%s]: the schema for %q declares no columns — declare them with "+
				"TableSchema.Field(...)", r.name, r.schema.Table())
	}
	anchor := ""
	if joins.any() {
		anchor = dialect.QuoteIdent(r.schema.Table())
	}
	selectCols := qualifyIdentifiers(cols, dialect, anchor)
	if exprs := joinSelectExprs(r.rootJoinNodes(), dialect); len(exprs) > 0 {
		selectCols = append(selectCols, exprs...)
	}

	limit, offset := q.LimitValue(), q.OffsetValue()
	if limitOverride > 0 {
		limit, offset = limitOverride, 0
	}
	// compileFilterJoins hands back a clause that already carries its leading
	// separator, so it concatenates straight onto the FROM (the probe and the
	// aggregate paths rely on that too); only the ORDER BY needs one added.
	sql := "SELECT " + strings.Join(selectCols, ", ") + " FROM " + fromJoin + clause
	if order != "" {
		sql += " " + order
	}
	sql, err = applyWindow(dialect, sql, limit, offset, order)
	if err != nil {
		return nil, err
	}

	cursor, err := r.rows().Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer cursor.Close()

	var out []T
	for cursor.Next() {
		var row T
		targets, err := joinScanTargets(any(&row), r.rootJoinNodes())
		if err != nil {
			return nil, err
		}
		if err := core.ScanRowTrailing(cursor, &row, cols, byCol, targets...); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, cursor.Err()
}
