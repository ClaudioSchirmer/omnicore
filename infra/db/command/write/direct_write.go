package write

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// Values is the column set a Direct write binds, keyed by GO FIELD NAME — never
// by column. The schema resolves each name exactly as it resolves a criteria
// field, so the three-name model (wire ↔ Go ↔ column) holds on the write side
// too and no physical column name is ever written above infra.
//
// What it may NOT carry, each refused with the reason:
//
//   - "ID" — identity is the framework's to mint (UUID v7) and Insert RETURNS
//     it. One origin, everywhere.
//   - "CreatedAt" / "UpdatedAt" — the framework stamps them from the operation's
//     own instant when the schema declares them.
//   - "DeletedAt" — the archive transition has its own verbs (Archive /
//     Unarchive); writing the column directly would bypass them.
type Values map[string]any

// managedByVerb names the slots a Direct write may not bind directly, with what
// owns each one. Keyed by the Go name Values would spell.
var managedByVerb = map[string]string{
	idGoField:   "identity is minted by the framework and returned by Insert",
	"CreatedAt": "the framework stamps it from the operation's instant",
	"UpdatedAt": "the framework stamps it from the operation's instant",
	"DeletedAt": "the archive transition has its own verbs, Archive and Unarchive",
}

// resolveValues translates Values into the column-keyed map the statement
// builders bind, through the schema's OWN resolution — the same surface a
// criteria field resolves through, so a name works in a filter exactly where it
// works in a write.
func resolveValues(schema *TableSchema, v Values) (domain.Fields, error) {
	if len(v) == 0 {
		return nil, fmt.Errorf("db: a Direct write needs at least one value — an empty Values binds no column")
	}
	out := make(domain.Fields, len(v))
	for goField, val := range v {
		if why, managed := managedByVerb[goField]; managed {
			return nil, fmt.Errorf("db: %q cannot be written directly — %s", goField, why)
		}
		rf, ok := schema.Resolve(goField)
		if !ok {
			return nil, fmt.Errorf("db: unknown field %q on table %q — declare it with TableSchema.Field(...)",
				goField, schema.Table())
		}
		out[rf.Column] = val
	}
	return out, nil
}

// DirectWriter is the write half of a Direct repository: INSERT/UPDATE/DELETE
// over ONE table, with no aggregate behind it.
//
// What it deliberately does NOT do, and what that costs: no outbox row (so a
// Direct write never feeds a Mongo view), no audit event, no domain events, no
// revision guard, no cascade, no lifecycle hooks. It is a SQL command. A table
// that needs any of those is an entity, and the aggregate repository is its home.
//
// What it keeps: the framework's statement builders, the one criteria compiler,
// the dialect seam, the generated identity, and the typed unique-violation
// notification via the Constraints map.
//
// Every write runs inside a transaction — its own, opened through the engine's
// beginner exactly as an entity write is, or the caller's when the repository was
// bound with InTx. That is where the affected-row count comes from, uniformly on
// every engine.
type DirectWriter struct {
	eng         RelationalEngine
	beginner    WriteBeginner
	schema      *TableSchema
	name        string
	constraints map[string]ConstraintBinding
	// tx is the caller's OPEN transaction when the repository was bound with
	// InTx; nil means each write opens and commits its own.
	tx core.Tx
}

// NewDirectWriter binds the write half to an engine and a Direct schema. The
// engine's transaction beginner is resolved HERE, at construction, so a
// misconfigured composition root fails at boot rather than on the first write.
func NewDirectWriter(eng RelationalEngine, schema *TableSchema, name string) *DirectWriter {
	b, ok := any(eng).(WriteBeginner)
	if !ok {
		panic(fmt.Sprintf(
			"write.NewDirectWriter(%s): the relational engine %T cannot open a transaction — "+
				"every Direct write runs in one, exactly as an entity write does",
			schema.Table(), eng))
	}
	return &DirectWriter{eng: eng, beginner: b, schema: schema, name: name}
}

// BindTx returns a copy of this writer running inside tx instead of opening its
// own transaction. The receiver is left untouched, so a repository shared across
// requests is never mutated by one of them.
func (w *DirectWriter) BindTx(tx core.Tx) *DirectWriter {
	cp := *w
	cp.tx = tx
	return &cp
}

// SetConstraints declares the unique-constraint → notification bindings this
// writer translates violations through.
func (w *DirectWriter) SetConstraints(m map[string]ConstraintBinding) { w.constraints = m }

// SetContextName overrides the name notifications are raised under.
func (w *DirectWriter) SetContextName(name string) { w.name = name }

// Schema is the declaration this writer is bound to.
func (w *DirectWriter) Schema() *TableSchema { return w.schema }

// ---------------------------------------------------------------------------
// Verbs
// ---------------------------------------------------------------------------

// Insert binds one row and returns the identity the framework minted for it
// (UUID v7, like every other insert in the framework). Values never carries
// "ID"; the returned id is how the caller keeps it.
func (w *DirectWriter) Insert(ctx context.Context, v Values) (domain.ID, error) {
	fields, err := resolveValues(w.schema, v)
	if err != nil {
		return domain.ID{}, err
	}
	id, err := newWriteID()
	if err != nil {
		return domain.ID{}, err
	}
	sql, args := buildInsert(w.eng.Dialect(), w.schema.Table(), w.schema.IDColumn(), id, fields,
		w.schema.InsertNowColumns(), writeNow(), "")
	if _, err := w.run(ctx, sql, args); err != nil {
		return domain.ID{}, err
	}
	return domain.NewID(id), nil
}

// Update binds the given values on every row matching the criteria and returns
// how many were affected. The Query's archived scope gates the statement like it
// gates a read: by default only active rows are touched.
func (w *DirectWriter) Update(ctx context.Context, v Values, q *criteria.Query) (int64, error) {
	sql, args, err := w.updateStmt(v, q)
	if err != nil {
		return 0, err
	}
	return w.run(ctx, sql, args)
}

// updateStmt renders the UPDATE both Update and UpdateOne execute. They differ
// only in what they do with the affected-row count, never in what they emit.
func (w *DirectWriter) updateStmt(v Values, q *criteria.Query) (string, []any, error) {
	fields, err := resolveValues(w.schema, v)
	if err != nil {
		return "", nil, err
	}
	pred, err := w.predicate(q)
	if err != nil {
		return "", nil, err
	}
	return buildUpdate(w.eng.Dialect(), schemaTarget(w.schema), pred, fields,
		w.schema.UpdateNowColumns(), writeNow(), "", 0)
}

// UpdateOne is Update for a criteria the caller believes names exactly one row.
// Zero rows is a typed RecordNotFound; more than one is refused, and because the
// statement runs in a transaction the rows it already touched are rolled back.
func (w *DirectWriter) UpdateOne(ctx context.Context, v Values, q *criteria.Query) error {
	sql, args, err := w.updateStmt(v, q)
	if err != nil {
		return err
	}
	return w.runExpectingOne(ctx, sql, args, "UpdateOne")
}

// Delete removes every row matching the criteria — one statement against one
// table, no cascade — and returns how many were removed.
func (w *DirectWriter) Delete(ctx context.Context, q *criteria.Query) (int64, error) {
	sql, args, err := w.deleteStmt(q)
	if err != nil {
		return 0, err
	}
	return w.run(ctx, sql, args)
}

// deleteStmt renders the DELETE both Delete and DeleteOne execute.
func (w *DirectWriter) deleteStmt(q *criteria.Query) (string, []any, error) {
	pred, err := w.predicate(q)
	if err != nil {
		return "", nil, err
	}
	return deleteSQL(w.eng.Dialect(), schemaTarget(w.schema), pred)
}

// DeleteOne is Delete for a criteria the caller believes names exactly one row.
func (w *DirectWriter) DeleteOne(ctx context.Context, q *criteria.Query) error {
	sql, args, err := w.deleteStmt(q)
	if err != nil {
		return err
	}
	return w.runExpectingOne(ctx, sql, args, "DeleteOne")
}

// Archive stamps the DeletedAt column on every ACTIVE row matching the criteria.
// One table, no cascade: nothing else in the database learns about it.
func (w *DirectWriter) Archive(ctx context.Context, q *criteria.Query) (int64, error) {
	return w.transition(ctx, q, true)
}

// Unarchive clears the DeletedAt column on every ARCHIVED row matching the
// criteria — the statement gates on `deleted_at IS NOT NULL`, because that is
// what the verb means.
func (w *DirectWriter) Unarchive(ctx context.Context, q *criteria.Query) (int64, error) {
	return w.transition(ctx, q, false)
}

// UpdateAll binds the given values on EVERY row of the table, archived ones
// included. It is the deliberate sweep the empty-predicate refusal points at:
// the verb, not a filter that happened to come out empty, is what says so.
func (w *DirectWriter) UpdateAll(ctx context.Context, v Values) (int64, error) {
	fields, err := resolveValues(w.schema, v)
	if err != nil {
		return 0, err
	}
	d := w.eng.Dialect()
	sets, args := buildSet(d, fields, w.schema.UpdateNowColumns(), writeNow(), "")
	return w.run(ctx, fmt.Sprintf("UPDATE %s SET %s", d.QuoteIdent(w.schema.Table()), strings.Join(sets, ", ")), args)
}

// DeleteAll empties the table — every row, archived ones included.
func (w *DirectWriter) DeleteAll(ctx context.Context) (int64, error) {
	d := w.eng.Dialect()
	return w.run(ctx, "DELETE FROM "+d.QuoteIdent(w.schema.Table()), nil)
}

// ---------------------------------------------------------------------------
// Shared mechanics
// ---------------------------------------------------------------------------

// predicate renders the Query into the single Expr the statement is keyed on:
// the caller's condition AND the archived-scope gate.
//
// The gate is expressed as a criteria node over the "DeletedAt" name the schema
// already resolves, rather than as a clause spliced in beside the predicate —
// so it is compiled, qualified and bound by the same walk as everything else.
//
// An absent condition is refused HERE, before any statement exists: a criteria
// assembled conditionally that came out empty would otherwise become a
// full-table UPDATE or DELETE, and the sweep has its own verb.
func (w *DirectWriter) predicate(q *criteria.Query) (criteria.Expr, error) {
	if q == nil || q.Condition() == nil {
		return nil, fmt.Errorf(
			"db: a write on %q needs a predicate — an empty criteria would touch every row. "+
				"Use UpdateAll/DeleteAll when the whole table really is the target",
			w.schema.Table())
	}
	return gated(q.Condition(), q.Scope(), w.schema), nil
}

// gated ANDs the archived-scope condition onto a predicate when the schema
// declares DeletedAt, and returns the predicate untouched when it does not — the
// same "no column, no gate" rule the read path's scope gate follows.
func gated(pred criteria.Expr, scope criteria.Scope, schema *TableSchema) criteria.Expr {
	if _, ok := schema.DeletedAtColumn(); !ok {
		return pred
	}
	switch scope {
	case criteria.ScopeIncludeArchived:
		return pred
	case criteria.ScopeOnlyArchived:
		return criteria.And(pred, criteria.NotNull("DeletedAt"))
	default:
		return criteria.And(pred, criteria.IsNull("DeletedAt"))
	}
}

// transition renders Archive/Unarchive: the DeletedAt column written to the
// operation's instant or to NULL, gated on the side of the transition the verb
// comes FROM, so an already-archived row is not re-stamped and an active row is
// not "restored".
func (w *DirectWriter) transition(ctx context.Context, q *criteria.Query, archive bool) (int64, error) {
	verb, scope := "Unarchive", criteria.ScopeOnlyArchived
	if archive {
		verb, scope = "Archive", criteria.ScopeActive
	}
	sdCol, err := requireDeletedAt(w.schema, w.name)
	if err != nil {
		return 0, err
	}
	if q == nil || q.Condition() == nil {
		return 0, fmt.Errorf("db: %s on %q needs a predicate — an empty criteria would touch every row",
			verb, w.schema.Table())
	}
	if q.Scope() != criteria.ScopeActive {
		return 0, fmt.Errorf(
			"db: %s on %q sets the archived scope itself — the verb IS the scope, so the criteria must not declare one",
			verb, w.schema.Table())
	}
	now := writeNow()
	var stamp any
	if archive {
		stamp = now
	}
	sql, args, err := buildUpdate(w.eng.Dialect(), schemaTarget(w.schema),
		gated(q.Condition(), scope, w.schema), domain.Fields{sdCol: stamp},
		w.schema.UpdateNowColumns(), now, "", 0)
	if err != nil {
		return 0, err
	}
	return w.run(ctx, sql, args)
}

// runExpectingOne executes a statement that must affect exactly one row, and
// decides BEFORE the commit — which is the whole point.
//
// Zero rows is the framework's typed RecordNotFound (the same notification an
// entity write raises when its row is gone); more than one is a refusal naming
// the count. In both cases the transaction is abandoned rather than committed,
// so a criteria that turned out wider than the caller believed leaves the table
// exactly as it was. Running the multi-row verb first and inspecting its count
// afterwards would be too late: it commits its own transaction.
//
// Under InTx the rollback belongs to the transaction that OWNS this write: the
// error travels up to whoever opened it, and the framework's write path abandons
// the transaction on a hook error the same way.
//
// The id column carries the notification's field, with an empty value: the
// statement was keyed on a predicate, not on a primary key, and rendering the
// bound values into a message that may be logged is not worth the diagnosis.
func (w *DirectWriter) runExpectingOne(ctx context.Context, sql string, args []any, verb string) error {
	outcome := func(n int64) error {
		switch {
		case n == 0:
			return domain.NotFoundError(w.name, w.schema.IDColumn(), "")
		case n > 1:
			return fmt.Errorf("db: %s on %q matched %d rows — the criteria is wider than one row; "+
				"the write was abandoned", verb, w.schema.Table(), n)
		}
		return nil
	}

	if w.tx != nil {
		n, err := core.ExecCount(w.tx, ctx, sql, args...)
		if err != nil {
			return w.mapErr(err)
		}
		return outcome(n)
	}

	tx, err := w.beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n, err := tx.ExecCount(ctx, sql, args...)
	if err != nil {
		return w.mapErr(err)
	}
	if err := outcome(n); err != nil {
		return err // the deferred rollback undoes what the statement touched
	}
	return w.mapErr(tx.Commit(ctx))
}

// run executes one statement and reports the affected-row count, on the
// caller's transaction when the repository was bound with InTx and on a
// framework-owned one otherwise. Opening a transaction for a single statement is
// what makes the count available uniformly across engines — and what lets
// expectOne refuse a too-wide criteria with nothing committed.
func (w *DirectWriter) run(ctx context.Context, sql string, args []any) (int64, error) {
	if w.tx != nil {
		n, err := core.ExecCount(w.tx, ctx, sql, args...)
		return n, w.mapErr(err)
	}
	tx, err := w.beginner.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n, err := tx.ExecCount(ctx, sql, args...)
	if err != nil {
		return 0, w.mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, w.mapErr(err)
	}
	return n, nil
}

// mapErr translates a unique-constraint violation into the typed notification
// the Constraints map declares for it, exactly as the entity write path does.
func (w *DirectWriter) mapErr(err error) error {
	if err == nil || w.eng == nil {
		return err
	}
	return TranslateUniqueViolation(w.eng.Dialect(), err, w.name, w.constraints)
}
