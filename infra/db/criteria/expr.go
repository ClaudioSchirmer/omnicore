// Package criteria is the framework's backend-neutral query DSL for loading
// live domain aggregates from the authoritative store.
//
// It is a pure mechanism: zero IO, zero SQL, zero knowledge of any entity.
// The developer composes an Expr tree (via the builder functions Eq/In/And/
// Or/Not/…) wrapped in a Query (where + order + limit + archived scope), and
// hands it to an infra repository's FindOne/FindAll. The translation of the
// tree into a concrete backend (PostgreSQL today) lives in the infra layer
// behind the Visitor seam — the criteria package never emits SQL.
//
// Dependency posture: stdlib only. Imported by infra (the framework's
// translator + a consumer's own infra repository implementations). It is NOT
// imported by domain or application — the repository interfaces those layers
// declare stay in business vocabulary (FindByID, FindByEmail, …) and the
// infra implementation is the only place that consumes this engine.
package criteria

// Operator is a leaf comparison operator.
type Operator string

const (
	OpEq      Operator = "eq"      // field = value
	OpNe      Operator = "ne"      // field <> value
	OpIn      Operator = "in"      // field IN (values…)
	OpNin     Operator = "nin"     // field NOT IN (values…)
	OpGt      Operator = "gt"      // field > value
	OpGte     Operator = "gte"     // field >= value
	OpLt      Operator = "lt"      // field < value
	OpLte     Operator = "lte"     // field <= value
	OpLike    Operator = "like"    // field LIKE pattern (case-sensitive; caller supplies % / _)
	OpILike   Operator = "ilike"   // field ILIKE pattern (case-insensitive)
	OpIsNull  Operator = "isnull"  // field IS NULL
	OpNotNull Operator = "notnull" // field IS NOT NULL
)

// LogicalOp combines sub-expressions.
type LogicalOp string

const (
	LogicalAnd LogicalOp = "and"
	LogicalOr  LogicalOp = "or"
)

// Expr is the sealed criterion tree. Only this package produces nodes (the
// unexported isExpr method seals the interface), mirroring the codebase's
// ValidEntity / Notification idiom. Accept drives the double-dispatch walk a
// backend Visitor consumes.
type Expr interface {
	isExpr()
	Accept(Visitor) error
}

// Comparison is a leaf. Field is the GO FIELD NAME ("Email"), never a column —
// resolution to the column happens in the translator. Values length: 1 for
// eq/ne/gt/gte/lt/lte/like/ilike, N for in/nin, 0 for isnull/notnull. The
// fields are exported so a backend Visitor can inspect them; cardinality is
// validated by the Visitor at compile time.
type Comparison struct {
	Field  string
	Op     Operator
	Values []any
}

func (Comparison) isExpr()                  {}
func (c Comparison) Accept(v Visitor) error { return v.VisitComparison(c) }

// Logical is an n-ary AND/OR node.
type Logical struct {
	Op       LogicalOp
	Operands []Expr
}

func (Logical) isExpr()                  {}
func (l Logical) Accept(v Visitor) error { return v.VisitLogical(l) }

// Negation negates the inner expression. (The builder for it is Not().)
type Negation struct{ Inner Expr }

func (Negation) isExpr()                  {}
func (n Negation) Accept(v Visitor) error { return v.VisitNot(n) }

// ─── Subqueries ─────────────────────────────────────────────────────────────
//
// A subquery makes the RIGHT-HAND SIDE of a comparison something other than a
// literal. That is the whole extension: the operator set is untouched, so `eq`,
// `ne`, `gt`, `gte`, `lt`, `lte`, `in` and `nin` all take a subquery the moment
// the operand can be one. EXISTS is the only genuinely new shape, because it
// has no left-hand side at all.

// Source is the table a subquery reads FROM. It is deliberately the smallest
// interface that names one — the translator asks the concrete schema for the
// rest (column resolution, identity typing, the archive marker) once it has
// asserted to the type it understands.
//
// The interface lives here, rather than this package naming *core.TableSchema,
// because criteria is stdlib + domain by design (see the package doc): core
// imports criteria, never the reverse.
type Source interface {
	Table() string
}

// AggFunc is the aggregate a subquery projects instead of a plain column. The
// empty value means "a column, not an aggregate".
//
// There is deliberately no exact-integer / fractional split like the one the
// read package's Aggregate specs carry: that distinction exists because those
// values are SCANNED into a Go int64 or float64. A subquery's value never
// leaves SQL.
type AggFunc string

const (
	AggNone  AggFunc = ""
	AggCount AggFunc = "COUNT"
	AggMax   AggFunc = "MAX"
	AggMin   AggFunc = "MIN"
	AggSum   AggFunc = "SUM"
	AggAvg   AggFunc = "AVG"
)

// Quantifier turns a scalar comparison into a quantified one — `col > ANY (…)`
// / `col > ALL (…)`. It is a property of the SUBQUERY rather than of the
// operator so that it costs one field instead of a second family of builders.
type Quantifier uint8

const (
	QuantNone Quantifier = iota
	QuantAny
	QuantAll
)

// SubQuery is one nested SELECT: where it reads from, what it projects, what it
// filters by, and the envelope concerns (order, limit, archived scope) that the
// outer Query carries for itself.
//
// The fields are exported for the same reason Comparison's are — a backend
// Visitor inspects them — and validated by the Visitor at compile time. Build it
// with Sub(...) and the fluent setters; the zero Scope is ScopeActive, so the
// archive gate the framework applies to every other read applies here too,
// without the developer asking.
//
// The predicate is Predicate rather than Where, and the row cap LimitN rather
// than Limit, because the fluent setters own those two names — a struct cannot
// carry a field and a method spelled alike, and the BUILDER is what a developer
// types.
type SubQuery struct {
	Src       Source
	Func      AggFunc
	Field     string
	Predicate Expr
	Order     []OrderField
	LimitN    int64
	Scope     Scope
	Quant     Quantifier

	// selects counts the Select/Select<Agg> calls. A subquery projects exactly
	// one item, and the counter is what lets the translator tell "none" from
	// "more than one" instead of silently keeping the last.
	selects int
}

// Selects reports how many projection calls the builder recorded. The
// translator reads it to refuse a subquery with no projected item, or with more
// than one, rather than guessing which was meant.
func (s *SubQuery) Selects() int {
	if s == nil {
		return 0
	}
	return s.selects
}

// OuterRef is a reference to a field of the ENCLOSING statement, usable
// anywhere a literal value is inside a subquery's Where — which is what makes
// correlation cost no new builders: Eq, In, Gte, Between and Contains all take
// `any`, so they all correlate for free.
//
// It reaches exactly ONE level, the immediately enclosing scope. SQL itself
// would resolve an unqualified name against the innermost scope that happens to
// have it, silently picking a different table when two levels both declare the
// field; this module declares names instead of inferring them, so a reference
// that does not resolve one level up is refused rather than searched for
// further out.
type OuterRef struct{ Field string }

// SubqueryComparison compares a field against a subquery. Op is the SAME
// operator set the literal comparisons use: eq/ne/gt/gte/lt/lte take a scalar
// subquery, in/nin a set one.
type SubqueryComparison struct {
	Field string
	Op    Operator
	Sub   *SubQuery
}

func (SubqueryComparison) isExpr()                  {}
func (c SubqueryComparison) Accept(v Visitor) error { return v.VisitSubquery(c) }

// Existence is EXISTS / NOT EXISTS. It has no field and its subquery projects
// nothing — the question is whether a row is there, so the translator emits
// `SELECT 1`.
type Existence struct {
	Negated bool
	Sub     *SubQuery
}

func (Existence) isExpr()                  {}
func (e Existence) Accept(v Visitor) error { return v.VisitExistence(e) }

// Visitor is the per-backend translation seam. The tree-structure walk
// (precedence, grouping of AND/OR/NOT) is universal and driven by Accept;
// each backend implements the three Visit methods to accumulate into its own
// native shape — PostgreSQL into a WHERE string + args; a future Mongo
// backend into a bson document. A single string-emitting "dialect" interface
// is deliberately NOT used because SQL and document stores diverge
// structurally; the neutral contract is the IR, not the rendered fragment.
//
// The subquery nodes are members of this same sealed algebra, so they are
// members of this same interface — deliberately, and at the cost of every
// implementation having to grow with it. A Visitor over a sealed sum earns its
// keep only while the COMPILER proves that every backend handles every node; an
// optional second interface would cost nothing today and cost exactly that
// property forever.
type Visitor interface {
	VisitComparison(Comparison) error
	VisitLogical(Logical) error
	VisitNot(Negation) error
	VisitSubquery(SubqueryComparison) error
	VisitExistence(Existence) error
}
