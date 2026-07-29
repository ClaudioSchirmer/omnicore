package read

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// RootScanner deserializes one row into a populated entity T. The loader passes
// a column-keyed map — the row read BY NAME, never by position, so it is
// order-independent and stable across an online ADD COLUMN: the scanner reads
// m["column"], type-asserts, and returns the entity. On the criteria path it MUST
// set the ID (SetID) — the loader reads it back via t.GetID(). The map carries the
// schema's declared columns, values normalized per backend (uuid → string, etc.),
// so a manual scanner runs on any engine.
type RootScanner[T domain.Entity] func(map[string]any) (T, error)

// ChildScanner deserializes one column-keyed row map into an AggregateValueObject
// (read by name, same normalization as RootScanner).
type ChildScanner func(map[string]any) (domain.AggregateValueObject, error)

// AggregateLoader[T] loads an aggregate root + its children from the configured
// relational backend. The
// root and children tables/columns come from the TableSchema attached via
// WithSchema (the same schema the write side uses) — no convention inference.
//
// Two scan paths coexist:
//
//  1. Auto-scan (default) — the framework reads each declared child schema and
//     scans the columns directly into the struct. Every child declared on the
//     schema (root.Child(...)) is auto-scanned unless a manual scanner is set.
//
//  2. Manual scanners — service provides RootScanner/ChildScanner via
//     WithRootScanner/WithChildScanner. Required for non-trivial queries.
//
// Typical usage (auto-scan):
//
//	loader := infra.NewAggregateLoader[*appdomain.User](pg, func() *appdomain.User {
//	    return &appdomain.User{}
//	}).WithContextName("User").WithSchema(userSchema)
//
//	func (r *UserRepository) FindByID(id domain.ID) (*appdomain.User, error) {
//	    return loader.FindOne(context.Background(), criteria.ByID(id))
//	}
type AggregateLoader[T domain.Entity] struct {
	eng           RelationalEngine
	newEntity     func() T
	contextName   string
	schema        *TableSchema
	rootScanner   RootScanner[T]
	childScanners map[string]ChildScanner
}

// NewAggregateLoader initializes a loader over a RelationalEngine. Both scan
// paths — auto-scan and manual RootScanner/ChildScanner — run through the
// engine's neutral Querier + Dialect, so a manual scanner works on any backend
// (the scanner receives infra.Row/infra.Rows, not pgx types).
func NewAggregateLoader[T domain.Entity](eng RelationalEngine, newEntity func() T) *AggregateLoader[T] {
	return &AggregateLoader[T]{
		eng:           eng,
		newEntity:     newEntity,
		childScanners: map[string]ChildScanner{},
	}
}

// WithContextName overrides the default. When not called, contextName is
// derived from type T via TypeName[T]() (e.g. T=*User → "User"). Set only
// for a custom magic pattern (legacy schema, two Repositories over the same
// entity, etc.).
func (l *AggregateLoader[T]) WithContextName(name string) *AggregateLoader[T] {
	l.contextName = name
	return l
}

// effectiveContextName returns contextName if set; otherwise derives it from T.
func (l *AggregateLoader[T]) effectiveContextName() string {
	if l.contextName != "" {
		return l.contextName
	}
	return TypeName[T]()
}

// WithSchema attaches the repository's TableSchema (root + child schemas) so the
// loader resolves table/ID/ParentID/column from the same explicit map the write side
// uses. Shared between write and read sides of a Repository.
func (l *AggregateLoader[T]) WithSchema(schema *TableSchema) *AggregateLoader[T] {
	l.schema = schema
	return l
}

// WithRootScanner registers a manual scanner for the root. Takes precedence over auto.
func (l *AggregateLoader[T]) WithRootScanner(fn RootScanner[T]) *AggregateLoader[T] {
	l.rootScanner = fn
	return l
}

// WithChildScanner registers a manual scanner for the child. Takes precedence over auto.
// typeName must match the Go type name of the AVO (e.g. "Address").
func (l *AggregateLoader[T]) WithChildScanner(typeName string, fn ChildScanner) *AggregateLoader[T] {
	l.childScanners[typeName] = fn
	return l
}

// FindOne loads exactly one aggregate matching the criteria — root + children,
// the live domain entity ready for a command. Returns a *DomainError with
// RecordNotFoundNotification (HTTP 404) when nothing matches, and an error when
// the criteria matches more than one row (the contract is "expected one"; >1 is
// a developer mistake surfaced loudly rather than silently picking the first).
// A LIMIT 2 probe bounds the >1 check; any Limit set on the Query is overridden.
func (l *AggregateLoader[T]) FindOne(ctx context.Context, q *criteria.Query) (T, error) {
	entities, ids, err := l.findRoots(ctx, q, 2)
	if err != nil {
		return *new(T), err
	}
	if len(entities) == 0 {
		return *new(T), domain.NotFoundError(l.effectiveContextName(), "criteria", "")
	}
	if len(entities) > 1 {
		return *new(T), fmt.Errorf(
			"AggregateLoader[%s].FindOne: criteria matched more than one row — use FindAll or a more specific criterion",
			l.effectiveContextName(),
		)
	}
	if err := l.hydrateSiblings(ctx, entities, ids); err != nil {
		return *new(T), err
	}
	if err := l.hydrateSharedBase(ctx, entities, ids); err != nil {
		return *new(T), err
	}
	if err := l.hydrateChildren(ctx, entities, ids, q.Scope()); err != nil {
		return *new(T), err
	}
	if err := l.hydrateBaseChildren(ctx, entities, ids, q.Scope()); err != nil {
		return *new(T), err
	}
	return entities[0], nil
}

// FindAll loads every aggregate matching the criteria — root + children — under
// the Query's order/limit/scope. Children are loaded in one batched SELECT per
// child type (WHERE fk IN (...)) and grouped in Go, so the query count is
// 1 + (number of child types), not 1 + (number of roots). Returns an empty
// slice (not NotFound) when nothing matches.
func (l *AggregateLoader[T]) FindAll(ctx context.Context, q *criteria.Query) ([]T, error) {
	entities, ids, err := l.findRoots(ctx, q, 0)
	if err != nil {
		return nil, err
	}
	if len(entities) == 0 {
		return []T{}, nil
	}
	if err := l.hydrateSiblings(ctx, entities, ids); err != nil {
		return nil, err
	}
	if err := l.hydrateSharedBase(ctx, entities, ids); err != nil {
		return nil, err
	}
	if err := l.hydrateChildren(ctx, entities, ids, q.Scope()); err != nil {
		return nil, err
	}
	if err := l.hydrateBaseChildren(ctx, entities, ids, q.Scope()); err != nil {
		return nil, err
	}
	return entities, nil
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
func (l *AggregateLoader[T]) Exists(ctx context.Context, q *criteria.Query) (bool, error) {
	fromJoin, clause, args, err := l.compileFilter(q)
	if err != nil {
		return false, err
	}
	return l.probeExists(ctx, fromJoin+clause, args...)
}

// compileFilter renders the shared front-half of a root query — FROM (+ the
// sibling/shared-base LEFT JOINs the criteria pulled in) and the WHERE clause
// (predicate + scope gate) with its ordered args. The probe/aggregate methods
// reuse exactly the resolution and gating semantics of findRoots without its
// SELECT/scan machinery.
func (l *AggregateLoader[T]) compileFilter(q *criteria.Query) (fromJoin, clause string, args []any, err error) {
	joins := &relSpecJoins{siblings: map[string]*TableSchema{}}
	return l.compileFilterJoins(q, joins)
}

// idKindResolver reports the identity typing of a criteria field across the
// SAME resolution surface specResolver walks — the anchor schema, then its
// siblings, then the shared base. The kind is derived from the Go struct
// (TableSchema.IDKindOf — the field TYPE is the declaration), so a bare-string
// probe on a domain.ID-typed field binds in the dialect's native id form; the
// managed ID slot ("ID") is always IDValue via the anchor. A type-less shared
// base derives nothing and answers IDNone for its own fields.
func (l *AggregateLoader[T]) idKindResolver() func(string) core.IDKind {
	anchor := l.schema
	sibs := anchor.Siblings()
	base, _, hasBase := anchor.SharedBaseRef()
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
		return core.IDNone
	}
}

// compileFilterJoins is compileFilter with a caller-owned joins accumulator, so
// an aggregate method can resolve its aggregated field through the SAME joins
// (a sibling field pulls its LEFT JOIN whether it appears in the predicate or
// in the SELECT aggregate).
func (l *AggregateLoader[T]) compileFilterJoins(q *criteria.Query, joins *relSpecJoins) (fromJoin, clause string, args []any, err error) {
	if q == nil {
		q = criteria.Where(nil)
	}
	resolve := l.specResolver(joins)
	dialect := l.eng.Dialect()

	where, args, err := compileWhere(q.Condition(), resolve, dialect, l.idKindResolver())
	if err != nil {
		return "", "", nil, err
	}
	rootQualifier := ""
	if len(joins.siblings) > 0 || joins.base != nil {
		rootQualifier = dialect.QuoteIdent(l.schema.Table())
	}
	clause = buildWhereClause(where, scopeGate(q.Scope(), l.schema, dialect, rootQualifier))
	if clause != "" {
		// Leading separator so callers can append fromJoin+clause directly — a
		// dialect may render the table unquoted, and "tableWHERE" lexes as one
		// identifier.
		clause = " " + clause
	}
	fromJoin = dialect.QuoteIdent(l.schema.Table()) + l.specJoinClause(joins, dialect)
	return fromJoin, clause, args, nil
}

// findRoots compiles the criteria into a root SELECT and scans the matched
// roots, recovering each row's id (FindOne/FindAll do not know the id a priori).
// limitOverride > 0 replaces the Query's limit (FindOne uses 2).
//
// Two root-scan modes, mirroring the load contract:
//
//   - Auto-scan (default): the framework controls the SELECT, prepends `id` and
//     reads it back positionally (the root struct does not expose id as a field).
//   - Manual scanner (WithRootScanner): the developer controls the row scan via
//     `SELECT *`. Because the framework no longer injects the id (there is no
//     input id on the criteria path), the manual scanner MUST populate the id
//     (scan it and call SetID); findRoots recovers it via GetID(). An empty id
//     is a configuration error surfaced loudly.
func (l *AggregateLoader[T]) findRoots(ctx context.Context, q *criteria.Query, limitOverride int64) ([]T, []string, error) {
	sample := l.newEntity()
	table := l.schema.Table()
	// A sibling-aware resolver: anchor fields resolve as before; a field that
	// lives in a sibling resolves to the sibling's column AND records the sibling
	// so it is LEFT JOINed below. Sibling columns are unique across the node
	// (the schema's bijection), so they stay unqualified and unambiguous; only
	// the shared ID is qualified (in the JOIN ON + the SELECT leading key).
	joins := &relSpecJoins{siblings: map[string]*TableSchema{}}
	resolve := l.specResolver(joins)
	dialect := l.eng.Dialect()

	where, args, err := compileWhere(q.Condition(), resolve, dialect, l.idKindResolver())
	if err != nil {
		return nil, nil, err
	}
	orderSQL, err := compileOrder(q.OrderFields(), resolve, dialect)
	if err != nil {
		return nil, nil, err
	}
	// When the criteria pulled in a sibling/base LEFT JOIN, the root's soft-delete
	// column must be table-qualified (the base carries its own deleted_at) — the
	// same disambiguation the leading ID gets below.
	rootQualifier := ""
	if len(joins.siblings) > 0 || joins.base != nil {
		rootQualifier = dialect.QuoteIdent(table)
	}
	clause := buildWhereClause(where, scopeGate(q.Scope(), l.schema, dialect, rootQualifier))
	limit := q.LimitValue()
	offset := q.OffsetValue()
	if limitOverride > 0 {
		// An internal cap (FindOne's uniqueness probe) replaces the Query's window
		// wholesale — a "first match" lookup is single-row by definition, so a
		// caller's offset never shifts it.
		limit = limitOverride
		offset = 0
	}

	// Manual root scanner: explicit columns (never SELECT *) + dev-controlled
	// decode BY NAME; id via GetID(). Runs through the neutral Querier's QueryMaps
	// (column-keyed rows), so it works on any engine and the selected column set
	// stays stable across an online ADD COLUMN.
	if l.rootScanner != nil {
		sql := "SELECT " + selectColumns(dialect, l.schema.ReadColumns()) + " FROM " + dialect.QuoteIdent(table) + tailClause(clause, orderSQL)
		sql, err = applyWindow(dialect, sql, limit, offset, orderSQL)
		if err != nil {
			return nil, nil, err
		}
		rowMaps, err := l.eng.Querier().QueryMaps(ctx, sql, args...)
		if err != nil {
			return nil, nil, err
		}
		var (
			entities []T
			ids      []string
		)
		for _, m := range rowMaps {
			root, err := l.rootScanner(m)
			if err != nil {
				return nil, nil, err
			}
			idp := root.GetID()
			if idp == nil || idp.IsEmpty() {
				return nil, nil, fmt.Errorf(
					"AggregateLoader[%s]: a manual root scanner used with FindOne/FindAll must populate the id "+
						"(read m[\"<pk>\"] and call SetID) — the framework injects no id on the criteria path",
					l.effectiveContextName(),
				)
			}
			entities = append(entities, root)
			ids = append(ids, idp.Value())
		}
		return entities, ids, nil
	}

	// Auto-scan: SELECT <pk>, <cols> + positional scan with the ID read back.
	cols, byCol := l.schema.ScanPlan()
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf(
			"AggregateLoader[%s]: schema declares no columns — %T exposes no persisted fields. "+
				"Use WithRootScanner to provide a manual scanner",
			l.effectiveContextName(), sample,
		)
	}
	// When the criteria referenced a sibling field, LEFT JOIN those tables so the
	// WHERE/ORDER can resolve against them. The SELECT list stays anchor-only (the
	// sibling VALUES are loaded separately by hydrateSiblings); the join exists
	// only to make the sibling columns reachable for filtering. The leading ID is
	// qualified to the anchor table because the shared ID is the one ambiguous
	// column under the join.
	joinSQL := l.specJoinClause(joins, dialect)
	leadingPK := dialect.QuoteIdent(l.schema.IDColumn())
	if joinSQL != "" {
		leadingPK = dialect.QuoteIdent(table) + "." + dialect.QuoteIdent(l.schema.IDColumn())
	}
	sql := "SELECT " + leadingPK + ", " + strings.Join(quoteIdentifiers(cols, dialect), ", ") + " FROM " + dialect.QuoteIdent(table) + joinSQL
	sql += tailClause(clause, orderSQL)
	sql, err = applyWindow(dialect, sql, limit, offset, orderSQL)
	if err != nil {
		return nil, nil, err
	}
	rows, err := l.eng.Querier().Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var (
		entities []T
		ids      []string
	)
	for rows.Next() {
		root := l.newEntity()
		raw, err := core.ScanLeadingKey(rows, any(root), cols, byCol)
		if err != nil {
			return nil, nil, err
		}
		id, err := dialect.DecodeID(raw)
		if err != nil {
			return nil, nil, err
		}
		root.SetID(domain.NewID(id))
		entities = append(entities, root)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return entities, ids, nil
}

// selectColumns renders an explicit, dialect-quoted column list for a read —
// never SELECT *, so the result type stays stable across an online ADD COLUMN
// (see core.TableSchema.ReadColumns for the rationale).
func selectColumns(d Dialect, cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return strings.Join(quoted, ", ")
}

// tailClause renders the " WHERE … [ORDER BY …]" suffix shared by the
// auto-scan and manual-scanner root SELECTs (each part already validated). The
// row cap is NOT part of the tail: the caller applies it over the complete
// statement via Dialect.ApplyLimit, so each engine caps in its native position.
func tailClause(clause, orderSQL string) string {
	var sb strings.Builder
	if clause != "" {
		sb.WriteByte(' ')
		sb.WriteString(clause)
	}
	if orderSQL != "" {
		sb.WriteByte(' ')
		sb.WriteString(orderSQL)
	}
	return sb.String()
}

// applyWindow caps/windows a complete SELECT in the dialect's native position.
// With no offset it is the plain row cap (Dialect.ApplyLimit — needs no ORDER BY,
// so it also serves the unordered default listing). A non-zero offset is only
// defined over a bounded, ordered result, so it REQUIRES a positive limit (an
// offset paginates a page, not an open-ended skip) and a non-empty ORDER BY (a
// skip over arbitrary physical order is meaningless — and SQL Server's
// OFFSET…FETCH mandates the ORDER BY outright). Either violation is a loud
// error, never a silently wrong page. offset/limit <= 0 mean "unset", matching
// LimitValue/OffsetValue.
func applyWindow(d Dialect, sql string, limit, offset int64, orderSQL string) (string, error) {
	if offset > 0 {
		if limit <= 0 {
			return "", fmt.Errorf("criteria: Offset(%d) requires a positive Limit — an offset paginates a bounded page, not an open-ended skip", offset)
		}
		if orderSQL == "" {
			return "", fmt.Errorf("criteria: Offset(%d) requires an OrderBy — a row skip is undefined without a deterministic order", offset)
		}
		return d.ApplyLimitOffset(sql, int(limit), int(offset)), nil
	}
	if limit > 0 {
		return d.ApplyLimit(sql, int(limit)), nil
	}
	return sql, nil
}

// hydrateChildren loads + attaches children for the given roots. Auto-scan
// children are loaded in one batched SELECT per type (WHERE fk IN (...)) and
// grouped by ParentID; a manual ChildScanner falls back to one SELECT per root (it
// cannot expose the ParentID generically). Child rows honor the scope the same way
// the root gate does — see childScopeFilter (active → deleted_at IS NULL; any
// archived scope → unfiltered, so the unarchive cascade sees every child via
// AllAggregateItems()).
func (l *AggregateLoader[T]) hydrateChildren(ctx context.Context, entities []T, ids []string, scope criteria.Scope) error {
	if len(entities) == 0 || (len(l.childScanners) == 0 && len(l.schema.ChildSchemaNames()) == 0) {
		return nil
	}
	dialect := l.eng.Dialect()

	providersByID := make(map[string]domain.AggregateRootProvider, len(entities))
	rootIDs := make([]string, 0, len(entities))
	for i, e := range entities {
		p, ok := any(e).(domain.AggregateRootProvider)
		if !ok {
			return nil // flat entity — nothing to hydrate
		}
		providersByID[ids[i]] = p
		rootIDs = append(rootIDs, ids[i])
	}

	avosByRoot := make(map[string][]domain.AggregateValueObject, len(providersByID))

	// The schema declares the aggregate children; a child with a manual scanner
	// (WithChildScanner) uses it, every other declared child is auto-scanned
	// from its TableSchema.
	seen := map[string]bool{}
	for typeName := range l.childScanners {
		seen[typeName] = true
	}
	for _, typeName := range l.schema.ChildSchemaNames() {
		seen[typeName] = true
	}

	for typeName := range seen {
		child := l.schema.ChildSchema(typeName)
		if child == nil {
			return fmt.Errorf(
				"AggregateLoader[%s] child %q: no TableSchema declared (root.Child(...))",
				l.effectiveContextName(), typeName,
			)
		}
		childTable := child.Table()
		fkCol := child.ParentIDColumn()
		childFilter := childScopeFilter(scope, child, dialect, "")

		if manual, ok := l.childScanners[typeName]; ok {
			// Manual child scanner: explicit columns (never SELECT *) + decode BY
			// NAME via QueryMaps; the ParentID arg is dialect-encoded (text on PG,
			// BINARY(16) bytes on MySQL) just like the auto path.
			sql := fmt.Sprintf(
				"SELECT %s FROM %s WHERE %s = %s %s",
				selectColumns(dialect, child.ReadColumns()), dialect.QuoteIdent(childTable), dialect.QuoteIdent(fkCol), dialect.Placeholder(1), childFilter,
			)
			for _, id := range rootIDs {
				maps, err := l.eng.Querier().QueryMaps(ctx, sql, dialect.EncodeArg(domain.NewID(id)))
				if err != nil {
					return err
				}
				for _, m := range maps {
					avo, err := manual(m)
					if err != nil {
						return err
					}
					avosByRoot[id] = append(avosByRoot[id], avo)
				}
			}
			continue
		}

		childCols, childByCol := child.ScanPlan()
		if len(childCols) == 0 {
			return fmt.Errorf(
				"AggregateLoader[%s] child %q: schema declares no columns",
				l.effectiveContextName(), typeName,
			)
		}
		placeholders := make([]string, len(rootIDs))
		qargs := make([]any, len(rootIDs))
		for i, id := range rootIDs {
			placeholders[i] = dialect.Placeholder(i + 1)
			qargs[i] = dialect.EncodeArg(domain.NewID(id))
		}
		sql, scanCols, scanByCol := childScanSQL(child, fkCol, childCols, childByCol, placeholders, childFilter, dialect)
		rows, err := l.eng.Querier().Query(ctx, sql, qargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			vp := reflect.New(child.GoType())
			fkRaw, err := core.ScanLeadingKey(rows, vp.Interface(), scanCols, scanByCol)
			if err != nil {
				rows.Close()
				return err
			}
			fk, err := dialect.DecodeID(fkRaw)
			if err != nil {
				rows.Close()
				return err
			}
			// The ParentID is the leading key (decoded above); the child's OWN ID is a
			// non-leading column scanned straight into its struct field, so on a
			// backend that stores ids as raw bytes (MySQL BINARY(16)) the field
			// holds 16 raw bytes, not the canonical uuid. Normalize it through the
			// dialect — identity on Postgres (pgx already yields text). Every other
			// child column is a non-uuid business field by the all-ids-are-uuid model.
			if err := decodeChildPK(vp, child, dialect); err != nil {
				rows.Close()
				return err
			}
			avosByRoot[fk] = append(avosByRoot[fk], vp.Elem().Interface().(domain.AggregateValueObject))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	for id, prov := range providersByID {
		if avos := avosByRoot[id]; len(avos) > 0 {
			prov.GetAggregateRoot().AggregateConstructor(avos)
		}
	}
	return nil
}

// hydrateBaseChildren loads the shared base's NATIVE children (base-children) into
// each role aggregate as Constructor items — the read-side mirror of the write
// routing. They key on the base's deterministic id, reached by JOINing the
// base-child to the role on the shared ParentID (baseChild.fk = role.fkToBase), so the
// rows come back grouped by the ROLE id and attach to the right aggregate. Loading
// them here is what lets an UPDATE diff the person-native collection instead of
// re-inserting it (the same clobber the role's own children already avoid). No-op
// without a shared base or when the base declares no children. AggregateConstructor
// is additive per type, so this coexists with the role's own hydrated children.
func (l *AggregateLoader[T]) hydrateBaseChildren(ctx context.Context, entities []T, ids []string, scope criteria.Scope) error {
	base, fkCol, ok := l.schema.SharedBaseRef()
	if !ok {
		return nil
	}
	baseChildren := base.ChildSchemas()
	if len(baseChildren) == 0 {
		return nil
	}
	providersByID := make(map[string]domain.AggregateRootProvider, len(entities))
	rootIDs := make([]string, 0, len(entities))
	for i, e := range entities {
		p, ok := any(e).(domain.AggregateRootProvider)
		if !ok {
			return nil // flat entity — nothing to hydrate
		}
		providersByID[ids[i]] = p
		rootIDs = append(rootIDs, ids[i])
	}
	if len(rootIDs) == 0 {
		return nil
	}
	d := l.eng.Dialect()
	roleTbl := d.QuoteIdent(l.schema.Table())
	rolePK := d.QuoteIdent(l.schema.IDColumn())
	roleFK := d.QuoteIdent(fkCol)

	placeholders := make([]string, len(rootIDs))
	qargs := make([]any, len(rootIDs))
	for i, id := range rootIDs {
		placeholders[i] = d.Placeholder(i + 1)
		qargs[i] = d.EncodeArg(domain.NewID(id))
	}

	avosByRoot := make(map[string][]domain.AggregateValueObject, len(rootIDs))
	for _, bc := range baseChildren {
		bcCols, bcByCol := bc.ScanPlan()
		if len(bcCols) == 0 {
			return fmt.Errorf(
				"AggregateLoader[%s] base child %q: schema declares no columns",
				l.effectiveContextName(), bc.GoType().Name())
		}
		bcTbl := d.QuoteIdent(bc.Table())
		// SELECT role.pk (leading key → groups by aggregate) + base-child columns,
		// joining the base-child to the role on the shared base id.
		sel := roleTbl + "." + rolePK
		for _, c := range bcCols {
			sel += ", " + bcTbl + "." + d.QuoteIdent(c)
		}
		sql := "SELECT " + sel + " FROM " + bcTbl +
			" JOIN " + roleTbl + " ON " + bcTbl + "." + d.QuoteIdent(bc.ParentIDColumn()) + " = " + roleTbl + "." + roleFK +
			// bcTbl-qualified: the JOIN to the role table brings a second deleted_at
			// into scope, so the base-child's active gate must name its own table.
			" WHERE " + roleTbl + "." + rolePK + " IN (" + strings.Join(placeholders, ", ") + ") " + childScopeFilter(scope, bc, d, bcTbl)
		rows, err := l.eng.Querier().Query(ctx, sql, qargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			vp := reflect.New(bc.GoType())
			rootRaw, err := core.ScanLeadingKey(rows, vp.Interface(), bcCols, bcByCol)
			if err != nil {
				rows.Close()
				return err
			}
			rootID, err := d.DecodeID(rootRaw)
			if err != nil {
				rows.Close()
				return err
			}
			if err := decodeChildPK(vp, bc, d); err != nil {
				rows.Close()
				return err
			}
			avosByRoot[rootID] = append(avosByRoot[rootID], vp.Elem().Interface().(domain.AggregateValueObject))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	for id, prov := range providersByID {
		if avos := avosByRoot[id]; len(avos) > 0 {
			prov.GetAggregateRoot().AggregateConstructor(avos)
		}
	}
	return nil
}

// LoadSharedBaseIdentity loads the existing shared identity (base fields + native
// base-children as Constructor) by the natural key read from fresh — the load-first
// half of the SharedBase UPSERT insert. Returns the hydrated NEW entity with
// existed=true when the identity already exists; otherwise (fresh, false) for a
// cold insert. The role row does not exist yet, so it looks the base up by its
// natural-key COLUMN directly (not the role→base join the update-path hydrate uses).
func (l *AggregateLoader[T]) LoadSharedBaseIdentity(ctx context.Context, fresh T) (T, bool, error) {
	base, fkCol, ok := l.schema.SharedBaseRef()
	if !ok {
		return fresh, false, nil
	}
	nkCol := base.NaturalIDColumn()
	nkGo, ok := base.GoOf(nkCol)
	if !ok {
		return fresh, false, fmt.Errorf(
			"AggregateLoader[%s]: shared base natural key %q has no Go field", l.effectiveContextName(), nkCol)
	}
	nkVal := goStringFieldValue(fresh, nkGo)
	if nkVal == "" {
		return fresh, false, nil // no natural key → cold (the write guard rejects an empty key on persist)
	}
	cols, byCol, _ := l.schema.SharedBaseScanPlan()
	d := l.eng.Dialect()
	sel := d.QuoteIdent(base.IDColumn())
	for _, c := range cols {
		sel += ", " + d.QuoteIdent(c)
	}
	sql := d.ApplyLimit("SELECT "+sel+" FROM "+d.QuoteIdent(base.Table())+
		" WHERE "+d.QuoteIdent(nkCol)+" = "+d.Placeholder(1), 1)
	rows, err := l.eng.Querier().Query(ctx, sql, nkVal)
	if err != nil {
		return fresh, false, err
	}
	if !rows.Next() {
		errc := rows.Err()
		rows.Close()
		return fresh, false, errc // cold: no existing identity
	}
	newE := l.newEntity()
	baseIDRaw, scanErr := core.ScanLeadingKey(rows, any(newE), cols, byCol)
	rows.Close()
	if scanErr != nil {
		return fresh, false, scanErr
	}
	baseID, err := d.DecodeID(baseIDRaw)
	if err != nil {
		return fresh, false, err
	}
	// Pre-flight conflict check. This load is exclusive to the SharedBase UPSERT
	// insert, so it owns the "already exists" verdict: if an ACTIVE specialization
	// role already references this identity, a POST is a conflict — surface the
	// canonical 409 here, before the handler re-applies the request, so it is not
	// masked by a child-level validation (e.g. a re-sent address). An archived role
	// is excluded — soft-delete is delete, so the insert falls through and the
	// schema's constraints arbitrate the collision with the remnant (there is no
	// revival on POST; /unarchive is the explicit path back). The persister's
	// active-role probe + UNIQUE(fk) remain the in-TX race backstop.
	roleExists, err := l.activeRoleExists(ctx, fkCol, baseID)
	if err != nil {
		return fresh, false, err
	}
	if roleExists {
		return fresh, false, core.SingleNotificationError(
			l.effectiveContextName(), l.schema.IDColumn(), domain.EntityAlreadyAddedNotification{})
	}
	if err := l.loadBaseChildrenConstructor(ctx, newE, base, baseID); err != nil {
		return fresh, false, err
	}
	return newE, true, nil
}

// activeRoleExists reports whether a live (non-soft-deleted) specialization role
// already references the shared base id. It mirrors the write-side findRoleByFK
// active/archived split as a read-side existence probe: the SharedBase UPSERT load
// uses it to reject a re-POST of an existing active role with the canonical
// conflict, before the request is re-applied onto the loaded identity.
func (l *AggregateLoader[T]) activeRoleExists(ctx context.Context, fkCol, baseID string) (bool, error) {
	// COLUMN-level probe (the ParentID is injected by infra, not a Go field), so it
	// cannot ride the criteria-based Exists — but it shares probeExists, the
	// single home of the SELECT 1 … LIMIT 1 execution.
	d := l.eng.Dialect()
	where := " WHERE " + d.QuoteIdent(fkCol) + " = " + d.Placeholder(1)
	if sd, ok := l.schema.SoftDeleteColumn(); ok {
		where += " AND " + d.QuoteIdent(sd) + " IS NULL"
	}
	return l.probeExists(ctx, d.QuoteIdent(l.schema.Table())+where, d.EncodeArg(domain.NewID(baseID)))
}

// probeExists executes the shared existence probe — SELECT 1 over the given
// FROM/WHERE tail, capped at one row via the dialect, true when any row comes
// back. The single execution home for the public criteria-level Exists and the
// internal column-level probes.
func (l *AggregateLoader[T]) probeExists(ctx context.Context, fromWhere string, args ...any) (bool, error) {
	rows, err := l.eng.Querier().Query(ctx, l.eng.Dialect().ApplyLimit("SELECT 1 FROM "+fromWhere, 1), args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
}

// loadBaseChildrenConstructor loads the shared base's native children by the base
// id and attaches them to newE as Constructor items, so the command's ApplyTo can
// dedup the request against them. Keyed directly by the base id (the role does not
// exist yet); the base-child ParentID is the leading key (discarded), reusing ScanLeadingKey.
func (l *AggregateLoader[T]) loadBaseChildrenConstructor(ctx context.Context, newE T, base *TableSchema, baseID string) error {
	baseChildren := base.ChildSchemas()
	if len(baseChildren) == 0 {
		return nil
	}
	prov, ok := any(newE).(domain.AggregateRootProvider)
	if !ok {
		return nil
	}
	d := l.eng.Dialect()
	var avos []domain.AggregateValueObject
	for _, bc := range baseChildren {
		bcCols, bcByCol := bc.ScanPlan()
		if len(bcCols) == 0 {
			continue
		}
		bcFK := d.QuoteIdent(bc.ParentIDColumn())
		sel := bcFK
		for _, c := range bcCols {
			sel += ", " + d.QuoteIdent(c)
		}
		sql := "SELECT " + sel + " FROM " + d.QuoteIdent(bc.Table()) +
			" WHERE " + bcFK + " = " + d.Placeholder(1)
		rows, err := l.eng.Querier().Query(ctx, sql, d.EncodeArg(domain.NewID(baseID)))
		if err != nil {
			return err
		}
		for rows.Next() {
			vp := reflect.New(bc.GoType())
			if _, err := core.ScanLeadingKey(rows, vp.Interface(), bcCols, bcByCol); err != nil {
				rows.Close()
				return err
			}
			if err := decodeChildPK(vp, bc, d); err != nil {
				rows.Close()
				return err
			}
			avos = append(avos, vp.Elem().Interface().(domain.AggregateValueObject))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if len(avos) > 0 {
		prov.GetAggregateRoot().AggregateConstructor(avos)
	}
	return nil
}

// goStringFieldValue reads an exported (possibly pointer) string-ish field of e by
// Go name as a string ("" when absent or a nil pointer) — used to read the natural
// key value off the role entity for the SharedBase identity lookup.
func goStringFieldValue(e any, goName string) string {
	rv := reflect.ValueOf(e)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}
	f := rv.FieldByName(goName)
	if !f.IsValid() {
		return ""
	}
	for f.Kind() == reflect.Pointer {
		if f.IsNil() {
			return ""
		}
		f = f.Elem()
	}
	return fmt.Sprintf("%v", f.Interface())
}

// hydrateSiblings loads each owner sibling's columns into the root entity by the
// shared primary key — the read-side mirror of the write-side partition. Each
// sibling is a SEPARATE single-table SELECT (not a JOIN): column names are unique
// by the schema's bijection, but the shared ID would be ambiguous under a join,
// and a per-table SELECT keeps the driver's scan + NULL handling simple. The
// sibling's own fields scan straight into the SAME struct at the indices its
// TableSchema resolved against T (a sibling is over the same Go type as its
// owner). An absent sibling row leaves those fields at their zero value. Loading
// the sibling here is what lets an UPDATE see the row's real sibling values
// rather than clobbering them with zeros.
func (l *AggregateLoader[T]) hydrateSiblings(ctx context.Context, entities []T, ids []string) error {
	sibs := l.schema.Siblings()
	if len(sibs) == 0 {
		return nil
	}
	dialect := l.eng.Dialect()
	pkCol := l.schema.IDColumn()
	for _, sib := range sibs {
		sibCols, sibByCol := sib.ScanPlan()
		if len(sibCols) == 0 {
			continue
		}
		// SELECT pk (leading key, discarded) + sibling columns, keyed by the
		// shared ID. The leading-key form reuses ScanLeadingKey, which scans the
		// remaining columns into the target struct at the byCol indices.
		sql := dialect.ApplyLimit("SELECT "+dialect.QuoteIdent(pkCol)+", "+
			strings.Join(quoteIdentifiers(sibCols, dialect), ", ")+
			" FROM "+dialect.QuoteIdent(sib.Table())+
			" WHERE "+dialect.QuoteIdent(pkCol)+" = "+dialect.Placeholder(1), 1)
		for i, ent := range entities {
			if err := l.scanSiblingInto(ctx, sql, ids[i], ent, sibCols, sibByCol); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *AggregateLoader[T]) scanSiblingInto(ctx context.Context, sql, id string, ent T, sibCols []string, sibByCol map[string]int) error {
	dialect := l.eng.Dialect()
	rows, err := l.eng.Querier().Query(ctx, sql, dialect.EncodeArg(domain.NewID(id)))
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		if _, err := core.ScanLeadingKey(rows, any(ent), sibCols, sibByCol); err != nil {
			return err
		}
	}
	return rows.Err()
}

// decodeChildPK normalizes an aggregate child's own ID struct field to the
// canonical uuid string via the dialect, after the row has been auto-scanned.
// A child's ID is mapped to an exported string field (the AggregateValueObject
// GetID() contract) and read as a non-leading column, so it bypasses the
// leading-key DecodeID the loader runs on the root ID and the child ParentID. On MySQL
// the field would otherwise carry the raw BINARY(16) bytes; on Postgres DecodeID
// is a passthrough (the scanned text is not 16 bytes). No-op when the schema
// declares no struct-field ID (IDIndex() < 0) or the field is not a string.
func decodeChildPK(vp reflect.Value, child *TableSchema, dialect Dialect) error {
	if child.IDIndex() < 0 {
		return nil
	}
	f := vp.Elem().Field(child.IDIndex())
	if f.Kind() != reflect.String {
		return nil
	}
	decoded, err := dialect.DecodeID(f.String())
	if err != nil {
		return err
	}
	f.SetString(decoded)
	return nil
}

// relSpecJoins records which relational-specialization tables a criteria
// referenced so findRoots can LEFT JOIN exactly those: sibling tables (joined on
// the shared ID) and/or the shared base (joined on the role's ParentID → base ID).
type relSpecJoins struct {
	siblings map[string]*TableSchema
	base     *TableSchema
	baseFK   string
}

// specResolver resolves a criteria Go field to its column, checking the anchor,
// then each sibling, then the shared base — recording any sibling/base referenced
// so the matching LEFT JOIN is emitted. Sibling and base columns are unique vs
// the anchor (the schema bijection), so they stay unqualified; only the shared ID
// is qualified by findRoots. A criteria mixing the ID and a specialization field
// is the one unsupported case (ID ambiguity) — filtering by ID makes any other
// predicate redundant.
func (l *AggregateLoader[T]) specResolver(j *relSpecJoins) core.FieldResolver {
	return func(goField string) (string, bool) {
		if col, ok := l.schema.ColumnOf(goField); ok {
			return col, true
		}
		for _, sib := range l.schema.Siblings() {
			if col, ok := sib.ColumnOf(goField); ok {
				j.siblings[sib.Table()] = sib
				return col, true
			}
		}
		if base, fk, ok := l.schema.SharedBaseRef(); ok {
			if col, ok2 := base.ColumnOf(goField); ok2 {
				j.base, j.baseFK = base, fk
				return col, true
			}
		}
		return "", false
	}
}

// specJoinClause renders a LEFT JOIN per referenced sibling (shared ID) and the
// shared base (role ParentID → base ID), ordered deterministically. Empty when the
// criteria referenced no specialization field.
func (l *AggregateLoader[T]) specJoinClause(j *relSpecJoins, dialect Dialect) string {
	if len(j.siblings) == 0 && j.base == nil {
		return ""
	}
	anchor := dialect.QuoteIdent(l.schema.Table())
	pk := dialect.QuoteIdent(l.schema.IDColumn())
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
	return sb.String()
}

// hydrateSharedBase loads a role's shared-base columns into the role entity,
// joining the base on the role's ParentID to the base ID and scanning the shared
// columns into the SAME struct at the role-field indices the schema pre-resolved.
// No-op when the role declares no shared base.
func (l *AggregateLoader[T]) hydrateSharedBase(ctx context.Context, entities []T, ids []string) error {
	base, fkCol, ok := l.schema.SharedBaseRef()
	if !ok {
		return nil
	}
	cols, byCol, _ := l.schema.SharedBaseScanPlan()
	if len(cols) == 0 {
		return nil
	}
	d := l.eng.Dialect()
	roleTbl := d.QuoteIdent(l.schema.Table())
	baseTbl := d.QuoteIdent(base.Table())
	rolePK := d.QuoteIdent(l.schema.IDColumn())
	sel := roleTbl + "." + rolePK
	for _, c := range cols {
		sel += ", " + baseTbl + "." + d.QuoteIdent(c)
	}
	sql := d.ApplyLimit("SELECT "+sel+" FROM "+roleTbl+
		" JOIN "+baseTbl+" ON "+baseTbl+"."+d.QuoteIdent(base.IDColumn())+" = "+roleTbl+"."+d.QuoteIdent(fkCol)+
		" WHERE "+roleTbl+"."+rolePK+" = "+d.Placeholder(1), 1)
	for i, ent := range entities {
		if err := l.scanSiblingInto(ctx, sql, ids[i], ent, cols, byCol); err != nil {
			return err
		}
	}
	return nil
}

// childScanSQL builds the child SELECT + the scan plan. With no child siblings it
// is the plain single-table SELECT (leading ParentID + child columns). With siblings it
// LEFT JOINs each on the shared child ID and folds the sibling columns into the
// scan plan — they map into the SAME child struct (a sibling is over the child's
// Go type). Everything is qualified by table under the join; sibling/child
// business columns are unique (the schema bijection), so the scan keys cleanly.
func childScanSQL(child *TableSchema, fkCol string, childCols []string, childByCol map[string]int, placeholders []string, childFilter string, dialect Dialect) (string, []string, map[string]int) {
	ct := dialect.QuoteIdent(child.Table())
	sibs := child.Siblings()
	if len(sibs) == 0 {
		sql := "SELECT " + dialect.QuoteIdent(fkCol) + ", " + strings.Join(quoteIdentifiers(childCols, dialect), ", ") +
			" FROM " + ct + " WHERE " + dialect.QuoteIdent(fkCol) + " IN (" + strings.Join(placeholders, ", ") + ") " + childFilter
		return sql, childCols, childByCol
	}
	pk := dialect.QuoteIdent(child.IDColumn())
	sel := ct + "." + dialect.QuoteIdent(fkCol)
	for _, c := range childCols {
		sel += ", " + ct + "." + dialect.QuoteIdent(c)
	}
	scanCols := append([]string{}, childCols...)
	scanByCol := make(map[string]int, len(childByCol))
	for k, v := range childByCol {
		scanByCol[k] = v
	}
	var join strings.Builder
	for _, sib := range sibs {
		sc, sb := sib.ScanPlan()
		st := dialect.QuoteIdent(sib.Table())
		join.WriteString(" LEFT JOIN " + st + " ON " + ct + "." + pk + " = " + st + "." + pk)
		for _, c := range sc {
			sel += ", " + st + "." + dialect.QuoteIdent(c)
			scanCols = append(scanCols, c)
		}
		for k, v := range sb {
			scanByCol[k] = v
		}
	}
	sql := "SELECT " + sel + " FROM " + ct + join.String() +
		" WHERE " + ct + "." + dialect.QuoteIdent(fkCol) + " IN (" + strings.Join(placeholders, ", ") + ") " + childFilter
	return sql, scanCols, scanByCol
}

// quoteIdentifiers quotes each name via the dialect (which validates against the
// SQL-identifier allowlist first — the same injection defense on every backend).
func quoteIdentifiers(cols []string, dialect Dialect) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = dialect.QuoteIdent(c)
	}
	return out
}
