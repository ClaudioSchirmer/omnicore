package queries

// RegexMatch is a transport-agnostic marker that ViewReader implementations
// translate into their store's native regex match (Mongo bson.Regex, SQL
// LIKE-equivalent, etc.). The wire wrappers in `web/` emit this struct for
// the partial-match operators (`startswith`, `contains`, `istartswith`,
// `icontains`) and for the case-insensitive single-value variants (`ieq`,
// `ine`) so a single ReadCriteria.Filter shape works across stores.
//
// Pattern carries the already-escaped regular expression (web/ applies
// regexp.QuoteMeta to the user-supplied value and prepends `^` for prefix
// matches), so implementations forward it verbatim. CaseInsensitive
// activates the case-folded match (Mongo: `$options:"i"`).
//
// The struct is intentionally inert from the application layer's
// perspective — Query handlers do not consult Pattern/CaseInsensitive
// themselves; they only forward the criteria to ViewReader.ReadPage /
// ReadByID. Tests that exercise the criteria assembly assert against the
// emitted struct directly.
type RegexMatch struct {
	Pattern         string
	CaseInsensitive bool
	Negate          bool
}

// RegexMatchList is the multi-value counterpart of RegexMatch, used by the
// `iin` and `inin` operators. Patterns are already escaped by web/ and
// anchored as needed (`iin` anchors each pattern with `^...$` to preserve
// the equality semantic; `inin` does the same and negates via Negate=true).
// ViewReader implementations expand the list using the store's native
// equivalent of `$in` over regex values (Mongo: bson.Regex elements inside
// `$in` / `$nin`).
type RegexMatchList struct {
	Patterns        []string
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
// a scalar for `eq`, a `map[string]any{"$op": ...}` sub-document for the
// operator variants, or one of the RegexMatch / RegexMatchList sentinels.
// The struct is inert from the application layer's perspective — Query
// handlers do not consult Clauses themselves; they only forward the
// criteria to ViewReader.ReadPage / ReadByID. Tests that exercise the
// criteria assembly assert against the emitted struct directly.
type MultiClause struct {
	Clauses []any
}
