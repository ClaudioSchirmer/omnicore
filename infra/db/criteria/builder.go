package criteria

import (
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ─── Leaf builders ──────────────────────────────────────────────────────────
//
// field is always the Go field name on the aggregate root (e.g. "Email"); the
// translator resolves it to a column. value(s) are bound as parameters, never
// concatenated into SQL.

func Eq(field string, v any) Expr  { return Comparison{Field: field, Op: OpEq, Values: []any{v}} }
func Ne(field string, v any) Expr  { return Comparison{Field: field, Op: OpNe, Values: []any{v}} }
func Gt(field string, v any) Expr  { return Comparison{Field: field, Op: OpGt, Values: []any{v}} }
func Gte(field string, v any) Expr { return Comparison{Field: field, Op: OpGte, Values: []any{v}} }
func Lt(field string, v any) Expr  { return Comparison{Field: field, Op: OpLt, Values: []any{v}} }
func Lte(field string, v any) Expr { return Comparison{Field: field, Op: OpLte, Values: []any{v}} }

// In matches when the field equals any of the values. Zero values is rejected
// by the translator at compile time (no `IN ()`).
func In(field string, vs ...any) Expr  { return Comparison{Field: field, Op: OpIn, Values: vs} }
func Nin(field string, vs ...any) Expr { return Comparison{Field: field, Op: OpNin, Values: vs} }

// Like / ILike take the raw pattern — the caller controls the % / _ wildcards.
func Like(field, pattern string) Expr {
	return Comparison{Field: field, Op: OpLike, Values: []any{pattern}}
}
func ILike(field, pattern string) Expr {
	return Comparison{Field: field, Op: OpILike, Values: []any{pattern}}
}

func IsNull(field string) Expr  { return Comparison{Field: field, Op: OpIsNull} }
func NotNull(field string) Expr { return Comparison{Field: field, Op: OpNotNull} }

// ─── Convenience sugar (case-insensitive substring/prefix/suffix) ────────────
//
// The literal is escaped so LIKE metacharacters in the caller's value match
// verbatim — Contains("Name", "50%") matches the literal "50%", not "starts
// with 50". Backslash is the escape char (PostgreSQL LIKE default).

func Contains(field, s string) Expr   { return ILike(field, "%"+EscapeLike(s)+"%") }
func StartsWith(field, s string) Expr { return ILike(field, EscapeLike(s)+"%") }
func EndsWith(field, s string) Expr   { return ILike(field, "%"+EscapeLike(s)) }

// Between is sugar for Gte(lo) AND Lte(hi) on the same field (inclusive).
func Between(field string, lo, hi any) Expr { return And(Gte(field, lo), Lte(field, hi)) }

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike escapes the LIKE metacharacters (%, _ and the backslash escape
// char) in a raw value so it matches VERBATIM inside a Like / ILike pattern the
// caller assembles — EscapeLike("50%")+"%" is a prefix match on the literal
// "50%", not "starts with 50 then anything". Exposed so a reader that renders a
// neutral text predicate (prefix / substring / whole, case-sensitive or not)
// can build the Like/ILike pattern itself while reusing the single, dialect-safe
// escape (backslash, the PostgreSQL LIKE default the translator declares).
func EscapeLike(s string) string { return likeEscaper.Replace(s) }

// ─── Boolean composition ─────────────────────────────────────────────────────

func And(operands ...Expr) Expr { return Logical{Op: LogicalAnd, Operands: operands} }
func Or(operands ...Expr) Expr  { return Logical{Op: LogicalOr, Operands: operands} }
func Not(inner Expr) Expr       { return Negation{Inner: inner} }

// ─── Subquery builders ───────────────────────────────────────────────────────
//
// Sub(...) opens a nested SELECT over another table; the …Sub builders put it on
// the right-hand side of a comparison; Exists / NotExists ask only whether a row
// is there. Every refusal (no projected item, two of them, a source the backend
// cannot read, a field that does not belong to the source) is reported by the
// translator at compile time, with the offending name in the message — this
// package builds the tree and never panics.

// Sub starts a subquery over src — any schema naming a real table in this
// database. Its scope starts ACTIVE, exactly like Where's, so the framework's
// archive gate applies inside the subquery without the caller writing it.
func Sub(src Source) *SubQuery { return &SubQuery{Src: src} }

// Select projects one column, named by its GO FIELD name. Mandatory for every
// form but Exists / NotExists, which project nothing.
func (s *SubQuery) Select(goField string) *SubQuery {
	s.Func, s.Field, s.selects = AggNone, goField, s.selects+1
	return s
}

// SelectCount projects COUNT(*) — the scalar form of "how many rows match".
func (s *SubQuery) SelectCount() *SubQuery {
	s.Func, s.Field, s.selects = AggCount, "", s.selects+1
	return s
}

// SelectMax projects MAX over goField. The classic use is the newest revision:
// EqSub("Version", Sub(rev).SelectMax("Version").Where(Eq("DocID", Outer("ID")))).
func (s *SubQuery) SelectMax(goField string) *SubQuery { return s.agg(AggMax, goField) }

// SelectMin projects MIN over goField.
func (s *SubQuery) SelectMin(goField string) *SubQuery { return s.agg(AggMin, goField) }

// SelectSum projects SUM over goField.
func (s *SubQuery) SelectSum(goField string) *SubQuery { return s.agg(AggSum, goField) }

// SelectAvg projects AVG over goField.
func (s *SubQuery) SelectAvg(goField string) *SubQuery { return s.agg(AggAvg, goField) }

func (s *SubQuery) agg(fn AggFunc, goField string) *SubQuery {
	s.Func, s.Field, s.selects = fn, goField, s.selects+1
	return s
}

// Where sets the subquery's predicate. Inside it, Outer(goField) refers to the
// enclosing statement — that is how a subquery correlates.
func (s *SubQuery) Where(e Expr) *SubQuery { s.Predicate = e; return s }

// OrderBy / OrderByDesc order the subquery's rows, which only matters together
// with Limit — "the first row by this order".
func (s *SubQuery) OrderBy(goField string) *SubQuery {
	s.Order = append(s.Order, OrderField{Field: goField})
	return s
}

func (s *SubQuery) OrderByDesc(goField string) *SubQuery {
	s.Order = append(s.Order, OrderField{Field: goField, Desc: true})
	return s
}

// Limit caps the subquery's rows. It REQUIRES an order — an unordered "first
// n" is undefined, the same rule Query.Offset already enforces.
func (s *SubQuery) Limit(n int64) *SubQuery { s.LimitN = n; return s }

// IncludeArchived / OnlyArchived move the subquery off the active scope. Same
// names, same meaning, same default as on Query.
func (s *SubQuery) IncludeArchived() *SubQuery { s.Scope = ScopeIncludeArchived; return s }
func (s *SubQuery) OnlyArchived() *SubQuery    { s.Scope = ScopeOnlyArchived; return s }

// Any / All quantify a scalar comparison over EVERY row the subquery returns —
// GtSub("Price", sub.All()) is "greater than all of them".
func (s *SubQuery) Any() *SubQuery { s.Quant = QuantAny; return s }
func (s *SubQuery) All() *SubQuery { s.Quant = QuantAll; return s }

// InSub / NinSub are set membership against the subquery's rows.
//
// NinSub over a NULLABLE column is refused by the translator: SQL's NOT IN
// yields no rows at all when the set contains a single NULL, which is a silent
// wrong answer rather than an error. NotExists says the same thing safely.
func InSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpIn, Sub: q}
}
func NinSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpNin, Sub: q}
}

// EqSub / NeSub / GtSub / GteSub / LtSub / LteSub compare against a SCALAR
// subquery — one that returns a single row and a single column, unless it is
// quantified with Any / All.
func EqSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpEq, Sub: q}
}
func NeSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpNe, Sub: q}
}
func GtSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpGt, Sub: q}
}
func GteSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpGte, Sub: q}
}
func LtSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpLt, Sub: q}
}
func LteSub(goField string, q *SubQuery) Expr {
	return SubqueryComparison{Field: goField, Op: OpLte, Sub: q}
}

// Exists / NotExists ask only whether the subquery matches a row. They project
// nothing (a Select on them is refused), and they are the canonical way to
// filter by the MANY side of a 1:N relation — the pushdown a single root SELECT
// cannot otherwise express:
//
//	Exists(Sub(phoneSchema).Where(Eq("UserID", Outer("ID"))))
func Exists(q *SubQuery) Expr    { return Existence{Sub: q} }
func NotExists(q *SubQuery) Expr { return Existence{Negated: true, Sub: q} }

// Outer references a field of the ENCLOSING statement from inside a subquery's
// predicate. It is a VALUE, so every operator that takes one correlates without
// a builder of its own: Eq("RoleID", Outer("ID")).
func Outer(goField string) OuterRef { return OuterRef{Field: goField} }

// ─── Query envelope ──────────────────────────────────────────────────────────

// Scope governs the DeletedAt gate applied around the boolean predicate. It
// is NOT part of the Expr algebra — the where-tree stays DeletedAt-agnostic;
// the translator reads the scope and appends the deleted_at condition.
type Scope uint8

const (
	ScopeActive          Scope = iota // deleted_at IS NULL (default)
	ScopeIncludeArchived              // active + archived (no deleted_at gate)
	ScopeOnlyArchived                 // deleted_at IS NOT NULL
)

// OrderField is one ORDER BY term. Field is the Go field name.
type OrderField struct {
	Field string
	Desc  bool
}

// Query is the generic query the engine compiles: a boolean predicate plus the
// envelope concerns (order, limit/offset window, archived scope) that are not
// part of the predicate algebra. Build it via Where / ByID and the fluent
// setters.
type Query struct {
	where  Expr
	order  []OrderField
	limit  int64
	offset int64
	scope  Scope
}

// Where starts a query from a predicate. e may be nil — meaning "no predicate"
// (all rows under the active scope), useful with OrderBy + Limit.
func Where(e Expr) *Query { return &Query{where: e} }

// ByID is the readable shortcut for a primary-key lookup. It assumes the
// framework's fixed ID convention (Go field "ID" ↔ column "id") — the same
// convention the by-id load path has always used. The id value is bound as a
// parameter; domain.ID values are unwrapped by the translator.
func ByID(id domain.ID) *Query { return Where(Eq("ID", id)) }

func (q *Query) OrderBy(field string) *Query {
	q.order = append(q.order, OrderField{Field: field})
	return q
}

func (q *Query) OrderByDesc(field string) *Query {
	q.order = append(q.order, OrderField{Field: field, Desc: true})
	return q
}

// Limit caps the number of rows (0 = no limit).
func (q *Query) Limit(n int64) *Query { q.limit = n; return q }

// Offset skips the first n rows before the limited window begins — the page
// cursor for offset pagination (0 = no skip). A non-zero offset is only
// meaningful over a bounded, ordered window, so it REQUIRES both a positive
// Limit (an offset paginates a page, it is not a bare skip) and at least one
// OrderBy (a skip is undefined without a deterministic order — and SQL Server's
// OFFSET…FETCH mandates the ORDER BY outright). Violating either is a load-time
// error on FindAll, never a silently wrong page.
func (q *Query) Offset(n int64) *Query { q.offset = n; return q }

func (q *Query) IncludeArchived() *Query { q.scope = ScopeIncludeArchived; return q }
func (q *Query) OnlyArchived() *Query    { q.scope = ScopeOnlyArchived; return q }

// ─── Accessors consumed by the infra translator ─────────────────────────────

// Condition returns the boolean predicate (may be nil).
func (q *Query) Condition() Expr { return q.where }

// OrderFields returns the ORDER BY terms in declaration order.
func (q *Query) OrderFields() []OrderField { return q.order }

// LimitValue returns the row cap (0 = no limit).
func (q *Query) LimitValue() int64 { return q.limit }

// OffsetValue returns the row skip (0 = no offset).
func (q *Query) OffsetValue() int64 { return q.offset }

// Scope returns the DeletedAt scope.
func (q *Query) Scope() Scope { return q.scope }
