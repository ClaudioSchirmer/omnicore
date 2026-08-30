package write

import (
	"context"
	"fmt"
	"strings"
	"time"

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
//
// A STAMPED column (TableSchema.StampedTimeField) sits between the two: the
// caller may ask for it but not dictate it, so its slot takes the Stamp marker
// below and refuses a real value.
type Values map[string]any

// stampMarker is the type of Stamp. It is a distinct, unexported type carrying
// no data, so it can never be confused with a value a caller meant to bind and
// so no other package can forge one.
type stampMarker struct{}

// Stamp is the marker a Direct write puts in a Values slot to ask the framework
// to FILL that column. It is the Direct half of TableSchema.StampedTimeField —
// what domain.Managed.Stamp is on the entity side, expressed through the only
// channel this path has: an entity write takes an entity and can accumulate the
// request on it, a Direct write takes a map, so the request rides in the map.
//
// HOW TO USE IT. Declare the column on the Direct schema, then mark its slot on
// the write that decides the moment:
//
//	func JobTable() *core.TableSchema {
//	    return core.NewDirectSchema[JobRow]("job_queue").
//	        ID("id").
//	        Field("Status", "status").
//	        StampedTimeField("StartedAt", "started_at").   // *time.Time, nullable column
//	        UpdatedAt("updated_at")
//	}
//
//	// the WHEN is the call site's; the value is still not its own
//	_, err := jobs.Update(ctx, write.Values{
//	    "Status":    "running",
//	    "StartedAt": write.Stamp,
//	}, criteria.Where(criteria.Eq("Status", "pending")))
//
// It works on every write verb that binds columns — Insert, Update, UpdateOne
// and UpdateAll — and the slot is keyed by GO FIELD NAME, like every other
// Values key. One statement over a hundred rows dates all of them with the SAME
// instant, which is a reason to prefer it over a time.Now() the caller computes.
//
// The caller owns the WHEN, the framework owns the value: the write operation's
// own authoritative instant, read from the clock the service declared
// (relational.clock). A column left out of Values is left out of the statement,
// so an already-stamped row keeps what it had.
//
// What it may mark is what the schema declared with StampedTimeField — a plain
// field is refused, since its value comes from Values and stamping it would mean
// nothing. Binding a time.Time into a stamped slot is refused too: the point of
// the field is that its value is not the caller's to choose.
//
// It carries no type of its own on purpose. What the marker FILLS is the
// schema's declaration to make — a stamped time column today, whatever the
// family grows into later — so the call site never has to change when it does.
var Stamp = stampMarker{}

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
func resolveValues(schema *TableSchema, v Values) (domain.Fields, []string, error) {
	if len(v) == 0 {
		return nil, nil, fmt.Errorf("db: a Direct write needs at least one value — an empty Values binds no column")
	}
	out := make(domain.Fields, len(v))
	var asked []string
	for goField, val := range v {
		if why, managed := managedByVerb[goField]; managed {
			return nil, nil, fmt.Errorf("db: %q cannot be written directly — %s", goField, why)
		}
		if _, marked := val.(stampMarker); marked {
			// The marker never becomes a bound value: it names a column for the
			// stamp list, which StampColumns then validates against the schema.
			asked = append(asked, goField)
			continue
		}
		rf, ok := schema.Resolve(goField)
		if !ok {
			return nil, nil, fmt.Errorf("db: unknown field %q on table %q — declare it with TableSchema.Field(...)",
				goField, schema.Table())
		}
		if schema.IsStampedField(goField) {
			return nil, nil, fmt.Errorf(
				"db: %q on table %q is a stamped field — its value is the framework's (the write operation's "+
					"instant), never the caller's. Pass write.Stamp to ask for it: Values{%q: write.Stamp}",
				goField, schema.Table(), goField)
		}
		out[rf.Column] = val
	}
	// Validate what was ASKED before judging the shape of the write: a marker on
	// a plain field is a mistake about that field, and saying so beats telling
	// the caller their write has no substance when the substance is the very key
	// they got wrong.
	stamps, err := schema.StampColumns(asked)
	if err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf(
			"db: a Direct write on %q asked only for stamps — a write has to change something the caller "+
				"decided; a stamp records WHEN that happened, it is not the change itself",
			schema.Table())
	}
	return out, stamps, nil
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
	// clock is the declared instant source (relational.clock), recovered from
	// the engine at construction exactly as the WriteBeginner is. A Direct write
	// stamps the same managed columns an entity write does, so it reads the same
	// clock; an engine that does not carry one leaves ClockApp, the zero value.
	clock core.ClockMode
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
	w := &DirectWriter{eng: eng, beginner: b, schema: schema, name: name}
	if c, ok := any(eng).(interface{ ClockMode() core.ClockMode }); ok {
		w.clock = c.ClockMode()
	}
	return w
}

// now mints this write's authoritative instant, read through the transaction the
// statement will run in — the caller's under InTx, the framework-owned one
// otherwise. Same contract as the entity path: one reading per statement, bound
// as an ordinary argument.
func (w *DirectWriter) now(ctx context.Context, tx Tx) (time.Time, error) {
	return writeNow(ctx, tx, w.clock)
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
	fields, stamps, err := resolveValues(w.schema, v)
	if err != nil {
		return domain.ID{}, err
	}
	id, err := newWriteID()
	if err != nil {
		return domain.ID{}, err
	}
	if _, err := w.run(ctx, func(tx Tx) (string, []any, error) {
		now, err := w.now(ctx, tx)
		if err != nil {
			return "", nil, err
		}
		sql, args := buildInsert(tx.Dialect(), w.schema.Table(), w.schema.IDColumn(), id, fields,
			append(w.schema.InsertNowColumns(), stamps...), now, "")
		return sql, args, nil
	}); err != nil {
		return domain.ID{}, err
	}
	return domain.NewID(id), nil
}

// Update binds the given values on every row matching the criteria and returns
// how many were affected. The Query's archived scope gates the statement like it
// gates a read: by default only active rows are touched.
func (w *DirectWriter) Update(ctx context.Context, v Values, q *criteria.Query) (int64, error) {
	return w.run(ctx, w.updateStmt(ctx, v, q))
}

// updateStmt renders the UPDATE both Update and UpdateOne execute. They differ
// only in what they do with the affected-row count, never in what they emit.
func (w *DirectWriter) updateStmt(ctx context.Context, v Values, q *criteria.Query) func(Tx) (string, []any, error) {
	return func(tx Tx) (string, []any, error) {
		fields, stamps, err := resolveValues(w.schema, v)
		if err != nil {
			return "", nil, err
		}
		pred, err := w.predicate(q)
		if err != nil {
			return "", nil, err
		}
		now, err := w.now(ctx, tx)
		if err != nil {
			return "", nil, err
		}
		return buildUpdate(tx.Dialect(), schemaTarget(w.schema), pred, fields,
			append(w.schema.UpdateNowColumns(), stamps...), now, "", 0)
	}
}

// UpdateOne is Update for a criteria the caller believes names exactly one row.
// Zero rows is a typed RecordNotFound; more than one is refused, and because the
// statement runs in a transaction the rows it already touched are rolled back.
func (w *DirectWriter) UpdateOne(ctx context.Context, v Values, q *criteria.Query) error {
	return w.runExpectingOne(ctx, w.updateStmt(ctx, v, q), "UpdateOne")
}

// Delete removes every row matching the criteria — one statement against one
// table, no cascade — and returns how many were removed.
func (w *DirectWriter) Delete(ctx context.Context, q *criteria.Query) (int64, error) {
	return w.run(ctx, w.deleteStmt(q))
}

// deleteStmt renders the DELETE both Delete and DeleteOne execute.
func (w *DirectWriter) deleteStmt(q *criteria.Query) func(Tx) (string, []any, error) {
	return func(tx Tx) (string, []any, error) {
		pred, err := w.predicate(q)
		if err != nil {
			return "", nil, err
		}
		return deleteSQL(tx.Dialect(), schemaTarget(w.schema), pred)
	}
}

// DeleteOne is Delete for a criteria the caller believes names exactly one row.
func (w *DirectWriter) DeleteOne(ctx context.Context, q *criteria.Query) error {
	return w.runExpectingOne(ctx, w.deleteStmt(q), "DeleteOne")
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
	fields, stamps, err := resolveValues(w.schema, v)
	if err != nil {
		return 0, err
	}
	return w.run(ctx, func(tx Tx) (string, []any, error) {
		now, err := w.now(ctx, tx)
		if err != nil {
			return "", nil, err
		}
		d := tx.Dialect()
		sets, args := buildSet(d, fields, append(w.schema.UpdateNowColumns(), stamps...), now, "")
		return fmt.Sprintf("UPDATE %s SET %s", d.QuoteIdent(w.schema.Table()), strings.Join(sets, ", ")), args, nil
	})
}

// DeleteAll empties the table — every row, archived ones included.
func (w *DirectWriter) DeleteAll(ctx context.Context) (int64, error) {
	return w.run(ctx, func(tx Tx) (string, []any, error) {
		return "DELETE FROM " + tx.Dialect().QuoteIdent(w.schema.Table()), nil, nil
	})
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
	return w.run(ctx, func(tx Tx) (string, []any, error) {
		now, err := w.now(ctx, tx)
		if err != nil {
			return "", nil, err
		}
		var stamp any
		if archive {
			stamp = now
		}
		return buildUpdate(tx.Dialect(), schemaTarget(w.schema),
			gated(q.Condition(), scope, w.schema), domain.Fields{sdCol: stamp},
			w.schema.UpdateNowColumns(), now, "", 0)
	})
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
func (w *DirectWriter) runExpectingOne(ctx context.Context, build func(Tx) (string, []any, error), verb string) error {
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
		sql, args, err := build(w.tx)
		if err != nil {
			return err
		}
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
	sql, args, err := build(tx)
	if err != nil {
		return err
	}
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
//
// The statement arrives as a BUILDER rather than as finished SQL because a
// managed timestamp may have to be read from the database itself
// (relational.clock: db): the transaction has to exist before the statement can
// be rendered, so the builder runs with the live transaction in hand. It also
// keeps the resolver/dialect the statement is compiled against the one belonging
// to the transaction that will execute it.
func (w *DirectWriter) run(ctx context.Context, build func(Tx) (string, []any, error)) (int64, error) {
	if w.tx != nil {
		sql, args, err := build(w.tx)
		if err != nil {
			return 0, err
		}
		n, err := core.ExecCount(w.tx, ctx, sql, args...)
		return n, w.mapErr(err)
	}
	tx, err := w.beginner.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sql, args, err := build(tx)
	if err != nil {
		return 0, err
	}
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
