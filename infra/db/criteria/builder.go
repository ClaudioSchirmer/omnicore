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
