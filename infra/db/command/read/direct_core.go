package read

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// directCore is the entity-free half of a relational read: the schema, the
// engine, the declared traversals and the optional transaction binding — plus
// everything that can be answered from those alone.
//
// Every method here predates this type: they were written on AggregateLoader[T]
// and never touched T. Splitting them out is what lets a table with NO entity
// behind it — a control table, an aggregate's child counted as a fact — run the
// SAME criteria compilation, the SAME resolution, the SAME joins and the SAME
// aggregate DSL as an aggregate root does. AggregateLoader embeds it, so the
// aggregate path is one of this core's callers rather than a parallel one, and
// DirectRepository is the other.
type directCore struct {
	eng    RelationalEngine
	schema *TableSchema
	// name is what this read answers under in errors — TypeName[T]() for a
	// loader, the row type's name for a Direct repository, or an explicit
	// override.
	name string
	// joins are the READ-ONLY traversals this read may reach across — declared
	// up front, validated against the schema at construction, and inert until a
	// read actually uses one. See join.go.
	joins []Join
	// joinsDeclared records that the declaration already ran, so a second call is
	// refused rather than silently validated against its own argument alone. A
	// flag rather than len(joins) > 0: the rule is "declare them once", and a
	// first call that happened to declare nothing must not turn that into
	// "declare them once, unless the first time was empty".
	joinsDeclared bool
	// tx binds this read to the framework's OPEN transaction; nil runs it on the
	// engine's pool. A read inside the write's own transaction sees that
	// transaction's uncommitted rows — which is the whole point of asking a fact
	// from a lifecycle hook.
	tx core.Tx
}

// rowSource is the surface a read runs its statement through: the engine's
// pooled Querier, or the framework's open transaction when the caller bound one.
// Both already speak it; the interface exists so the statement code names
// neither.
type rowSource interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// newJoinedTables opens the accumulator a criteria compilation records its 1:1
// joins into. hasDeclared is seeded from the declared root traversals, which are
// in the FROM whether or not any field mentions them.
func newJoinedTables(joins []Join) *joinedTables {
	return &joinedTables{
		siblings:    map[string]*TableSchema{},
		hasDeclared: len(rootJoins(joins)) > 0,
	}
}

// rows answers where this read executes: the bound transaction when there is
// one, the engine's pool otherwise.
//
// EVERY read in this package runs through here — the loader's and the Direct
// repository's alike. A statement that reached for the pooled Querier directly
// would silently ignore an InTx binding, and the failure mode is the quiet kind:
// a fact asked from inside a write would answer about the state BEFORE that
// write, which is the one thing asking it in-transaction was for.
func (c *directCore) rows() rowSource {
	if c.tx != nil {
		return c.tx
	}
	return c.eng.Querier()
}

// contextName is the name this read answers under in errors and panics — the
// loader's WithContextName override, or the Go type name it defaulted to at
// construction.
func (c *directCore) contextName() string { return c.name }

// WithJoins declares the READ-ONLY traversals this aggregate may reach across in
// a query — see join.go for what a join is and why it lives here rather than on
// the TableSchema. Every declaration is validated against the schema NOW: a join
// naming a column, a child or a Go field that does not exist panics at
// construction, never on the first request. Call WithSchema first.
//
// It takes the WHOLE set and may be called ONCE. The rules that make a set of
// joins coherent are cross-declaration — one foreign key reaches one table, one
// Go field receives one column — so they can only be checked against every
// traversal at the same time. A second call would validate its own argument
// against a schema that says nothing about what the first call already claimed,
// and the collision would surface as a duplicate SQL alias on the first read, or
// as two scan targets writing the same struct field in silence. Declaring all of
// them in one call is what makes "validated at construction" true.
func (c *directCore) declareJoins(bindings ...*JoinBinding) {
	if c.joinsDeclared {
		panic(fmt.Sprintf(
			"read.WithJoins[%s]: called twice — a loader declares its traversals ONCE, in one call. "+
				"The rules that keep them coherent (one foreign key reaches one table, one Go field "+
				"receives one column) span the whole set, so a second call cannot be checked against "+
				"the first: merge every read.InnerJoin/LeftJoin/...InChild into a single WithJoins(...).",
			c.contextName()))
	}
	c.joinsDeclared = true
	joins := make([]Join, 0, len(bindings))
	for _, b := range bindings {
		if b == nil {
			continue
		}
		joins = append(joins, b.build())
	}
	validateJoins(c.contextName(), c.schema, joins)
	c.joins = append(c.joins, joins...)
}

// Joins exposes the declared traversals. A read model built over this loader
// reads them to know which fields it can serve beyond the schema's own.
func (c *directCore) Joins() []Join { return c.joins }

// JoinFields names the Go fields the declared joins add, keyed by the table they
// land on — the root's for a root join, the child's for a child join. A read
// model over this loader consults it to know what it can serve beyond the
// TableSchema; the fields themselves are ordinary fields of the loaded entity.
func (c *directCore) JoinFields() map[string][]string {
	if len(c.joins) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, j := range c.joins {
		table := c.schema.Table()
		if j.Child != nil {
			table = j.Child.Table()
		}
		for _, f := range j.Fields {
			out[table] = append(out[table], f.GoField)
		}
	}
	return out
}

// Exists reports whether at least one root matches the criteria, under the same
// scope gate as FindOne/FindAll (active rows by default; the Query's scope can
// include archived). It compiles to an indexed existence probe — SELECT 1 …
// LIMIT 1 — and hydrates NOTHING: this is the primitive for uniqueness
// pre-checks and cardinality guards on the write path, where loading whole
// aggregates to answer a yes/no question would waste the request's latency.
// The ID is addressable as the fixed Go-side field "ID" (exclude-self checks:
// And(Eq("Field", v), Not(Eq("ID", id)))); criteria may reference sibling and
// shared-base fields — the same LEFT JOINs FindAll uses apply.
func (c *directCore) Exists(ctx context.Context, q *criteria.Query) (bool, error) {
	fromJoin, clause, args, err := c.compileFilter(q)
	if err != nil {
		return false, err
	}
	return c.probeExists(ctx, fromJoin+clause, args...)
}

// compileFilter renders the shared front-half of a root query — FROM (+ the
// sibling/shared-base LEFT JOINs the criteria pulled in) and the WHERE clause
// (predicate + scope gate) with its ordered args. The probe/aggregate methods
// reuse exactly the resolution and gating semantics of findRoots without its
// SELECT/scan machinery.
func (c *directCore) compileFilter(q *criteria.Query) (fromJoin, clause string, args []any, err error) {
	joins := newJoinedTables(c.joins)
	return c.compileFilterJoins(q, joins)
}

// idKindResolver reports the identity typing of a criteria field across the
// SAME resolution surface resolverRecordingJoins walks — the anchor schema, then its
// siblings, then the shared base, then the DECLARED ROOT JOINS. The kind is
// derived from the Go struct (TableSchema.IDKindOf — the field TYPE is the
// declaration), so a bare-string probe on a domain.ID-typed field binds in the
// dialect's native id form; the managed ID slot ("ID") is always IDValue via the
// anchor. A type-less shared base derives nothing and answers IDNone for its own
// fields.
//
// The join leg is what keeps a traversal's fields honest in a predicate. A join
// field is addressable in a criteria, and one that maps an IDENTITY column of the
// target is declared on this side as a plain string (a join field carries no
// domain type) — so nothing about the FIELD says "identity" and the probe would
// bind as text. On mysql, sqlserver and oracle the column is BINARY(16)/RAW(16),
// and a text probe against it matches NOTHING, silently. The typing therefore
// comes from where it is declared: the TARGET's schema. Only root joins are
// consulted, because a child join is load-only and never reaches a predicate.
func (c *directCore) idKindResolver() func(string) core.IDKind {
	anchor := c.schema
	sibs := anchor.Siblings()
	base, _, hasBase := anchor.SharedBaseRef()
	joins := rootJoins(c.joins)
	return func(goField string) core.IDKind {
		if k := anchor.IDKindOf(goField); k != core.IDNone {
			return k
		}
		for _, sib := range sibs {
			if k := sib.IDKindOf(goField); k != core.IDNone {
				return k
			}
		}
		if hasBase {
			if k := base.IDKindOf(goField); k != core.IDNone {
				return k
			}
		}
		for _, j := range joins {
			for _, f := range j.Fields {
				if f.GoField == goField {
					return targetIDKindOf(j, f)
				}
			}
		}
		return core.IDNone
	}
}

// compileFilterJoins is compileFilter with a caller-owned joins accumulator, so
// an aggregate method can resolve its aggregated field through the SAME joins
// (a sibling field pulls its LEFT JOIN whether it appears in the predicate or
// in the SELECT aggregate).
func (c *directCore) compileFilterJoins(q *criteria.Query, joins *joinedTables) (fromJoin, clause string, args []any, err error) {
	if q == nil {
		q = criteria.Where(nil)
	}
	resolve := c.resolverRecordingJoins(joins)
	dialect := c.eng.Dialect()

	// A DECLARED read join is known before any resolution (it is on the loader,
	// not discovered from the criteria), so the owner qualification is decided
	// up front — the single pass below already emits it.
	qual := core.ColQual{Owner: len(rootJoins(c.joins)) > 0}
	where, args, err := core.CompileWhereQualified(q.Condition(), resolve, dialect, c.idKindResolver(), qual)
	if err != nil {
		return "", "", nil, err
	}
	rootQualifier := ""
	if joins.any() {
		rootQualifier = dialect.QuoteIdent(c.schema.Table())
		// Qualify the anchor id under the 1:1 join so a predicate mixing the id
		// and a sibling/base field (e.g. an exclude-self uniqueness probe) is
		// unambiguous — see findRoots for the full rationale. Only a sibling/base
		// join needs this second pass: it is discovered DURING resolution, so the
		// first pass could not know about it.
		qual.IDCol, qual.IDQualifier = c.schema.IDColumn(), rootQualifier
		where, args, err = core.CompileWhereQualified(q.Condition(), resolve, dialect, c.idKindResolver(), qual)
		if err != nil {
			return "", "", nil, err
		}
	}
	clause = core.BuildWhereClause(where, core.ScopeGate(q.Scope(), c.schema, dialect, rootQualifier))
	if clause != "" {
		// Leading separator so callers can append fromJoin+clause directly — a
		// dialect may render the table unquoted, and "tableWHERE" lexes as one
		// identifier.
		clause = " " + clause
	}
	fromJoin = dialect.QuoteIdent(c.schema.Table()) + c.joinClause(joins, dialect)
	return fromJoin, clause, args, nil
}

// probeExists executes the shared existence probe — SELECT 1 over the given
// FROM/WHERE tail, capped at one row via the dialect, true when any row comes
// back. The single execution home for the public criteria-level Exists and the
// internal column-level probes.
func (c *directCore) probeExists(ctx context.Context, fromWhere string, args ...any) (bool, error) {
	rows, err := c.rows().Query(ctx, c.eng.Dialect().ApplyLimit("SELECT 1 FROM "+fromWhere, 1), args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
}

// resolverRecordingJoins resolves a criteria Go field to its column and records the 1:1
// join the answer implies, so compileFilterJoins emits the matching LEFT JOIN.
//
// The resolution itself is core.TableSchema.Resolve — the ONE surface every
// read path consults, so this loader admits exactly the names a Mongo view of
// the same schema admits. What is left here is the part that IS this backing's
// business: turning "the column lives on that sibling / on the shared base"
// into a JOIN. Deriving the bookkeeping FROM the resolution is the point — the
// two used to be separate walks, and a name the walks disagreed about produced
// a WHERE against a table the FROM never joined.
//
// Sibling and base columns are unique vs the anchor (the schema bijection), so
// they stay unqualified; only the shared ID is ambiguous under a join, and
// findRoots/compileFilterJoins qualify it to the anchor table — so a criteria
// may freely mix the ID and a specialization field.
//
// A name the SCHEMA does not answer may still be a field of a declared join
// (WithJoins). Those resolve LAST — the schema always wins, so declaring a join
// can never change the meaning of a name the entity already had — and they come
// back qualified by the join's alias, because a joined aggregate shares no
// namespace with the anchor.
func (c *directCore) resolverRecordingJoins(j *joinedTables) core.FieldResolver {
	return func(goField string) (core.ResolvedField, bool) {
		if r, ok := c.schema.Resolve(goField); ok {
			switch r.Owner {
			case core.OwnerSibling:
				j.siblings[r.Schema.Table()] = r.Schema
			case core.OwnerSharedBase:
				if _, fk, has := c.schema.SharedBaseRef(); has {
					j.base, j.baseFK = r.Schema, fk
				}
			}
			return r, true
		}
		return c.resolveDeclaredJoin(j, goField)
	}
}

// resolveDeclaredJoin answers a Go field name declared by a ROOT join, recording
// the traversal so joinClause emits it. Child joins are load-only and are not
// offered here: filtering the root by a field of a 1:N child is a pushdown a
// single root SELECT cannot express.
func (c *directCore) resolveDeclaredJoin(j *joinedTables, goField string) (core.ResolvedField, bool) {
	for _, dj := range c.joins {
		if dj.Child != nil {
			continue
		}
		for _, f := range dj.Fields {
			if f.GoField != goField {
				continue
			}
			return core.ResolvedField{
				Column:    f.Column,
				Schema:    dj.Target,
				Owner:     core.OwnerJoin,
				Qualifier: joinAlias(dj),
			}, true
		}
	}
	return core.ResolvedField{}, false
}

// joinClause renders a LEFT JOIN per referenced sibling (shared ID) and the
// shared base (role ParentID → base ID), ordered deterministically. Empty when the
// criteria referenced no specialization field.
func (c *directCore) joinClause(j *joinedTables, dialect Dialect) string {
	if len(j.siblings) == 0 && j.base == nil && len(rootJoins(c.joins)) == 0 {
		return ""
	}
	anchor := dialect.QuoteIdent(c.schema.Table())
	pk := dialect.QuoteIdent(c.schema.IDColumn())
	var sb strings.Builder
	tables := make([]string, 0, len(j.siblings))
	for t := range j.siblings {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		st := dialect.QuoteIdent(t)
		sb.WriteString(" LEFT JOIN " + st + " ON " + anchor + "." + pk + " = " + st + "." + pk)
	}
	if j.base != nil {
		bt := dialect.QuoteIdent(j.base.Table())
		sb.WriteString(" LEFT JOIN " + bt + " ON " + bt + "." + dialect.QuoteIdent(j.base.IDColumn()) +
			" = " + anchor + "." + dialect.QuoteIdent(j.baseFK))
	}
	// Declared joins (WithJoins), each under its own alias so two traversals to
	// the SAME table stay distinct. Emitted in declaration order, which is the
	// order the SELECT list and the scan targets follow.
	//
	// The alias follows the table with NO "AS": that keyword is optional before a
	// TABLE alias in standard SQL and Oracle rejects it outright (ORA-02000),
	// while every other backend accepts the bare form. Column aliases are a
	// different position with a different rule — the dialects that write one keep
	// their AS.
	for _, dj := range rootJoins(c.joins) {
		verb := " LEFT JOIN "
		if dj.Kind == JoinInner {
			verb = " INNER JOIN "
		}
		qa := dialect.QuoteIdent(joinAlias(dj))
		sb.WriteString(verb + dialect.QuoteIdent(dj.Target.Table()) + " " + qa +
			" ON " + qa + "." + dialect.QuoteIdent(dj.Target.IDColumn()) +
			" = " + anchor + "." + dialect.QuoteIdent(dj.FKColumn))
	}
	return sb.String()
}
