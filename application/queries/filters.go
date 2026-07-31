package queries

// TextMatchKind is the shape of a text predicate, independent of any store's
// pattern syntax: a prefix match, a substring match, or a whole-value match.
type TextMatchKind int

const (
	// TextPrefix matches values that START WITH Value (the `startswith` family).
	TextPrefix TextMatchKind = iota
	// TextContains matches values that CONTAIN Value anywhere (the `contains`
	// family).
	TextContains
	// TextExact matches values EQUAL to Value (the case-insensitive equality
	// operators `ieq` / `ine`).
	TextExact
)

// TextMatch is the store-neutral text predicate the wire wrappers emit for the
// partial-match operators (`startswith`, `contains`, `istartswith`, `icontains`)
// and the case-insensitive equality operators (`ieq`, `ine`). Value is the RAW
// user value — never escaped and never anchored: each ViewReader renders it for
// its own store (the Mongo reader builds an anchored, regexp.QuoteMeta'd
// bson.Regex; a relational reader builds a LIKE / ILIKE pattern), so no store's
// pattern syntax leaks through the port. Kind picks prefix / substring / whole,
// CaseInsensitive folds case, Negate inverts.
//
// The struct is inert from the application layer's perspective — Query handlers
// do not consult it; they only forward the criteria to ViewReader.ReadPage /
// ReadByID. Tests that exercise the criteria assembly assert against the emitted
// struct directly.
type TextMatch struct {
	Value           string
	Kind            TextMatchKind
	CaseInsensitive bool
	Negate          bool
}

// TextMatchList is the multi-value counterpart of TextMatch for the
// case-insensitive membership operators (`iin`, `inin`): a whole-value match
// against ANY of Values, case-folded, Negate inverting to none-of. Like
// TextMatch the values are RAW — each ViewReader anchors/escapes for its own
// store (the Mongo reader emits `$in` / `$nin` over anchored bson.Regex
// elements).
type TextMatchList struct {
	Values          []string
	CaseInsensitive bool
	Negate          bool
}

// MultiClause carries more than one operator clause for the same logical
// field. The wire wrappers in `web/` emit it when a query string declares
// multiple operators on the same field name (e.g. `?age.gte=18&age.lte=65`
// or `?name.startswith=Bob&name.icontains=ob`). ViewReader implementations
// expand it into the store's native AND construct (Mongo: every clause
// becomes a `{field: clause}` entry inside a top-level `$and` array on the
// filter document).
//
// Each element of Clauses is one of the canonical filter values the wire
// layer would otherwise have written directly to ReadCriteria.Filter[field]:
// a scalar for `eq`, a `Clause` sentinel for the ordinal/set operator
// variants (ne/gt/gte/lt/lte, in/nin), or one of the TextMatch /
// TextMatchList sentinels. The struct is inert from the application layer's
// perspective — Query handlers do not consult Clauses themselves; they only
// forward the criteria to ViewReader.ReadPage / ReadByID. Tests that exercise
// the criteria assembly assert against the emitted struct directly.
type MultiClause struct {
	Clauses []any
}

// FilterOp is the neutral comparison operator a Clause carries — the wire
// layer's ordinal and set operators lifted out of any store's native filter
// syntax. ViewReader implementations translate it to their backend
// (MongoViewReader to the `{$op: ...}` bson sub-document; the relational
// reader to a criteria comparison). It deliberately does NOT cover the
// text-match operators (startswith/contains and their case-folding variants)
// — those carry the raw value and ride TextMatch / TextMatchList instead.
type FilterOp string

const (
	FilterNe  FilterOp = "ne"
	FilterIn  FilterOp = "in"
	FilterNin FilterOp = "nin"
	FilterGt  FilterOp = "gt"
	FilterGte FilterOp = "gte"
	FilterLt  FilterOp = "lt"
	FilterLte FilterOp = "lte"
)

// Clause is the neutral comparison sentinel the wire wrappers emit for the
// ordinal and set operators (ne/gt/gte/lt/lte and in/nin) in place of a
// store-specific operator sub-document. Op names the comparison; Values
// carries the operand(s) already coerced to the leaf's Go type — a single
// element for the scalar operators, the full list for in/nin. ViewReader
// implementations translate it to their backend: MongoViewReader to the
// `{$op: ...}` bson sub-document, the relational reader to a criteria
// comparison. `eq` is deliberately NOT a Clause — an equality lands as the
// bare coerced scalar directly under the field, the shape both readers
// already treat as equality. Like the other sentinels the struct is inert
// from the application layer's perspective; Query handlers only forward it.
type Clause struct {
	Op     FilterOp
	Values []any
}
