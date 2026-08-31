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

// stampMarker is the type of Stamp, StampNull and StampEmpty. It is a distinct,
// unexported type, so it can never be confused with a value a caller meant to
// bind and no other package can forge one. The op it carries is the only thing
// that tells the three apart — a caller still supplies no value, only intent.
type stampMarker struct{ op domain.StampOp }

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
var Stamp = stampMarker{domain.StampFill}

// StampNull is the marker that asks the framework to write SQL NULL into a
// stamped column — the Direct half of domain.Managed.StampNull, through the same
// channel Stamp uses:
//
//	_, err := jobs.Update(ctx, write.Values{
//	    "Status":    "pending",
//	    "StartedAt": write.StampNull,   // the job is no longer running
//	}, criteria.Where(criteria.Eq("Status", "running")))
//
// The field has to be able to hold an absence: a stamped time always can, a
// stamped counter only when it is declared *int64. Asking it of a plain int64 is
// refused by the write, which names StampEmpty as the verb that zeroes it.
//
// Like every stamp verb, it is a REQUEST: a column left out of Values is left out
// of the statement, so nothing is cleared by omission.
var StampNull = stampMarker{domain.StampToNull}

// StampEmpty is the marker that asks the framework to write the declared type's
// ZERO — 0 for a stamped counter, the zero instant for a stamped time:
//
//	_, err := jobs.Update(ctx, write.Values{
//	    "Attempts": write.StampEmpty,   // the retry budget is refilled
//	}, criteria.Where(criteria.Eq("Status", "pending")))
//
// It is not a spelling of StampNull. A zero is a VALUE, so it reaches a column
// declared NOT NULL — where an absence cannot go — and on a counter it is the
// difference between "counted nothing" and "has no count".
var StampEmpty = stampMarker{domain.StampToEmpty}

// writeScope says which HALF of an upsert a value binds on. Its zero value is
// the ordinary one — bound on both — so a Values slot with no wrapper keeps
// meaning exactly what it has always meant, on every verb.
type writeScope int

const (
	scopeBoth writeScope = iota
	scopeInsert
	scopeUpdate
)

// verb names the wrapper a scope came from, for the diagnostics that have to say
// which one the caller wrote.
func (s writeScope) verb() string {
	if s == scopeUpdate {
		return "write.OnUpdate"
	}
	return "write.OnInsert"
}

// scopedValue wraps a value a caller wants bound on ONE half of an upsert: the
// row it creates, or the row it finds. What it carries may be an ordinary value
// or a stamp marker — the scope says WHERE it applies, the value itself still
// says what it is.
type scopedValue struct {
	scope writeScope
	v     any
}

// OnInsert marks a Values slot as insert-only: the value binds on the INSERT and
// the column is absent from the conflict clause, so an existing row keeps
// whatever it had.
//
//	w.Upsert(ctx, write.Values{
//	    "Identity":        id,
//	    "WindowStartedAt": write.OnInsert(write.Stamp),  // opened once, never reopened
//	    "TotalCount":      write.Stamp,                  // counted on every write
//	}, write.OnConflict("Identity"))
//
// It exists because an upsert has more things a column can be than a plain write
// does: bound on both paths (an ordinary value), filled by the framework
// (write.Stamp), or established once and never revised — a window's start, a
// first-seen instant, a creation-time attribution. Without it that last kind
// would be overwritten on every conflict by the value the caller happened to
// compute this time.
//
// IT TAKES A STAMP VERB TOO, and that pairing is the only way to date a row's
// CREATION with the operation's own instant: the caller cannot compute that
// instant (under relational.clock: db it is read from the very transaction the
// statement runs in), and a bare write.Stamp would refresh the column on every
// conflict. write.OnInsert(write.StampNull) and write.OnInsert(write.StampEmpty)
// read the same way — the absence or the zero is stated on the row being
// created, and the row that was already there is left alone.
//
// Outside Upsert it is refused: on a plain Insert every column is insert-only
// already, and on an Update there is no insert path for it to mean anything.
func OnInsert(v any) any { return scopedValue{scope: scopeInsert, v: v} }

// OnUpdate is the mirror of OnInsert: the value binds ONLY when the row was
// already there. The column is left out of the proposed row entirely, so on the
// creating path it takes whatever the table declares for it.
//
//	w.Upsert(ctx, write.Values{
//	    "Identity":   id,
//	    "LastAt":     write.Stamp,                  // every arrival is dated
//	    "RepeatedAt": write.OnUpdate(write.Stamp),  // only a SECOND one is
//	}, write.OnConflict("Identity"))
//
// It is what a column describing the COLLISION needs — when this repeated, who
// overwrote it, a reason that only exists because something was already there.
// Bound as an ordinary value, such a column would be filled on a first arrival
// that collided with nothing.
//
// Like OnInsert it takes a stamp verb, with the same meaning it carries
// anywhere else. It is the one place a stamped column is written from an
// argument of its own rather than read back out of the proposed row — that row
// does not carry the column at all.
//
// THE INSERT HALF IS THE CALLER'S TO THINK ABOUT: a column left out of it has to
// tolerate absence. NOT NULL with no DEFAULT makes the creating path fail, and
// that is the table's contract to settle, not the statement's.
//
// Outside Upsert it is refused, exactly as OnInsert is: an Update writes every
// value it is handed, and an Insert has no conflict path.
func OnUpdate(v any) any { return scopedValue{scope: scopeUpdate, v: v} }

// unwrapScoped separates the scope from the value. A bare value is scopeBoth,
// which is what every write outside an upsert has and what an upsert slot means
// when the caller wrapped nothing.
//
// Nesting is refused rather than resolved: a value binds on one half or on both,
// and two wrappers around it say nothing a single one does not already say.
func unwrapScoped(table, goField string, val any) (writeScope, any, error) {
	s, wrapped := val.(scopedValue)
	if !wrapped {
		return scopeBoth, val, nil
	}
	if inner, nested := s.v.(scopedValue); nested {
		return scopeBoth, nil, fmt.Errorf(
			"db: %q on table %q wraps %s in %s — a value binds on the insert half, on the conflict half, or "+
				"on both; nesting the two says nothing a single wrapper does not",
			goField, table, inner.scope.verb(), s.scope.verb())
	}
	return s.scope, s.v, nil
}

// UpsertOption configures one Upsert. Options carry what the STATEMENT needs and
// the schema cannot know: which key decides "already there", and what an upsert
// does to a row that is archived.
type UpsertOption func(*upsertConfig)

type upsertConfig struct {
	conflictGo   []string
	archive      archivePolicy
	archiveGiven bool
}

// archivePolicy is what an Upsert does to the archive column of a row it finds.
type archivePolicy int

const (
	archiveKeep archivePolicy = iota
	archiveUnarchive
)

// OnConflict names the fields whose values decide whether the row already
// exists — by GO FIELD NAME, like every other name above infra. It is declared
// per call rather than on the schema because one table legitimately has more
// than one way to be conflicted on, and because four of the five engines take
// the key on the statement itself.
//
// MySQL IS THE EXCEPTION, and it cannot be emulated: ON DUPLICATE KEY UPDATE
// fires on ANY unique key the row violates, not on the one named here. On a
// table with a single unique key the behavior is identical everywhere; with more
// than one, MySQL may resolve a conflict the other engines would have let fail.
// Declare one unique key per upserted table and the difference disappears.
func OnConflict(goFields ...string) UpsertOption {
	return func(c *upsertConfig) { c.conflictGo = goFields }
}

// UnarchiveOnConflict brings an archived row back to life when the upsert lands
// on one: deleted_at is set to NULL in the conflict clause.
//
// Note what it does NOT do: the row's other columns are updated by the same
// rules as any conflict, so counters CONTINUE from where they were rather than
// restarting. An upsert that must start over needs the old row deleted, not
// archived.
func UnarchiveOnConflict() UpsertOption {
	return func(c *upsertConfig) { c.archive, c.archiveGiven = archiveUnarchive, true }
}

// KeepArchiveStateOnConflict leaves the archive column exactly as it is —
// whatever it is. Most conflicts land on live rows, and those stay live; a
// conflict that lands on an ARCHIVED row updates it while it stays archived, and
// therefore stays invisible to every read.
//
// That second half is the reason this must be declared rather than defaulted. An
// upsert is the one write that cannot be archive-gated: INSERT ... ON CONFLICT
// has no WHERE for its conflict target, so an archived row still occupies the
// unique key and still absorbs the write. Choosing it deliberately is
// reasonable — a forensic counter that must keep counting after the subject was
// retired — but it is not a choice the framework may make on someone's behalf.
func KeepArchiveStateOnConflict() UpsertOption {
	return func(c *upsertConfig) { c.archive, c.archiveGiven = archiveKeep, true }
}

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
func resolveValues(schema *TableSchema, v Values) (domain.Fields, stampPlan, error) {
	if len(v) == 0 {
		return nil, stampPlan{}, fmt.Errorf("db: a Direct write needs at least one value — an empty Values binds no column")
	}
	out := make(domain.Fields, len(v))
	var asked []domain.StampRequest
	for goField, val := range v {
		if why, managed := managedByVerb[goField]; managed {
			return nil, stampPlan{}, fmt.Errorf("db: %q cannot be written directly — %s", goField, why)
		}
		if sv, scoped := val.(scopedValue); scoped {
			return nil, stampPlan{}, fmt.Errorf(
				"db: %q on table %q is wrapped in %s — an insert half and a conflict half are an UPSERT's, and "+
					"no other verb has two of them. Bind the value plainly here",
				goField, schema.Table(), sv.scope.verb())
		}
		if m, marked := val.(stampMarker); marked {
			// The marker never becomes a bound value: it names a column and the
			// verb, which the schema then validates against its declaration.
			asked = append(asked, domain.StampRequest{Field: goField, Op: m.op})
			continue
		}
		rf, ok := schema.Resolve(goField)
		if !ok {
			return nil, stampPlan{}, fmt.Errorf("db: unknown field %q on table %q — declare it with TableSchema.Field(...)",
				goField, schema.Table())
		}
		if schema.IsStampedField(goField) {
			return nil, stampPlan{}, fmt.Errorf(
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
	claimed, err := schema.StampRequestColumns(asked)
	if err != nil {
		return nil, stampPlan{}, err
	}
	if len(out) == 0 {
		return nil, stampPlan{}, fmt.Errorf(
			"db: a Direct write on %q asked only for stamps — a write has to change something the caller "+
				"decided; a stamp records WHEN that happened, it is not the change itself",
			schema.Table())
	}
	var plan stampPlan
	plan.splitStamps(claimed)
	return out, plan, nil
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
	fields, plan, err := resolveValues(w.schema, v)
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
		plan.nowCols = append(w.schema.InsertNowColumns(), plan.nowCols...)
		sql, args := buildInsertWithCounters(tx.Dialect(), w.schema.Table(), w.schema.IDColumn(), id,
			fields, plan, now, "")
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
		fields, plan, err := resolveValues(w.schema, v)
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
		plan.nowCols = append(w.schema.UpdateNowColumns(), plan.nowCols...)
		return buildUpdatePlan(tx.Dialect(), schemaTarget(w.schema), pred, fields, plan, now, "", 0)
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

// Upsert writes one row, keyed on the fields OnConflict names rather than on the
// identity: it inserts when nothing matches that key, and updates the row that
// does — in one statement, so two callers racing on the same key cannot both
// decide the row is missing.
//
//	w.Upsert(ctx, write.Values{
//	    "Identity":        id,
//	    "IdentityKind":    kind,
//	    "Outcome":         "FAILURE",
//	    "TotalCount":      write.Stamp,                 // += 1 on both paths
//	    "WindowStartedAt": write.OnInsert(write.Stamp),  // dated once, never re-dated
//	    "LastAt":          write.Stamp,                  // the operation's instant
//	    "LastIP":          ip,                           // overwritten on conflict
//	}, write.OnConflict("Identity", "IdentityKind", "Outcome"),
//	   write.KeepArchiveStateOnConflict())
//
// Each Values slot says what happens on a conflict, and the cases are the kinds
// of column an upsert has: an ordinary value is overwritten; write.Stamp is
// filled by the framework (the operation's instant for a stamped TIME column,
// `col = col + 1` for a stamped COUNTER, computed server-side under the row's
// lock so two racing increments cannot collapse into one); OnInsert is
// established on creation and never revised; OnUpdate is stated only when the
// row was already there, and is absent from the row being created; and the
// conflict key itself is insert-only by definition — it is the thing that
// matched.
//
// The two wrappers take a stamp verb as readily as a value, which is how a
// column gets the framework's OWN instant on exactly one of the two paths —
// write.OnInsert(write.Stamp) dates a creation without ever re-dating it,
// write.OnUpdate(write.Stamp) dates only the collision.
//
// It returns only an error. A row count would have to mean the same on every
// backend and it does not: MySQL reports 2 for a conflicting upsert and 1 for an
// inserting one, while the others report 1 for both. Reporting which path ran
// would need a RETURNING/OUTPUT clause MySQL has no equivalent for, so the
// framework declines to invent an answer.
//
// A schema declaring DeletedAt MUST also declare what an upsert does to the
// archive column — UnarchiveOnConflict or KeepArchiveStateOnConflict — because
// this is the one write that cannot be archive-gated (see those two).
func (w *DirectWriter) Upsert(ctx context.Context, v Values, opts ...UpsertOption) error {
	var cfg upsertConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	plan, err := w.upsertPlan(v, cfg)
	if err != nil {
		return err
	}
	// The identity is minted like every other Direct insert; on the conflict path
	// it is simply not used — the row that matched keeps the id it already has,
	// which is why Upsert returns no id.
	id, err := newWriteID()
	if err != nil {
		return err
	}
	_, err = w.run(ctx, func(tx Tx) (string, []any, error) {
		now, err := w.now(ctx, tx)
		if err != nil {
			return "", nil, err
		}
		return w.renderUpsert(tx.Dialect(), plan, id, now)
	})
	return err
}

// UpdateAll binds the given values on EVERY row of the table, archived ones
// included. It is the deliberate sweep the empty-predicate refusal points at:
// the verb, not a filter that happened to come out empty, is what says so.
func (w *DirectWriter) UpdateAll(ctx context.Context, v Values) (int64, error) {
	fields, plan, err := resolveValues(w.schema, v)
	if err != nil {
		return 0, err
	}
	return w.run(ctx, func(tx Tx) (string, []any, error) {
		now, err := w.now(ctx, tx)
		if err != nil {
			return "", nil, err
		}
		d := tx.Dialect()
		plan.nowCols = append(w.schema.UpdateNowColumns(), plan.nowCols...)
		sets, args := buildSetWithCounters(d, fields, plan, now, "")
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

// upsertPlan is a Values map resolved for one upsert: which columns bind what,
// and what each one does when the row already exists. Every refusal is raised
// while building it — before a transaction is opened — so a rejected call costs
// nothing.
//
// It exists apart from resolveValues because an upsert reads the same map with a
// wider vocabulary: OnInsert and OnUpdate mean nothing on the other verbs, which
// have a single half, and there a stamped column rides the managed-column
// channel instead of a conflict clause.
type upsertPlan struct {
	bound      domain.Fields // ordinary values — written on both halves
	insertOnly domain.Fields // established on creation, never revised
	updateOnly domain.Fields // stated only when the row was already there
	// The stamp requests, one plan per half they reach. They are split HERE
	// rather than tagged column by column because that is what keeps renderUpsert
	// honest: a bucket goes into the proposed row, into the conflict clause, or
	// into both, and neither half can forget a column the other one states.
	stamps       stampPlan // asked on both halves
	insertStamps stampPlan // OnInsert(write.Stamp…) — the proposed row only
	updateStamps stampPlan // OnUpdate(write.Stamp…) — the conflict clause only
	keyCols      []string  // the conflict target, in declaration order
	keyed        map[string]bool
	unarchive    bool
	sdCol        string
}

func (w *DirectWriter) upsertPlan(v Values, cfg upsertConfig) (upsertPlan, error) {
	table := w.schema.Table()
	if len(cfg.conflictGo) == 0 {
		return upsertPlan{}, fmt.Errorf(
			"db: an Upsert on %q needs write.OnConflict(...) — the key that decides whether the row already "+
				"exists is the statement's whole premise, and the framework will not guess it from the schema",
			table)
	}
	sdCol, hasDeletedAt := w.schema.DeletedAtColumn()
	if hasDeletedAt && !cfg.archiveGiven {
		return upsertPlan{}, fmt.Errorf(
			"db: an Upsert on %q must declare what it does to an ARCHIVED row — write.UnarchiveOnConflict() "+
				"or write.KeepArchiveStateOnConflict(). Every other write verb is gated on deleted_at IS NULL, "+
				"but an upsert cannot be: its conflict target takes no WHERE, so an archived row still holds "+
				"the unique key and still absorbs the write. Unarchiving and updating-while-invisible are both "+
				"defensible; picking one on your behalf is not",
			table)
	}
	if len(v) == 0 {
		return upsertPlan{}, fmt.Errorf("db: an Upsert on %q needs at least one value", table)
	}

	plan := upsertPlan{
		bound:      domain.Fields{},
		insertOnly: domain.Fields{},
		updateOnly: domain.Fields{},
		unarchive:  cfg.archive == archiveUnarchive,
		sdCol:      sdCol,
	}

	// The conflict key resolves through the schema like any other name. Its
	// columns are insert-only by definition — they are what matched — so they are
	// tracked here and skipped when the conflict clause is assembled.
	isKey := make(map[string]bool, len(cfg.conflictGo))
	keyGo := make(map[string]bool, len(cfg.conflictGo))
	for _, g := range cfg.conflictGo {
		rf, ok := w.schema.Resolve(g)
		if !ok {
			return upsertPlan{}, fmt.Errorf(
				"db: Upsert on %q — unknown conflict field %q; declare it with TableSchema.Field(...)", table, g)
		}
		if _, present := v[g]; !present {
			return upsertPlan{}, fmt.Errorf(
				"db: Upsert on %q — the conflict field %q carries no value; the key a row is matched on has to "+
					"be part of the row being written", table, g)
		}
		isKey[rf.Column] = true
		keyGo[g] = true
		plan.keyCols = append(plan.keyCols, rf.Column)
	}

	var asked, askedOnInsert, askedOnUpdate []domain.StampRequest
	for goField, raw := range v {
		if why, managed := managedByVerb[goField]; managed {
			return upsertPlan{}, fmt.Errorf("db: %q cannot be written directly — %s", goField, why)
		}
		scope, val, err := unwrapScoped(table, goField, raw)
		if err != nil {
			return upsertPlan{}, err
		}
		// The key is insert-only by definition — it is what MATCHED — so scoping
		// it says either nothing or something the statement cannot honor: the
		// MERGE dialects refuse an assignment to a join column outright.
		if scope != scopeBoth && keyGo[goField] {
			return upsertPlan{}, fmt.Errorf(
				"db: Upsert on %q — the conflict field %q cannot be wrapped in %s: the key is what the row was "+
					"matched on, so it is written once and never revised. Bind it plainly",
				table, goField, scope.verb())
		}
		if m, marked := val.(stampMarker); marked {
			req := domain.StampRequest{Field: goField, Op: m.op}
			switch scope {
			case scopeInsert:
				askedOnInsert = append(askedOnInsert, req)
			case scopeUpdate:
				askedOnUpdate = append(askedOnUpdate, req)
			default:
				asked = append(asked, req)
			}
			continue
		}
		rf, ok := w.schema.Resolve(goField)
		if !ok {
			return upsertPlan{}, fmt.Errorf(
				"db: unknown field %q on table %q — declare it with TableSchema.Field(...)", goField, table)
		}
		if w.schema.IsStampedField(goField) {
			return upsertPlan{}, fmt.Errorf(
				"db: %q on table %q is a stamped field — its value is the framework's, never the caller's. "+
					"Pass write.Stamp to ask for it: Values{%q: write.Stamp}", goField, table, goField)
		}
		switch scope {
		case scopeInsert:
			plan.insertOnly[rf.Column] = val
		case scopeUpdate:
			plan.updateOnly[rf.Column] = val
		default:
			plan.bound[rf.Column] = val
		}
	}

	// Each half's requests are validated by the SAME schema surface — a marker on
	// a plain field is the same mistake whichever half it was scoped to.
	for _, ask := range []struct {
		reqs []domain.StampRequest
		into *stampPlan
	}{
		{asked, &plan.stamps},
		{askedOnInsert, &plan.insertStamps},
		{askedOnUpdate, &plan.updateStamps},
	} {
		claimed, err := w.schema.StampRequestColumns(ask.reqs)
		if err != nil {
			return upsertPlan{}, err
		}
		ask.into.splitStamps(claimed)
	}
	// The key columns stay in `bound` — they are written on the INSERT like any
	// other value. keyed records them so the CONFLICT clause can skip them:
	// assigning a column the row was MATCHED on is a no-op at best, and on the
	// MERGE dialects assigning a join key is an error.
	plan.keyed = isKey
	return plan, nil
}

// renderUpsert turns the resolved plan into the statement and its arguments.
// It runs inside the transaction because a stamped time column binds the
// operation's instant, and under relational.clock: db that instant is read from
// the very transaction the statement will run in.
//
// The two halves are assembled in this order for a reason: every argument the
// proposed row binds comes first, and the conflict-only ones follow in the order
// their assignments appear — which is exactly how the dialect numbers the
// placeholders it renders for them.
func (w *DirectWriter) renderUpsert(d Dialect, plan upsertPlan, id string, now time.Time) (string, []any, error) {
	boundKeys := SortedKeys(plan.bound)
	insertKeys := SortedKeys(plan.insertOnly)
	updateKeys := SortedKeys(plan.updateOnly)

	cols := make([]string, 0, 1+len(boundKeys)+len(insertKeys))
	args := make([]any, 0, cap(cols))

	cols = append(cols, w.schema.IDColumn())
	args = append(args, d.EncodeArg(domain.NewID(id)))
	for _, c := range boundKeys {
		cols = append(cols, c)
		args = append(args, d.EncodeArg(plan.bound[c]))
	}
	for _, c := range insertKeys {
		cols = append(cols, c)
		args = append(args, d.EncodeArg(plan.insertOnly[c]))
	}
	// The stamps the proposed row states: the ones asked on both halves and the
	// insert-only ones alike. Here they are the same columns bound to the same
	// values — only the conflict clause below tells the two apart.
	cols, args = appendProposedStamps(d, cols, args, plan.stamps, now)
	cols, args = appendProposedStamps(d, cols, args, plan.insertStamps, now)
	// The managed timestamps ride the INSERT like they do on every other verb.
	for _, c := range w.schema.InsertNowColumns() {
		cols = append(cols, c)
		args = append(args, d.EncodeArg(now))
	}

	sets := make([]UpsertSet, 0, len(cols)+len(updateKeys))
	for _, c := range boundKeys {
		if plan.keyed[c] {
			continue
		}
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetNew})
	}
	sets = appendProposedStampSets(sets, plan.stamps)
	// From here on the assignments bind arguments of their own: their columns are
	// absent from the proposed row, so there is nothing there to read them from.
	for _, c := range updateKeys {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetArg})
		args = append(args, d.EncodeArg(plan.updateOnly[c]))
	}
	sets, args = appendConflictStampSets(d, sets, args, plan.updateStamps, now)
	// updated_at is already in the INSERT list, bound to this same instant, so
	// the conflict path takes it from the proposed row rather than binding a
	// second argument — one value, one placeholder, no ordering to get wrong.
	// created_at is deliberately absent: a row's creation is not revised.
	for _, c := range w.schema.UpdateNowColumns() {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetNew})
	}
	if plan.unarchive {
		sets = append(sets, UpsertSet{Col: plan.sdCol, Mode: core.UpsertSetExpr, Expr: "NULL"})
	}
	return d.BuildUpsert(w.schema.Table(), cols, plan.keyCols, sets), args, nil
}

// appendProposedStamps states a stamp plan in the row the upsert PROPOSES. Every
// bucket becomes a column of the INSERT with its value bound — including the
// absence and the zero, which are literals in a conflict clause but cannot be
// here: the insert half pairs one column with one argument and has no room for
// an expression, and a NOT NULL counter may declare no DEFAULT at all.
func appendProposedStamps(d Dialect, cols []string, args []any, sp stampPlan, now time.Time) ([]string, []any) {
	add := func(columns []string, val any) {
		for _, c := range columns {
			cols = append(cols, c)
			args = append(args, val)
		}
	}
	add(sp.requestedTimes, d.EncodeArg(now))
	// A fresh row has counted one thing.
	add(sp.counters, int64(1))
	add(sp.zeroTimes, d.EncodeArg(time.Time{}))
	add(sp.nullCols, nil)
	add(sp.zeroCounters, int64(0))
	return cols, args
}

// appendProposedStampSets is the conflict half of the stamps the proposed row
// already states: a filled or zeroed time is taken from there, a counter
// increments the EXISTING row, and the two clearing verbs are literals — nothing
// is bound on either side of them, so there is no argument order to get wrong.
func appendProposedStampSets(sets []UpsertSet, sp stampPlan) []UpsertSet {
	for _, c := range sp.requestedTimes {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetNew})
	}
	for _, c := range sp.counters {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetBump})
	}
	for _, c := range sp.zeroTimes {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetNew})
	}
	for _, c := range sp.nullCols {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetExpr, Expr: "NULL"})
	}
	for _, c := range sp.zeroCounters {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetExpr, Expr: "0"})
	}
	return sets
}

// appendConflictStampSets is the same vocabulary for a stamp asked ONLY on the
// conflict path. The clearing verbs stay literals and a counter still increments
// the existing row; the times are what differs — with no proposed row to be read
// back from, each binds an argument of its own.
func appendConflictStampSets(d Dialect, sets []UpsertSet, args []any, sp stampPlan, now time.Time) ([]UpsertSet, []any) {
	for _, c := range sp.requestedTimes {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetArg})
		args = append(args, d.EncodeArg(now))
	}
	for _, c := range sp.counters {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetBump})
	}
	for _, c := range sp.zeroTimes {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetArg})
		args = append(args, d.EncodeArg(time.Time{}))
	}
	for _, c := range sp.nullCols {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetExpr, Expr: "NULL"})
	}
	for _, c := range sp.zeroCounters {
		sets = append(sets, UpsertSet{Col: c, Mode: core.UpsertSetExpr, Expr: "0"})
	}
	return sets, args
}
