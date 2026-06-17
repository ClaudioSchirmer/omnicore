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
	OpEq      Operator = "eq"     // field = value
	OpNe      Operator = "ne"     // field <> value
	OpIn      Operator = "in"     // field IN (values…)
	OpNin     Operator = "nin"    // field NOT IN (values…)
	OpGt      Operator = "gt"     // field > value
	OpGte     Operator = "gte"    // field >= value
	OpLt      Operator = "lt"     // field < value
	OpLte     Operator = "lte"    // field <= value
	OpLike    Operator = "like"   // field LIKE pattern (case-sensitive; caller supplies % / _)
	OpILike   Operator = "ilike"  // field ILIKE pattern (case-insensitive)
	OpIsNull  Operator = "isnull" // field IS NULL
	OpNotNull Operator = "notnull"// field IS NOT NULL
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

// Visitor is the per-backend translation seam. The tree-structure walk
// (precedence, grouping of AND/OR/NOT) is universal and driven by Accept;
// each backend implements the three Visit methods to accumulate into its own
// native shape — PostgreSQL into a WHERE string + args; a future Mongo
// backend into a bson document. A single string-emitting "dialect" interface
// is deliberately NOT used because SQL and document stores diverge
// structurally; the neutral contract is the IR, not the rendered fragment.
type Visitor interface {
	VisitComparison(Comparison) error
	VisitLogical(Logical) error
	VisitNot(Negation) error
}
