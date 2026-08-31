package write

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// idGoField is the Go field name the framework's identity resolves under — the
// Entity contract fixes it and criteria.ByID uses the same spelling. Declared
// here like the read backings declare it, rather than exported from core: it is
// a constant of the contract, not a knob.
const idGoField = "ID"

// newWriteID mints the framework-authoritative id for a new row: a UUID v7
// (time-ordered, so a clustered ID stays local) generated in Go on EVERY
// backend. No dialect generates ids anymore — Postgres' gen_random_uuid()
// DEFAULT is simply overridden and MySQL never had one — so the INSERT is
// byte-identical across dialects and carries no RETURNING.
func newWriteID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("db: uuid v7: %w", err)
	}
	return id.String(), nil
}

// writeNow mints the single authoritative timestamp of one write operation.
// Managed columns (created_at/updated_at and the DeletedAt stamp) are bound as
// ordinary arguments — the same move the ids made with the Go-minted UUID v7:
// no dialect NOW() expression in the data DML, so every statement of one
// operation (root, children, siblings, base cascade) carries the SAME instant,
// known in Go before COMMIT (the outbox payload can therefore carry it too).
// UTC, truncated to microseconds — the precision every supported backend stores
// (timestamptz / DATETIME(6) / DATETIME2(6) / TIMESTAMP(6)) — so the value bound
// here and the value the composer reads back are identical. Minted once per verb
// call and threaded down, never per statement.
//
// WHERE the instant is read from is the operator's declaration
// (relational.clock): the writing process under ClockApp, the database itself
// under ClockDB — one clock for a whole fleet, at the cost of one round-trip per
// write TX. Because the reading rides the OPEN transaction, the mint moved from
// "before Begin" to "just after it"; everything downstream is unchanged, since
// the value is still a Go time.Time bound as an argument.
func writeNow(ctx context.Context, tx Tx, clock ClockMode) (time.Time, error) {
	return core.NowFrom(ctx, tx, clock)
}

// writeTarget names the table a write statement runs against and how a criteria
// field resolves on it. For every ordinary write both come from the SAME schema
// (schemaTarget). A shared-ID SECONDARY table is the exception: a sibling
// declares no id of its own — TableSchema.ID panics on it, because it BORROWS
// the owner's — so the statement targets the sibling's table while the
// framework's "ID" field resolves to the column that owns the identity
// (idOnlyTarget).
//
// It exists because the statement builders below take a criteria predicate
// rather than a hardcoded `pk = ?`: compiling that predicate needs a resolver,
// and the resolver is not always the target table's own schema.
type writeTarget struct {
	table   string
	resolve core.FieldResolver
	idKind  func(string) core.IDKind
}

// schemaTarget targets a schema's table and resolves criteria fields through
// that same schema — every field it maps, plus the managed slots and the
// ParentID projection Resolve already answers.
func schemaTarget(s *TableSchema) writeTarget {
	return writeTarget{table: s.Table(), resolve: s.Resolve, idKind: s.IDKindOf}
}

// idOnlyTarget targets `table` and resolves the framework's "ID" field — and
// nothing else — to idColumn. It is what a shared-ID secondary table needs, and
// the narrowest resolver a by-id statement can be given: any other field name in
// the predicate fails to resolve rather than binding something unintended.
func idOnlyTarget(table, idColumn string) writeTarget {
	return writeTarget{
		table: table,
		resolve: func(goField string) (core.ResolvedField, bool) {
			if goField != idGoField {
				return core.ResolvedField{}, false
			}
			return core.ResolvedField{Column: idColumn}, true
		},
		idKind: func(goField string) core.IDKind {
			if goField == idGoField {
				return core.IDValue
			}
			return core.IDNone
		},
	}
}

// compilePredicate renders the WHERE fragment for a write statement, numbering
// its placeholders AFTER the `bound` arguments the statement already carries (a
// SET list). A nil predicate is refused here, at the lowest level: an UPDATE or
// DELETE with no WHERE is a full-table sweep, and no caller of these builders
// ever means one — the deliberate sweep renders its own statement.
func compilePredicate(d Dialect, t writeTarget, pred criteria.Expr, bound int) (string, []any, error) {
	if pred == nil {
		return "", nil, fmt.Errorf(
			"db: a write statement on %q was built with no predicate — that is a full-table "+
				"UPDATE/DELETE, which this path never emits", t.table)
	}
	where, args, err := core.CompileWhereForWrite(pred, t.resolve, d, t.idKind, bound, t.table)
	if err != nil {
		return "", nil, err
	}
	return where, args, nil
}

// buildInsert renders the INSERT for the bound columns + the managed timestamp
// columns (bound to the operation stamp `now`), with the Go-generated ID
// prepended. Placeholders, identifier quoting and value encoding all come from
// the Dialect, so the one statement serves both backends: EncodeArg renders the
// ID (and any UUID-shaped value) in the dialect's wire form — uuid text on
// Postgres, BINARY(16) on MySQL. No RETURNING: the id AND the timestamps are
// known up front.
func buildInsert(d Dialect, table, pk, id string, fields domain.Fields, nowCols []string, now time.Time, revCol string) (string, []any) {
	return buildInsertWithCounters(d, table, pk, id, fields, stampPlan{nowCols: nowCols}, now, revCol)
}

// buildInsertWithCounters is buildInsert plus the stamped COUNTER columns, which
// a fresh row starts at 1 — the row that creates the identity has counted one
// thing. They bind as ordinary arguments here (there is no existing value to add
// to yet); only on an UPDATE or an upsert conflict do they become the
// server-side `col = col + 1`.
func buildInsertWithCounters(d Dialect, table, pk, id string, fields domain.Fields, plan stampPlan, now time.Time, revCol string) (string, []any) {
	nowCols, counterCols := plan.nowCols, plan.counters
	keys := SortedKeys(fields)
	cols := make([]string, 0, len(keys)+1+len(nowCols))
	phs := make([]string, 0, len(keys)+1+len(nowCols))
	args := make([]any, 0, len(keys)+1+len(nowCols))

	n := 0
	cols = append(cols, d.QuoteIdent(pk))
	n++
	phs = append(phs, d.Placeholder(n))
	args = append(args, d.EncodeArg(domain.NewID(id)))

	for _, k := range keys {
		cols = append(cols, d.QuoteIdent(k))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, d.EncodeArg(fields[k]))
	}
	for _, nc := range nowCols {
		cols = append(cols, d.QuoteIdent(nc))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, d.EncodeArg(now))
	}
	for _, cc := range counterCols {
		cols = append(cols, d.QuoteIdent(cc))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, int64(1))
	}
	// A clearing verb on an INSERT says the same thing it says on an UPDATE: the
	// column is written, with the framework's value rather than the caller's. On a
	// fresh row NULL is what the column would have held anyway — asking for it is
	// still honoured, so the statement a reader sees matches the request.
	for _, nc := range plan.nullCols {
		cols = append(cols, d.QuoteIdent(nc))
		phs = append(phs, "NULL")
	}
	for _, zc := range plan.zeroCounters {
		cols = append(cols, d.QuoteIdent(zc))
		phs = append(phs, "0")
	}
	for _, zt := range plan.zeroTimes {
		cols = append(cols, d.QuoteIdent(zt))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, d.EncodeArg(time.Time{}))
	}
	if revCol != "" {
		// A fresh row starts its commit-order token at 1 — appended here so the
		// caller's fields map stays untouched (it flows into the outbox payload
		// and WriteResult.Fields; the physical revision column must never leak
		// there — the document form is the _revision watermark).
		cols = append(cols, d.QuoteIdent(revCol))
		n++
		phs = append(phs, d.Placeholder(n))
		args = append(args, int64(1))
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		d.QuoteIdent(table), strings.Join(cols, ", "), strings.Join(phs, ", "))
	return sql, args
}

// buildUpdate renders the UPDATE for the bound columns + the managed timestamp
// columns (bound to the operation stamp `now`), keyed on the ID. Existence is
// checked by the caller via the rows-affected count (no RETURNING) — uniform
// across dialects.
//
// expectedRevision is the
// OPTIMISTIC-CONCURRENCY guard: when it is > 0 (the entity came from a load,
// see domain.Managed.GetRevision) and the schema declares a revision column,
// the WHERE also pins that revision, so the statement matches only while the
// row still holds the value the caller read. A stale write then matches zero
// rows instead of reverting whatever landed in between — execExpectingRow turns
// that into ConcurrentModificationNotification (409).
//
// Pass 0 to write unguarded: an aggregate CHILD (guarded by its owner's token,
// it declares no revision of its own), a shared BASE (converged last-write-wins
// on purpose, since several roles write it), or an entity that never came from
// the loader.
func buildUpdate(d Dialect, t writeTarget, pred criteria.Expr, fields domain.Fields, nowCols []string, now time.Time, revCol string, expectedRevision int64) (string, []any, error) {
	return buildUpdatePlan(d, t, pred, fields, stampPlan{nowCols: nowCols}, now, revCol, expectedRevision)
}

// buildUpdatePlan is buildUpdate over a resolved stampPlan — the instant-bound
// columns and the server-side counters in one argument, so a caller cannot pass
// one and forget the other.
func buildUpdatePlan(d Dialect, t writeTarget, pred criteria.Expr, fields domain.Fields, plan stampPlan, now time.Time, revCol string, expectedRevision int64) (string, []any, error) {
	sets, args := buildSetWithCounters(d, fields, plan, now, revCol)
	where, whereArgs, err := compilePredicate(d, t, pred, len(args))
	if err != nil {
		return "", nil, err
	}
	args = append(args, whereArgs...)
	if revCol != "" && expectedRevision > 0 {
		where += " AND " + d.QuoteIdent(revCol) + " = " + d.Placeholder(len(args)+1)
		args = append(args, d.EncodeArg(expectedRevision))
	}
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		d.QuoteIdent(t.table), strings.Join(sets, ", "), where)
	return sql, args, nil
}

// buildSet renders the SET list shared by every UPDATE this package emits — the
// bound columns, the managed timestamp columns stamped with the operation's
// `now`, and the revision bump when the schema declares one — returning the
// fragments and the arguments in placeholder order.
func buildSet(d Dialect, fields domain.Fields, nowCols []string, now time.Time, revCol string) ([]string, []any) {
	return buildSetWithCounters(d, fields, stampPlan{nowCols: nowCols}, now, revCol)
}

// buildSetWithCounters is buildSet plus the stamped COUNTER columns. A counter
// is rendered exactly like the revision bump — `col = col + 1`, computed by the
// SERVER under the row's lock — and for the same reason: two writers that both
// read the value in Go and both wrote back read+1 would lose one of the two
// increments, and neither would notice.
func buildSetWithCounters(d Dialect, fields domain.Fields, plan stampPlan, now time.Time, revCol string) ([]string, []any) {
	nowCols, counterCols := plan.nowCols, plan.counters
	keys := SortedKeys(fields)
	sets := make([]string, 0, len(keys)+len(nowCols)+len(counterCols)+1)
	args := make([]any, 0, len(keys)+len(nowCols)+1)
	n := 0
	for _, k := range keys {
		n++
		sets = append(sets, d.QuoteIdent(k)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(fields[k]))
	}
	for _, nc := range nowCols {
		n++
		sets = append(sets, d.QuoteIdent(nc)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(now))
	}
	for _, cc := range counterCols {
		c := d.QuoteIdent(cc)
		sets = append(sets, c+" = "+c+" + 1")
	}
	// The three clearing verbs. NULL and the counter's 0 are literals — no value
	// travels for them, so nothing can be bound wrong; a time's zero binds like
	// any other instant, because how an instant is written is the dialect's.
	for _, nc := range plan.nullCols {
		sets = append(sets, d.QuoteIdent(nc)+" = NULL")
	}
	for _, zc := range plan.zeroCounters {
		sets = append(sets, d.QuoteIdent(zc)+" = 0")
	}
	for _, zt := range plan.zeroTimes {
		n++
		sets = append(sets, d.QuoteIdent(zt)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(time.Time{}))
	}
	if revCol != "" {
		rc := d.QuoteIdent(revCol)
		sets = append(sets, rc+" = "+rc+" + 1")
	}
	return sets, args
}

// rowExistsSQL renders the probe that splits a zero-row guarded UPDATE into its
// two causes: the row is gone (404) or it moved past the caller's revision
// (409). It runs only on that failure path, so the happy path pays nothing.
func rowExistsSQL(d Dialect, table, pk string) string {
	return d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(table)+
		" WHERE "+d.QuoteIdent(pk)+" = "+d.Placeholder(1), 1)
}

// archiveSQL archives ONE row by id: the archive stamp binds as the first arg
// (the operation's writeNow() value), the ID as the second — the same app-clock
// stamp every other statement of the operation carries.
//
// Its one caller is archiveChild: a child the aggregate marked Removed during an
// UPDATE. The ROOT verbs do not come through here — Archive/Unarchive write the
// entity's full field set with the DeletedAt transition as one more column (see
// softWrite), so the row's business state and the event that announces it can
// never disagree.
func archiveSQL(d Dialect, t writeTarget, sdCol string, pred criteria.Expr, now time.Time, revCol string) (string, []any, error) {
	sets, args := buildSet(d, domain.Fields{sdCol: now}, nil, now, revCol)
	where, whereArgs, err := compilePredicate(d, t, pred, len(args))
	if err != nil {
		return "", nil, err
	}
	args = append(args, whereArgs...)
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		d.QuoteIdent(t.table), strings.Join(sets, ", "), where), args, nil
}

func deleteSQL(d Dialect, t writeTarget, pred criteria.Expr) (string, []any, error) {
	where, args, err := compilePredicate(d, t, pred, 0)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s", d.QuoteIdent(t.table), where), args, nil
}

// childDeleteSQL renders the hard-delete of every child row belonging to a root:
// DELETE FROM child WHERE fk = $1. The aggregate delete path issues one per
// declared child table (before the root DELETE, same TX), so the framework owns
// the cascade in Go instead of depending on a database ON DELETE CASCADE. The
// single arg is the root id.
func childDeleteSQL(d Dialect, childTable, fkCol string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s = %s",
		d.QuoteIdent(childTable), d.QuoteIdent(fkCol), d.Placeholder(1))
}

// archiveCascadeSQL renders the ARCHIVE direction of the symmetric cascade: set
// the DeletedAt column to the operation stamp (bound as the FIRST arg) for
// the ACTIVE children of the root (second arg). Gated on `IS NULL` so it is
// idempotent, never re-stamps an already-archived child — and so the stamp it
// writes is, for every row it touches, the SAME instant the root row carries:
// one writeNow() per operation, bound here and by the root UPDATE alike. That
// equality is not a coincidence to preserve casually — it IS the discriminator
// unarchiveCascadeSQL reads back.
func archiveCascadeSQL(d Dialect, childTable, childSd, fkCol string) string {
	return fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = %s AND %s IS NULL",
		d.QuoteIdent(childTable), d.QuoteIdent(childSd), d.Placeholder(1),
		d.QuoteIdent(fkCol), d.Placeholder(2), d.QuoteIdent(childSd))
}

// unarchiveCascadeSQL renders the UNARCHIVE direction of the symmetric cascade:
// clear the DeletedAt column of the children the OWNER'S OWN archive stamped —
// the owner id binds first (the ParentID), the owner ROW id second.
//
// The gate used to be `IS NOT NULL` ("every archived child"), and that is the
// bug it exists to fix: a child archived on its own two years ago has nothing to
// do with the root's archive, yet the root's unarchive resurrected it along with
// the rest. The two cases are already distinguishable in the data, because the
// archive cascade binds the operation's single writeNow() instant on the owner
// row AND on every child row it stamps, while skipping (IS NULL gate) the
// children that were already archived — those keep their own, older stamp. So
// "was archived BY this owner's archive" reads exactly as "carries the owner's
// archive stamp", with no marker column and no backfill: rows written by earlier
// versions of the framework already carry it.
//
// The owner's stamp is read INSIDE the statement, as a sub-select on its own
// row, instead of being bound as a Go time.Time. Both forms are the same
// comparison on paper; only this one survives every driver. A bound time.Time
// carries a location, and a driver is free to hand it to the server as a
// TIMESTAMP WITH TIME ZONE — Oracle's does — against a column declared without
// one, and the server then reconciles the two through the session's time zone
// and matches nothing. It does not error: the restore simply reaches no row
// while the event says the children woke up. Column against column, the values
// never leave the server and no time zone is ever introduced.
//
// The statement therefore MUST run before the owner's own row is cleared (see
// cascadeChildren / unarchiveBaseCascade) — that sub-select is the discriminator.
//
// The comparison is still an equality between two stored timestamps, so it is
// only as sharp as the columns' precision: owner and child DeletedAt columns must
// share the same type, with sub-second precision (DATETIME(6), TIMESTAMP(6),
// DATETIME2(6), TIMESTAMPTZ — what the generator emits). A second-precision
// column collapses two operations that happened within the same second into one
// stamp; a child column COARSER than the owner's truncates the stamp it was
// given and stops matching, which fails safe (nothing is revived) but silently.
func unarchiveCascadeSQL(d Dialect, childTable, childSd, fkCol, ownerTable, ownerSd, ownerPK string) string {
	return fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s = %s AND %s = (SELECT %s FROM %s WHERE %s = %s)",
		d.QuoteIdent(childTable), d.QuoteIdent(childSd),
		d.QuoteIdent(fkCol), d.Placeholder(1), d.QuoteIdent(childSd),
		d.QuoteIdent(ownerSd), d.QuoteIdent(ownerTable), d.QuoteIdent(ownerPK), d.Placeholder(2))
}

// childSiblingDeleteSQL hard-deletes a child's sibling rows when the root is
// deleted. The children are removed in bulk by their ParentID to the root (no per-row
// ids), so a child's siblings — keyed on the child's shared ID — are removed via
// a subquery over the soon-to-be-deleted child rows. Must run BEFORE the child
// rows are deleted (the subquery reads them). The single arg is the root id.
func childSiblingDeleteSQL(d Dialect, sibTable, childPKCol, childTable, childFKCol string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s IN (SELECT %s FROM %s WHERE %s = %s)",
		d.QuoteIdent(sibTable), d.QuoteIdent(childPKCol),
		d.QuoteIdent(childPKCol), d.QuoteIdent(childTable),
		d.QuoteIdent(childFKCol), d.Placeholder(1))
}

// buildSiblingUpsert renders the INSERT-or-update of one sibling row keyed on
// the shared ID: INSERT (pk + fields) ON CONFLICT(pk) DO UPDATE each field to the
// proposed value. Dialect-agnostic via Dialect.BuildUpsert (PG ON CONFLICT ⟷
// MySQL ON DUPLICATE KEY). Args bind in cols order (pk first, then SortedKeys);
// managed timestamp columns are not handled here (siblings carry plain fields).
func buildSiblingUpsert(d Dialect, sib *TableSchema, pkCol, id string, fields domain.Fields) (string, []any) {
	keys := SortedKeys(fields)
	cols := make([]string, 0, len(keys)+1)
	args := make([]any, 0, len(keys)+1)
	sets := make([]UpsertSet, 0, len(keys))

	cols = append(cols, pkCol)
	args = append(args, d.EncodeArg(domain.NewID(id)))
	for _, k := range keys {
		cols = append(cols, k)
		args = append(args, d.EncodeArg(fields[k]))
		sets = append(sets, UpsertSet{Col: k, Mode: UpsertSetNew})
	}
	return d.BuildUpsert(sib.Table(), cols, []string{pkCol}, sets), args
}

// requireDeletedAt is the runtime backstop for the boot-time Modes() ⟺
// DeletedAt check: a write path needing the DeletedAt column on a schema that
// did not declare it fails loudly instead of emitting broken SQL.
func requireDeletedAt(s *TableSchema, entityName string) (string, error) {
	col, ok := s.DeletedAtColumn()
	if !ok {
		return "", fmt.Errorf(
			"db: %s did not declare DeletedAt in its TableSchema — archive/unarchive is unavailable",
			entityName,
		)
	}
	return col, nil
}

// stampPlan is what one write's stamp requests resolve into: the columns bound
// to the operation's instant (the verb's managed timestamps plus every stamped
// TIME column asked for), the counter columns rendered server-side as
// `col = col + 1`, and the subset the payload has to be told about.
//
// Counters are deliberately absent from payload. Their new value is computed by
// the SERVER and the framework does not read it back, so there is no honest
// value to put in the payload or on the entity — and stating the old one, or a
// guessed old+1, would be worse than saying nothing. The projection is
// unaffected: the SyncEngine re-reads the row.
type stampPlan struct {
	nowCols  []string
	counters []string
	// nullCols are the stamped columns this write CLEARS — `col = NULL`. The
	// absence is the framework's own value, known before the statement runs, so
	// unlike a counter's increment it is written back onto the entity too.
	nullCols []string
	// zeroTimes and zeroCounters are the stamped columns this write RESETS to the
	// declared type's zero. They are two buckets rather than one because the
	// statement says it differently: a time binds the zero instant as an argument
	// (the dialect owns how an instant is encoded), a counter is the literal 0.
	zeroTimes    []string
	zeroCounters []string
	// requestedTimes are the stamped TIME columns a request asked to FILL, kept
	// apart from nowCols (which also holds the verb's own managed timestamps)
	// because only the requested ones belong in the payload.
	requestedTimes []string
	payload        []string
}

// splitStamps turns the claims the schema resolved into the buckets a statement
// renders, preserving declaration order within each. It is the ONE place a verb
// becomes a piece of SQL, so the insert path, the update path and the upsert's
// conflict clause cannot disagree about what a verb means.
func (p *stampPlan) splitStamps(claims []core.ClaimedStamp) {
	for _, c := range claims {
		switch {
		case c.Op == domain.StampToNull:
			p.nullCols = append(p.nullCols, c.Column)
		case c.Op == domain.StampToEmpty && c.Counter:
			p.zeroCounters = append(p.zeroCounters, c.Column)
		case c.Op == domain.StampToEmpty:
			p.zeroTimes = append(p.zeroTimes, c.Column)
		case c.Counter:
			p.counters = append(p.counters, c.Column)
		default:
			p.requestedTimes = append(p.requestedTimes, c.Column)
		}
	}
	// The payload states what the framework KNOWS it wrote. A filled time, a
	// cleared column and a reset one all qualify; a counter's increment does not
	// (its new value is the server's and is never read back).
	p.payload = append(append(append(append([]string{},
		p.requestedTimes...), p.nullCols...), p.zeroTimes...), p.zeroCounters...)
	// A requested time joins the verb's own managed timestamps: both are bound to
	// the SAME instant, and one statement dates everything it touches alike.
	if len(p.requestedTimes) > 0 {
		p.nowCols = append(append(make([]string, 0, len(p.nowCols)+len(p.requestedTimes)),
			p.nowCols...), p.requestedTimes...)
	}
}

// writesBack reports whether this plan has a value the framework can put back on
// the struct — everything except a bare counter increment, whose new value is the
// server's and is never read back.
func (p *stampPlan) writesBack() bool { return len(p.payload) > 0 }

// stampedCols resolves the stamped columns a write requested — the Go field
// names the domain accumulated through domain.Managed.Stamp, translated through
// the schema — and splits them by what filling each one MEANS.
//
// It also writes the instant back onto the entity for the TIME columns. The
// audit event reads its values from the STRUCT, and the caller keeps holding
// this entity after the write — without the write-back both would report the
// field as still empty on the very write that filled it.
//
// A schema with no stamped field never allocates: the requests cannot mean
// anything and are not read. A request naming a field the schema did not declare
// stamped is an error here — the domain asks by Go name and cannot see the
// schema, so this is the first moment the two meet.
func stampedCols(schema *TableSchema, src any, nowCols []string, now time.Time) (stampPlan, error) {
	plan, unclaimed, err := claimStampedCols(schema, src, nowCols, now)
	if err != nil {
		return stampPlan{}, err
	}
	// One schema owns the whole request here, so anything it did not claim is a
	// mistake — there is no sibling schema for the name to have belonged to.
	if err := schema.RefuseUnclaimedStamps(unclaimed); err != nil {
		return stampPlan{}, err
	}
	return plan, nil
}

// claimStampedCols is stampedCols for a write where MORE THAN ONE schema shares
// the entity's requests — a shared-base role writes its own row and the base's,
// and a name meant for one is not a mistake to the other. It claims what this
// schema declares and reports the rest, leaving the refusal to a caller that can
// see every schema in the operation.
func claimStampedCols(schema *TableSchema, src any, nowCols []string, now time.Time) (stampPlan, []string, error) {
	plan := stampPlan{nowCols: nowCols}
	asked := domain.RequestedStamps(src)
	if !schema.HasStampedFields() {
		return plan, domain.StampFields(asked), nil
	}
	claimed, unclaimed, err := schema.ClaimStampRequests(asked)
	if err != nil {
		return stampPlan{}, nil, err
	}
	if len(claimed) == 0 {
		return plan, unclaimed, nil
	}
	plan.splitStamps(claimed)
	schema.ApplyStamps(src, asked, now)
	return plan, unclaimed, nil
}

// stampedChildCols is stampedCols for an aggregate CHILD. It differs in one
// thing only, and that thing is the whole reason it exists: a child travels as
// an interface holding a struct VALUE, which is not addressable, so the
// write-back cannot happen in place. The value is written back into the
// AGGREGATE MAP instead — the copy every post-write reader sees — through the
// same seam the minted child id already uses.
//
// root may be nil (a child written outside an aggregate root): the statement is
// still stamped correctly, only the write-back is skipped.
func stampedChildCols(child *TableSchema, root *domain.AggregateRoot, item domain.AggregateValueObject, nowCols []string, now time.Time) (stampPlan, error) {
	plan := stampPlan{nowCols: nowCols}
	if !child.HasStampedFields() {
		return plan, nil
	}
	asked := domain.RequestedStamps(item)
	claimed, err := child.StampRequestColumns(asked)
	if err != nil {
		return stampPlan{}, err
	}
	if len(claimed) == 0 {
		return plan, nil
	}
	plan.splitStamps(claimed)
	if root != nil && plan.writesBack() {
		domain.ApplyToAggregateItem(root, item, func(ptr any) bool {
			child.ApplyStamps(ptr, asked, now)
			return true
		})
	}
	return plan, nil
}

// withStamps returns the payload's view of the written columns: the bound fields
// plus the stamped ones the statement filled. The copy is deliberate — fields is
// the very map the DML binds, and the payload must never reach back into it (the
// redaction pass has the same rule, for the same reason).
func withStamps(fields domain.Fields, plan stampPlan, now time.Time) domain.Fields {
	if len(plan.payload) == 0 {
		return fields
	}
	out := make(domain.Fields, len(fields)+len(plan.payload))
	for k, v := range fields {
		out[k] = v
	}
	// Each bucket states the value the STATEMENT wrote, not a single instant for
	// all of them: a cleared column that reported `now` here would tell the
	// projection the opposite of what the row holds.
	for _, c := range plan.requestedTimes {
		out[c] = now
	}
	for _, c := range plan.nullCols {
		out[c] = nil
	}
	for _, c := range plan.zeroTimes {
		out[c] = time.Time{}
	}
	for _, c := range plan.zeroCounters {
		out[c] = int64(0)
	}
	return out
}
