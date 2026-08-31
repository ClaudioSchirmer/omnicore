package core

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// sqlVisitor walks a criteria.Expr and accumulates a WHERE fragment + bound
// args. Unexported — the developer never constructs or sees it. Identifiers
// pass through validIdentifier (columns are framework/TableSchema-derived, never
// user input); values are parameterized; domain.ID args are unwrapped to their
// string value. The Go-field → column lookup is FieldResolver (built from
// the TableSchema by whichever statement is being compiled); idKind is the field's identity
// typing (TableSchema.IDKindOf, derived from the Go struct), so a probe
// against a domain.ID-typed field binds in the dialect's native id form even
// when the caller hands a bare string.
type sqlVisitor struct {
	resolve FieldResolver
	dialect Dialect
	idKind  func(goField string) IDKind // nil = no id lifting
	// qual carries how this statement must qualify the columns it renders —
	// see ColQual.
	qual ColQual
	// base is how many arguments the statement has ALREADY bound before this
	// predicate. A SELECT binds nothing first and leaves it 0; an UPDATE binds
	// its SET list first, so the WHERE must continue that numbering instead of
	// restarting at 1 — on a positional dialect a restart silently binds the
	// wrong values.
	base int
	sb   strings.Builder
	args []any

	// ─── subquery scaffolding ────────────────────────────────────────────────
	//
	// parent is the ENCLOSING scope, nil for a statement's own predicate. It is
	// what an Outer(...) reference resolves against — one level, never further:
	// searching outward is how SQL silently binds a mistyped inner column to an
	// outer table, and this module declares names instead of inferring them.
	parent *sqlVisitor
	// selfQual is the (quoted) qualifier this scope's own columns carry. Empty
	// at statement level, where qual already says what the FROM demands; a
	// subquery sets it to its source's alias, so every column it renders is
	// unambiguous and no name can drift out to the enclosing scope.
	selfQual string
	// depth is the nesting level (0 = statement), which names the alias.
	depth int
	// writeTarget is the table an UPDATE/DELETE is modifying, empty on a read.
	// It exists for one engine-specific refusal — see Dialect
	// AllowsSubqueryOnWriteTarget.
	writeTarget string
	// srcSchema is the subquery's source, nil at statement level. It is carried
	// only so an unresolved name gets the message that fits the place it was
	// written in.
	srcSchema *TableSchema
}

// ColQual is the qualification a statement needs for the columns it renders,
// decided by what is in its FROM.
//
// Two independent triggers, because two different things become ambiguous:
//
//   - owner: the statement holds a DECLARED read join (WithJoins). A joined
//     aggregate is a FOREIGN namespace — nothing stops it from having a "name",
//     a "code" or the framework's own deleted_at/created_at/updated_at/revision
//     — so EVERY column on the anchor side must be qualified by the table it
//     physically lives on, not just the id. Anchor, siblings and shared base are
//     all in the FROM under their own table names (never an alias), so their
//     table IS their qualifier.
//   - idCol/idQualifier: the statement pulled in a 1:1 sibling/shared-base LEFT
//     JOIN. Those share the node's schema bijection, so their business columns
//     stay unique; only the SHARED id exists in both tables. Qualifying just the
//     id keeps the emitted SQL of that (much older, much more common) case
//     byte-identical.
//
// The zero value is the single-table default: nothing qualified.
type ColQual struct {
	Owner       bool
	IDCol       string
	IDQualifier string
}

// QualifyCol renders a resolved field as the SQL identifier the statement needs.
//
// Qualification is decided by WHERE the column lives, not by which column it is.
// A joined aggregate always carries a qualifier — its columns share a namespace
// with nobody, so an unqualified "name" could belong to either side. The anchor,
// its siblings and its shared base carry the table they live on whenever a
// declared join is in the FROM (ColQual.owner): the schema's bijection makes
// their names unique across the NODE, and says nothing about the foreign
// namespace a join drags in. Without a declared join they stay bare, except the
// anchor's own id under a 1:1 join, which the joined table also has.
func QualifyCol(rf ResolvedField, q ColQual, dialect Dialect) string {
	col := dialect.QuoteIdent(rf.Column)
	if rf.Qualifier != "" {
		return dialect.QuoteIdent(rf.Qualifier) + "." + col
	}
	if q.Owner && rf.Schema != nil {
		return dialect.QuoteIdent(rf.Schema.Table()) + "." + col
	}
	if q.IDQualifier != "" && rf.Column == q.IDCol {
		return q.IDQualifier + "." + col
	}
	return col
}

// place binds one probe value for the (Go-named) field and returns its
// placeholder. A probe on an identity-typed field (domain.ID / *domain.ID —
// the field's Go TYPE is the declaration) is lifted into domain.ID first, so
// the dialect's typed codec renders it (BINARY(16) on MySQL, uuid text on PG);
// non-string probes (LIKE %patterns% never reach here as ids in practice —
// they stay strings on string-typed fields) and already-typed values pass to
// EncodeArg untouched.
func (v *sqlVisitor) place(goField string, val any) string {
	if v.idKind != nil {
		switch v.idKind(goField) {
		case IDValue, IDPointer:
			val = liftIDProbe(val)
		}
	}
	v.args = append(v.args, v.dialect.EncodeArg(val))
	return v.dialect.Placeholder(v.base + len(v.args))
}

// liftIDProbe lifts a bare probe value into the identity type: string →
// domain.ID, *string → *domain.ID (nil stays a typed nil → SQL NULL);
// already-typed values (domain.ID, *domain.ID, uuid.UUID) and anything else
// pass through for EncodeArg's own handling.
func liftIDProbe(val any) any {
	switch v := val.(type) {
	case string:
		return domain.NewID(v)
	case *string:
		if v == nil {
			return (*domain.ID)(nil)
		}
		return domain.NewID(*v)
	default:
		return val
	}
}

// column resolves a Go field of THIS scope and renders it as the SQL identifier
// the scope needs: what the statement's FROM demands at the top level (byte for
// byte what it always was), the subquery's own alias inside one.
func (v *sqlVisitor) column(goField string) (string, error) {
	rf, ok := v.resolve(goField)
	if !ok {
		if v.srcSchema != nil {
			return "", subFieldError(v.srcSchema, goField, "a predicate on")
		}
		return "", fmt.Errorf("criteria: unknown field %q (not a persisted field of the entity)", goField)
	}
	if v.selfQual != "" {
		return v.selfQual + "." + v.dialect.QuoteIdent(rf.Column), nil
	}
	return QualifyCol(rf, v.qual, v.dialect), nil
}

// columnForInner renders one of THIS scope's columns as a NESTED scope must
// spell it: always qualified. At statement level that means the table the column
// physically lives on (QualifyCol's owner form) — which is legal with no change
// to the enclosing statement, because that table is in its FROM under its own
// name; inside a subquery it is that subquery's alias.
func (v *sqlVisitor) columnForInner(rf ResolvedField) string {
	if v.selfQual != "" {
		return v.selfQual + "." + v.dialect.QuoteIdent(rf.Column)
	}
	return QualifyCol(rf, ColQual{Owner: true}, v.dialect)
}

// outerColumn renders an Outer(...) reference: the enclosing scope's column,
// qualified, with no argument bound. It is what makes a subquery correlated.
func (v *sqlVisitor) outerColumn(ref criteria.OuterRef) (string, error) {
	if v.parent == nil {
		return "", fmt.Errorf(
			"criteria: Outer(%q) used outside a subquery — an outer reference only means "+
				"something inside Sub(...).Where(...)", ref.Field)
	}
	rf, ok := v.parent.resolve(ref.Field)
	if !ok {
		return "", fmt.Errorf(
			"criteria: Outer(%q): the enclosing statement has no such field. An outer reference "+
				"reaches exactly one level up and is never searched for further out", ref.Field)
	}
	return v.parent.columnForInner(rf), nil
}

// operand renders the right-hand side of a comparison: a bound placeholder for a
// literal, the enclosing scope's column for an Outer reference. Correlation
// therefore costs no operator of its own — every builder that takes an `any`
// value takes an OuterRef.
func (v *sqlVisitor) operand(goField string, val any) (string, error) {
	if ref, ok := val.(criteria.OuterRef); ok {
		return v.outerColumn(ref)
	}
	return v.place(goField, val), nil
}

func (v *sqlVisitor) VisitComparison(c criteria.Comparison) error {
	col, err := v.column(c.Field)
	if err != nil {
		return err
	}

	switch c.Op {
	case criteria.OpIsNull:
		if len(c.Values) != 0 {
			return fmt.Errorf("criteria: operator %q on %q takes no values, got %d", c.Op, c.Field, len(c.Values))
		}
		v.sb.WriteString(col)
		v.sb.WriteString(" IS NULL")
	case criteria.OpNotNull:
		if len(c.Values) != 0 {
			return fmt.Errorf("criteria: operator %q on %q takes no values, got %d", c.Op, c.Field, len(c.Values))
		}
		v.sb.WriteString(col)
		v.sb.WriteString(" IS NOT NULL")
	case criteria.OpIn, criteria.OpNin:
		if len(c.Values) == 0 {
			// An empty set is a well-defined predicate, not an error (SQL forbids
			// the literal `IN ()`): `IN ()` matches nothing, `NOT IN ()` matches
			// everything — the same semantics MongoDB gives `$in:[]` / `$nin:[]`,
			// so the relational and Mongo read paths agree.
			if c.Op == criteria.OpNin {
				v.sb.WriteString("1=1")
			} else {
				v.sb.WriteString("1=0")
			}
			return nil
		}
		ph := make([]string, len(c.Values))
		for i, val := range c.Values {
			p, err := v.operand(c.Field, val)
			if err != nil {
				return err
			}
			ph[i] = p
		}
		v.sb.WriteString(col)
		if c.Op == criteria.OpNin {
			v.sb.WriteString(" NOT IN (")
		} else {
			v.sb.WriteString(" IN (")
		}
		v.sb.WriteString(strings.Join(ph, ", "))
		v.sb.WriteByte(')')
	default:
		if len(c.Values) != 1 {
			return fmt.Errorf("criteria: operator %q on %q requires exactly one value, got %d", c.Op, c.Field, len(c.Values))
		}
		rhs, err := v.operand(c.Field, c.Values[0])
		if err != nil {
			return err
		}
		if c.Op == criteria.OpILike {
			// Case-insensitive LIKE is dialect-specific and rendered as a whole
			// clause: native ILIKE on Postgres, LOWER(col) LIKE LOWER(?) on MySQL
			// (case-insensitive on any collation).
			v.sb.WriteString(v.dialect.ILikeClause(col, rhs))
			break
		}
		if c.Op == criteria.OpLike {
			// Case-SENSITIVE LIKE is dialect-specific too: a bare LIKE is only
			// reliably case-sensitive on PG/Oracle, so MySQL/SQL Server force
			// byte-exact comparison via Dialect.LikeClause. Rendered as a whole
			// clause like ILike (not a plain binary operator).
			v.sb.WriteString(v.dialect.LikeClause(col, rhs))
			break
		}
		op, ok := binaryOps[c.Op]
		if !ok {
			return fmt.Errorf("criteria: unsupported operator %q", c.Op)
		}
		v.sb.WriteString(col)
		v.sb.WriteByte(' ')
		v.sb.WriteString(op)
		v.sb.WriteByte(' ')
		v.sb.WriteString(rhs)
	}
	return nil
}

var binaryOps = map[criteria.Operator]string{
	criteria.OpEq:  "=",
	criteria.OpNe:  "<>",
	criteria.OpGt:  ">",
	criteria.OpGte: ">=",
	criteria.OpLt:  "<",
	criteria.OpLte: "<=",
	// OpLike and OpILike are NOT here — each renders as a whole clause via
	// Dialect.LikeClause / Dialect.ILikeClause (a bare LIKE is not reliably
	// case-sensitive across engines; a bare ILIKE is Postgres-only).
}

func (v *sqlVisitor) VisitLogical(l criteria.Logical) error {
	if len(l.Operands) == 0 {
		return fmt.Errorf("criteria: %s with no operands", l.Op)
	}
	joiner := " AND "
	if l.Op == criteria.LogicalOr {
		joiner = " OR "
	}
	v.sb.WriteByte('(')
	for i, op := range l.Operands {
		if i > 0 {
			v.sb.WriteString(joiner)
		}
		if err := op.Accept(v); err != nil {
			return err
		}
	}
	v.sb.WriteByte(')')
	return nil
}

func (v *sqlVisitor) VisitNot(n criteria.Negation) error {
	if n.Inner == nil {
		return fmt.Errorf("criteria: NOT with no inner expression")
	}
	v.sb.WriteString("NOT (")
	if err := n.Inner.Accept(v); err != nil {
		return err
	}
	v.sb.WriteByte(')')
	return nil
}

// ─── Subqueries ─────────────────────────────────────────────────────────────

// subAliasBudget is how long a derived subquery alias may get before it is
// hashed instead — the same budget, for the same reason, as the read joins'
// (MySQL caps an identifier at 64 characters, the others at 128).
const subAliasBudget = 48

// subAlias is the alias a subquery's source table is rendered under.
//
// A subquery's source is ALWAYS aliased, never written bare. The alternative —
// aliasing only on a detected collision — needs to know every table already in
// scope, which the translator does not: a statement hands it a resolver, not its
// FROM. Aliasing unconditionally makes self-correlation (a subquery over the
// same table as the statement) correct by construction, and makes it impossible
// for an inner column name to drift out and silently bind to an outer table.
func subAlias(table string, depth int) string {
	name := table + "_sq" + strconv.Itoa(depth)
	if len(name) <= subAliasBudget {
		return name
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(table))
	return "sq" + strconv.Itoa(depth) + "_" + strconv.FormatUint(h.Sum64(), 16)
}

// anchorOnlyResolver is the field resolution a subquery gets: its source's OWN
// row, and nothing else.
//
// TableSchema.Resolve deliberately reaches past the anchor into the siblings and
// the shared base, because a statement that resolves through it carries those
// tables in its FROM as 1:1 LEFT JOINs. A subquery's FROM is ONE table, so a
// field resolving onto a satellite would be qualified by a table that is not
// there. Refusing it names the fix (make that table the subquery's own source,
// or nest an Exists over it) instead of emitting SQL that cannot run.
func anchorOnlyResolver(s *TableSchema) FieldResolver {
	return func(goField string) (ResolvedField, bool) {
		rf, ok := s.Resolve(goField)
		if !ok || rf.Owner != OwnerAnchor {
			return ResolvedField{}, false
		}
		return rf, true
	}
}

func (v *sqlVisitor) VisitSubquery(c criteria.SubqueryComparison) error {
	col, err := v.column(c.Field)
	if err != nil {
		return err
	}
	op, ok := subqueryOps[c.Op]
	if !ok {
		return fmt.Errorf(
			"criteria: operator %q does not take a subquery — the set forms are InSub/NinSub and "+
				"the scalar ones EqSub/NeSub/GtSub/GteSub/LtSub/LteSub", c.Op)
	}
	if c.Op == criteria.OpNin {
		if err := v.refuseNotInOnNullable(c); err != nil {
			return err
		}
	}
	sub, err := v.renderSub(c.Sub, false)
	if err != nil {
		return err
	}
	v.sb.WriteString(col)
	v.sb.WriteByte(' ')
	v.sb.WriteString(op)
	v.sb.WriteByte(' ')
	switch c.Sub.Quant {
	case criteria.QuantAny:
		v.sb.WriteString("ANY ")
	case criteria.QuantAll:
		v.sb.WriteString("ALL ")
	}
	v.sb.WriteByte('(')
	v.sb.WriteString(sub)
	v.sb.WriteByte(')')
	return nil
}

// subqueryOps are the operators a subquery may sit on the right of. LIKE/ILIKE
// and the null probes are absent on purpose: a pattern match against a row set
// is not a thing, and IS NULL takes no operand at all.
var subqueryOps = map[criteria.Operator]string{
	criteria.OpIn:  "IN",
	criteria.OpNin: "NOT IN",
	criteria.OpEq:  "=",
	criteria.OpNe:  "<>",
	criteria.OpGt:  ">",
	criteria.OpGte: ">=",
	criteria.OpLt:  "<",
	criteria.OpLte: "<=",
}

// refuseNotInOnNullable rejects NOT IN over a subquery whose projected column
// can be NULL.
//
// SQL's NOT IN returns NO ROWS AT ALL when the set contains a single NULL — not
// an error, not a warning, just a silently empty result. It is the one trap in
// this family a developer cannot see in the query they wrote, so it is refused
// rather than documented, and the message names the safe form.
func (v *sqlVisitor) refuseNotInOnNullable(c criteria.SubqueryComparison) error {
	if c.Sub == nil {
		return nil
	}
	schema, ok := c.Sub.Src.(*TableSchema)
	if !ok || c.Sub.Field == "" {
		return nil
	}
	nullable, known := schema.fieldIsNullable(c.Sub.Field)
	if !known || !nullable {
		return nil
	}
	return fmt.Errorf(
		"criteria: NinSub(%q, Sub(%s).Select(%q)) — %q is nullable, and SQL's NOT IN matches NO "+
			"rows at all when the subquery returns a single NULL, silently. Use "+
			"NotExists(Sub(%s).Where(...)) instead, which says the same thing safely",
		c.Field, schema.Table(), c.Sub.Field, c.Sub.Field, schema.Table())
}

func (v *sqlVisitor) VisitExistence(e criteria.Existence) error {
	sub, err := v.renderSub(e.Sub, true)
	if err != nil {
		return err
	}
	if e.Negated {
		v.sb.WriteString("NOT ")
	}
	v.sb.WriteString("EXISTS (")
	v.sb.WriteString(sub)
	v.sb.WriteByte(')')
	return nil
}

// renderSub compiles one nested SELECT and folds its bound arguments into this
// scope's, in emission order — which is what keeps a positional dialect's
// numbering correct with no change to any Compile* signature: the subquery emits
// one contiguous run of placeholders, so continuing from base+len(args) is
// exact.
//
// existence selects nothing (EXISTS asks whether a row is there, so the item is
// the literal 1); every other form projects exactly one item.
func (v *sqlVisitor) renderSub(sq *criteria.SubQuery, existence bool) (string, error) {
	if sq == nil {
		return "", fmt.Errorf("criteria: a subquery node was built with no Sub(...)")
	}
	if sq.Src == nil {
		return "", fmt.Errorf("criteria: Sub(nil) — a subquery needs the schema it reads from")
	}
	schema, ok := sq.Src.(*TableSchema)
	if !ok {
		return "", fmt.Errorf(
			"criteria: Sub(%T) — this backend reads a *core.TableSchema (an entity schema, an "+
				"aggregate child, a sibling, a shared base or a Direct schema)", sq.Src)
	}
	if schema.isExternal() {
		return "", fmt.Errorf(
			"criteria: Sub(%s) is an external schema — its columns belong to an UPSTREAM service "+
				"and describe a Mongo view source, so there is no such table on this connection",
			schema.Table())
	}
	if v.writeTarget != "" && !v.dialect.AllowsSubqueryOnWriteTarget() && schema.Table() == v.writeTarget {
		return "", fmt.Errorf(
			"criteria: a subquery inside an UPDATE/DELETE cannot read %q, the table the statement "+
				"is writing — the selected engine forbids it (MySQL error 1093). Read the ids in "+
				"their own statement first, or point the subquery at another table",
			schema.Table())
	}
	item, err := v.subItem(sq, schema, existence)
	if err != nil {
		return "", err
	}

	alias := v.dialect.QuoteIdent(subAlias(schema.Table(), v.depth+1))
	inner := &sqlVisitor{
		resolve:     anchorOnlyResolver(schema),
		dialect:     v.dialect,
		idKind:      schema.IDKindOf,
		base:        v.base + len(v.args),
		parent:      v,
		selfQual:    alias,
		depth:       v.depth + 1,
		writeTarget: v.writeTarget,
		srcSchema:   schema,
	}

	var where string
	if sq.Predicate != nil {
		if err := sq.Predicate.Accept(inner); err != nil {
			return "", err
		}
		where = inner.sb.String()
	}
	gate := ScopeGate(sq.Scope, schema, v.dialect, alias)

	order, err := inner.subOrder(sq)
	if err != nil {
		return "", err
	}
	if sq.LimitN > 0 && order == "" {
		return "", fmt.Errorf(
			"criteria: Sub(%s).Limit(%d) with no OrderBy — the first n rows of an unordered set "+
				"are undefined", schema.Table(), sq.LimitN)
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(item)
	sb.WriteString(" FROM ")
	sb.WriteString(v.dialect.QuoteIdent(schema.Table()))
	sb.WriteByte(' ')
	sb.WriteString(alias)
	if clause := BuildWhereClause(where, gate); clause != "" {
		sb.WriteByte(' ')
		sb.WriteString(clause)
	}
	if order != "" {
		sb.WriteByte(' ')
		sb.WriteString(order)
	}
	sql := sb.String()
	if sq.LimitN > 0 {
		sql = v.dialect.ApplyLimit(sql, int(sq.LimitN))
	}

	v.args = append(v.args, inner.args...)
	return sql, nil
}

// subItem renders what the subquery projects, and refuses the two ways the
// projection can be wrong: absent, or asked for more than once.
func (v *sqlVisitor) subItem(sq *criteria.SubQuery, schema *TableSchema, existence bool) (string, error) {
	if existence {
		if sq.Selects() != 0 {
			return "", fmt.Errorf(
				"criteria: Exists(Sub(%s)...) projects nothing — drop the Select, the question is "+
					"whether a row is there", schema.Table())
		}
		return "1", nil
	}
	switch {
	case sq.Selects() == 0:
		return "", fmt.Errorf(
			"criteria: Sub(%s) has no projected item — a subquery on the right of a comparison "+
				"needs Select(goField) or one of the Select<Aggregate> forms", schema.Table())
	case sq.Selects() > 1:
		return "", fmt.Errorf(
			"criteria: Sub(%s) projects %d items — a subquery compares one column, so exactly one "+
				"Select is allowed", schema.Table(), sq.Selects())
	}
	if sq.Func == criteria.AggCount {
		return "COUNT(*)", nil
	}
	rf, ok := anchorOnlyResolver(schema)(sq.Field)
	if !ok {
		return "", subFieldError(schema, sq.Field, "Select")
	}
	col := v.dialect.QuoteIdent(subAlias(schema.Table(), v.depth+1)) + "." + v.dialect.QuoteIdent(rf.Column)
	if sq.Func == criteria.AggNone {
		return col, nil
	}
	return string(sq.Func) + "(" + col + ")", nil
}

// subOrder renders the subquery's ORDER BY, resolved and qualified in the
// subquery's own scope.
func (v *sqlVisitor) subOrder(sq *criteria.SubQuery) (string, error) {
	if len(sq.Order) == 0 {
		return "", nil
	}
	parts := make([]string, len(sq.Order))
	for i, o := range sq.Order {
		col, err := v.column(o.Field)
		if err != nil {
			return "", fmt.Errorf("criteria: subquery order field %q does not resolve on its source", o.Field)
		}
		if o.Desc {
			parts[i] = col + " DESC"
		} else {
			parts[i] = col + " ASC"
		}
	}
	return "ORDER BY " + strings.Join(parts, ", "), nil
}

// subFieldError explains a name that does not resolve INSIDE a subquery,
// separating "no such field" from the satellite case, which is the one a
// developer will hit by accident: the same name resolves fine in the enclosing
// statement, because that statement's FROM carries the satellite and the
// subquery's does not.
func subFieldError(schema *TableSchema, goField, where string) error {
	if rf, ok := schema.Resolve(goField); ok && rf.Owner != OwnerAnchor {
		return fmt.Errorf(
			"criteria: %s(%q) inside Sub(%s) resolves onto %q, which a subquery does not have in "+
				"its FROM — a subquery reads ONE table. Make that table the subquery's own source, "+
				"or nest an Exists over it",
			where, goField, schema.Table(), rf.Schema.Table())
	}
	return fmt.Errorf(
		"criteria: %s(%q) inside Sub(%s) — not a persisted field of that table", where, goField, schema.Table())
}

// CompileWhere renders the predicate into a SQL fragment + ordered args. A nil
// predicate yields an empty fragment (no WHERE).
func CompileWhere(e criteria.Expr, resolve FieldResolver, dialect Dialect, idKind func(string) IDKind) (string, []any, error) {
	return CompileWhereQualified(e, resolve, dialect, idKind, ColQual{})
}

// CompileWhereQualified is CompileWhere with the qualification the statement's
// FROM demands — every anchor-side column under a declared read join, the anchor
// id alone under a 1:1 sibling/shared-base LEFT JOIN. A zero ColQual makes it
// identical to CompileWhere.
func CompileWhereQualified(e criteria.Expr, resolve FieldResolver, dialect Dialect, idKind func(string) IDKind, qual ColQual) (string, []any, error) {
	return CompileWhereQualifiedFrom(e, resolve, dialect, idKind, qual, 0)
}

// CompileWhereQualifiedFrom is CompileWhereQualified with the placeholder
// numbering continuing AFTER base arguments the statement already bound.
//
// A SELECT binds nothing before its WHERE, so it passes 0 and the numbering
// starts at 1 — every read goes through that form. An UPDATE binds its SET list
// first: on a positional dialect ($1, $2, …) a WHERE that restarted at 1 would
// reuse the SET's placeholders and bind the wrong values, silently. The returned
// args are the WHERE's own, in order, to be appended after the caller's.
func CompileWhereQualifiedFrom(e criteria.Expr, resolve FieldResolver, dialect Dialect, idKind func(string) IDKind, qual ColQual, base int) (string, []any, error) {
	if e == nil {
		return "", nil, nil
	}
	v := &sqlVisitor{resolve: resolve, dialect: dialect, idKind: idKind, qual: qual, base: base}
	if err := e.Accept(v); err != nil {
		return "", nil, err
	}
	return v.sb.String(), v.args, nil
}

// CompileWhereForWrite is CompileWhereQualifiedFrom for the predicate of an
// UPDATE or a DELETE, which needs one thing a read does not: the table the
// statement is modifying.
//
// It is there for a single engine-specific refusal. A subquery in a write
// predicate is ordinary SQL on three of the four backends; MySQL forbids one
// that reads the statement's own target table (error 1093), and forbids nothing
// else. Knowing the target is what lets the translator refuse EXACTLY that —
// neither shrinking the capability on the engines that have it, nor letting the
// one that does not fail at runtime.
func CompileWhereForWrite(e criteria.Expr, resolve FieldResolver, dialect Dialect, idKind func(string) IDKind, base int, targetTable string) (string, []any, error) {
	if e == nil {
		return "", nil, nil
	}
	v := &sqlVisitor{resolve: resolve, dialect: dialect, idKind: idKind, base: base, writeTarget: targetTable}
	if err := e.Accept(v); err != nil {
		return "", nil, err
	}
	return v.sb.String(), v.args, nil
}

// ScopeGate returns the DeletedAt condition for the scope on the source's
// resolved DeletedAt column ("" = no gate). A source with DeletedAt
// disabled has no marker column, so every scope yields no gate.
// qualifier is the table-qualified prefix (already quoted) to prepend to the
// DeletedAt column, or "" to leave it bare. It MUST be non-empty when the
// query JOINs another archivable table (a role's SharedBase, whose own
// deleted_at would otherwise make the bare column reference ambiguous), matching
// how the leading ID is qualified under the same joins.
func ScopeGate(s criteria.Scope, schema *TableSchema, dialect Dialect, qualifier string) string {
	col, ok := schema.DeletedAtColumn()
	if !ok {
		return ""
	}
	qcol := dialect.QuoteIdent(col)
	if qualifier != "" {
		qcol = qualifier + "." + qcol
	}
	switch s {
	case criteria.ScopeOnlyArchived:
		return qcol + " IS NOT NULL"
	case criteria.ScopeIncludeArchived:
		return ""
	default:
		return qcol + " IS NULL"
	}
}

// ChildScopeFilter maps the scope to the trailing child filter clause on the
// child source's DeletedAt column: active children are gated on
// <col> IS NULL; under any archived scope children load unfiltered so the
// unarchive cascade sees every child via AllAggregateItems(). A child with
// DeletedAt disabled is never gated.
// qualifier follows the same rule as ScopeGate's: pass the (quoted) owning table
// when the child query JOINs another archivable table — as the base-child
// loader does (base child JOINed to the role, both carrying deleted_at) — and ""
// for a single-table child SELECT where the bare column is unambiguous.
func ChildScopeFilter(s criteria.Scope, schema *TableSchema, dialect Dialect, qualifier string) string {
	col, ok := schema.DeletedAtColumn()
	if !ok {
		return ""
	}
	if s == criteria.ScopeActive {
		qcol := dialect.QuoteIdent(col)
		if qualifier != "" {
			qcol = qualifier + "." + qcol
		}
		return "AND " + qcol + " IS NULL"
	}
	return ""
}

// CompileOrder renders the ORDER BY clause ("" when no order). Each field is
// resolved + validated like the predicate columns.
func CompileOrder(order []criteria.OrderField, resolve FieldResolver, dialect Dialect) (string, error) {
	return CompileOrderQualified(order, resolve, dialect, ColQual{})
}

// CompileOrderQualified is CompileOrder with the qualification the statement's
// FROM demands (see CompileWhereQualified) — so an ORDER BY on a column the
// joined aggregate also has, and the id tiebreak under a 1:1 join, are both
// unambiguous. A zero ColQual makes it identical to CompileOrder.
func CompileOrderQualified(order []criteria.OrderField, resolve FieldResolver, dialect Dialect, qual ColQual) (string, error) {
	if len(order) == 0 {
		return "", nil
	}
	parts := make([]string, len(order))
	for i, o := range order {
		rf, ok := resolve(o.Field)
		if !ok {
			return "", fmt.Errorf("criteria: unknown order field %q", o.Field)
		}
		col := QualifyCol(rf, qual, dialect)
		if o.Desc {
			parts[i] = col + " DESC"
		} else {
			parts[i] = col + " ASC"
		}
	}
	return "ORDER BY " + strings.Join(parts, ", "), nil
}

// BuildWhereClause joins the predicate fragment and the scope gate with AND,
// prefixing "WHERE " when anything is present.
func BuildWhereClause(where, gate string) string {
	parts := make([]string, 0, 2)
	if where != "" {
		parts = append(parts, where)
	}
	if gate != "" {
		parts = append(parts, gate)
	}
	if len(parts) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(parts, " AND ")
}
