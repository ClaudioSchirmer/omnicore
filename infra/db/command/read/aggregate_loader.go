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
// the backend-neutral infra.Row; the scanner does row.Scan(&t.field1, ...) and
// returns the entity. It does not need to set the ID — the loader reads it via
// t.GetID(). Because the scanner takes infra.Row (not pgx.Row), a manual scanner
// runs on any engine; the consumer owns any dialect-specific column decoding
// (e.g. a MySQL BINARY(16) id).
type RootScanner[T domain.Entity] func(Row) (T, error)

// ChildScanner deserializes one row into an AggregateValueObject (neutral
// infra.Rows — the scanner reads the current row via Scan).
type ChildScanner func(Rows) (domain.AggregateValueObject, error)

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
// loader resolves table/PK/FK/column from the same explicit map the write side
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
	return entities, nil
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
	// the shared PK is qualified (in the JOIN ON + the SELECT leading key).
	joins := &relSpecJoins{siblings: map[string]*TableSchema{}}
	resolve := l.specResolver(joins)
	dialect := l.eng.Dialect()

	where, args, err := compileWhere(q.Condition(), resolve, dialect)
	if err != nil {
		return nil, nil, err
	}
	orderSQL, err := compileOrder(q.OrderFields(), resolve, dialect)
	if err != nil {
		return nil, nil, err
	}
	clause := buildWhereClause(where, scopeGate(q.Scope(), l.schema, dialect))
	limit := q.LimitValue()
	if limitOverride > 0 {
		limit = limitOverride
	}

	// Manual root scanner: SELECT * + dev-controlled scan; id via GetID().
	// Runs through the neutral Querier — the scanner receives infra.Row, so this
	// works on any engine (the consumer owns any dialect-specific decoding).
	if l.rootScanner != nil {
		sql := "SELECT * FROM " + dialect.QuoteIdent(table)
		sql += tailClause(clause, orderSQL, limit)
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
			root, err := l.rootScanner(rows)
			if err != nil {
				return nil, nil, err
			}
			idp := root.GetID()
			if idp == nil || idp.IsEmpty() {
				return nil, nil, fmt.Errorf(
					"AggregateLoader[%s]: a manual root scanner used with FindOne/FindAll must populate the id "+
						"(scan it and call SetID) — the framework injects no id on the criteria path",
					l.effectiveContextName(),
				)
			}
			entities = append(entities, root)
			ids = append(ids, idp.Value())
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		return entities, ids, nil
	}

	// Auto-scan: SELECT <pk>, <cols> + positional scan with the PK read back.
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
	// only to make the sibling columns reachable for filtering. The leading PK is
	// qualified to the anchor table because the shared PK is the one ambiguous
	// column under the join.
	joinSQL := l.specJoinClause(joins, dialect)
	leadingPK := dialect.QuoteIdent(l.schema.PKColumn())
	if joinSQL != "" {
		leadingPK = dialect.QuoteIdent(table) + "." + dialect.QuoteIdent(l.schema.PKColumn())
	}
	sql := "SELECT " + leadingPK + ", " + strings.Join(quoteIdentifiers(cols, dialect), ", ") + " FROM " + dialect.QuoteIdent(table) + joinSQL
	sql += tailClause(clause, orderSQL, limit)
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

// tailClause renders the " WHERE … [ORDER BY …] [LIMIT n]" suffix shared by the
// auto-scan and manual-scanner root SELECTs (each part already validated).
func tailClause(clause, orderSQL string, limit int64) string {
	var sb strings.Builder
	if clause != "" {
		sb.WriteByte(' ')
		sb.WriteString(clause)
	}
	if orderSQL != "" {
		sb.WriteByte(' ')
		sb.WriteString(orderSQL)
	}
	if limit > 0 {
		fmt.Fprintf(&sb, " LIMIT %d", limit)
	}
	return sb.String()
}

// hydrateChildren loads + attaches children for the given roots. Auto-scan
// children are loaded in one batched SELECT per type (WHERE fk IN (...)) and
// grouped by FK; a manual ChildScanner falls back to one SELECT per root (it
// cannot expose the FK generically). Child rows honor the scope the same way
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
		fkCol := child.FKColumn()
		childFilter := childScopeFilter(scope, child, dialect)

		if manual, ok := l.childScanners[typeName]; ok {
			// Manual child scanner: neutral Querier; the FK arg is dialect-encoded
			// (text on PG, BINARY(16) bytes on MySQL) just like the auto path.
			sql := fmt.Sprintf(
				"SELECT * FROM %s WHERE %s = %s %s",
				dialect.QuoteIdent(childTable), dialect.QuoteIdent(fkCol), dialect.Placeholder(1), childFilter,
			)
			for _, id := range rootIDs {
				rows, err := l.eng.Querier().Query(ctx, sql, dialect.EncodeArg(domain.NewID(id)))
				if err != nil {
					return err
				}
				for rows.Next() {
					avo, err := manual(rows)
					if err != nil {
						rows.Close()
						return err
					}
					avosByRoot[id] = append(avosByRoot[id], avo)
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return err
				}
				rows.Close()
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
			// The FK is the leading key (decoded above); the child's OWN PK is a
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

// hydrateSiblings loads each owner sibling's columns into the root entity by the
// shared primary key — the read-side mirror of the write-side partition. Each
// sibling is a SEPARATE single-table SELECT (not a JOIN): column names are unique
// by the schema's bijection, but the shared PK would be ambiguous under a join,
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
	pkCol := l.schema.PKColumn()
	for _, sib := range sibs {
		sibCols, sibByCol := sib.ScanPlan()
		if len(sibCols) == 0 {
			continue
		}
		// SELECT pk (leading key, discarded) + sibling columns, keyed by the
		// shared PK. The leading-key form reuses ScanLeadingKey, which scans the
		// remaining columns into the target struct at the byCol indices.
		sql := "SELECT " + dialect.QuoteIdent(pkCol) + ", " +
			strings.Join(quoteIdentifiers(sibCols, dialect), ", ") +
			" FROM " + dialect.QuoteIdent(sib.Table()) +
			" WHERE " + dialect.QuoteIdent(pkCol) + " = " + dialect.Placeholder(1) + " LIMIT 1"
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

// decodeChildPK normalizes an aggregate child's own PK struct field to the
// canonical uuid string via the dialect, after the row has been auto-scanned.
// A child's PK is mapped to an exported string field (the AggregateValueObject
// GetID() contract) and read as a non-leading column, so it bypasses the
// leading-key DecodeID the loader runs on the root PK and the child FK. On MySQL
// the field would otherwise carry the raw BINARY(16) bytes; on Postgres DecodeID
// is a passthrough (the scanned text is not 16 bytes). No-op when the schema
// declares no struct-field PK (PKIndex() < 0) or the field is not a string.
func decodeChildPK(vp reflect.Value, child *TableSchema, dialect Dialect) error {
	if child.PKIndex() < 0 {
		return nil
	}
	f := vp.Elem().Field(child.PKIndex())
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
// the shared PK) and/or the shared base (joined on the role's FK → base PK).
type relSpecJoins struct {
	siblings map[string]*TableSchema
	base     *TableSchema
	baseFK   string
}

// specResolver resolves a criteria Go field to its column, checking the anchor,
// then each sibling, then the shared base — recording any sibling/base referenced
// so the matching LEFT JOIN is emitted. Sibling and base columns are unique vs
// the anchor (the schema bijection), so they stay unqualified; only the shared PK
// is qualified by findRoots. A criteria mixing the PK and a specialization field
// is the one unsupported case (PK ambiguity) — filtering by PK makes any other
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

// specJoinClause renders a LEFT JOIN per referenced sibling (shared PK) and the
// shared base (role FK → base PK), ordered deterministically. Empty when the
// criteria referenced no specialization field.
func (l *AggregateLoader[T]) specJoinClause(j *relSpecJoins, dialect Dialect) string {
	if len(j.siblings) == 0 && j.base == nil {
		return ""
	}
	anchor := dialect.QuoteIdent(l.schema.Table())
	pk := dialect.QuoteIdent(l.schema.PKColumn())
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
		sb.WriteString(" LEFT JOIN " + bt + " ON " + bt + "." + dialect.QuoteIdent(j.base.PKColumn()) +
			" = " + anchor + "." + dialect.QuoteIdent(j.baseFK))
	}
	return sb.String()
}

// hydrateSharedBase loads a role's shared-base columns into the role entity,
// joining the base on the role's FK to the base PK and scanning the shared
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
	rolePK := d.QuoteIdent(l.schema.PKColumn())
	sel := roleTbl + "." + rolePK
	for _, c := range cols {
		sel += ", " + baseTbl + "." + d.QuoteIdent(c)
	}
	sql := "SELECT " + sel + " FROM " + roleTbl +
		" JOIN " + baseTbl + " ON " + baseTbl + "." + d.QuoteIdent(base.PKColumn()) + " = " + roleTbl + "." + d.QuoteIdent(fkCol) +
		" WHERE " + roleTbl + "." + rolePK + " = " + d.Placeholder(1) + " LIMIT 1"
	for i, ent := range entities {
		if err := l.scanSiblingInto(ctx, sql, ids[i], ent, cols, byCol); err != nil {
			return err
		}
	}
	return nil
}

// childScanSQL builds the child SELECT + the scan plan. With no child siblings it
// is the plain single-table SELECT (leading FK + child columns). With siblings it
// LEFT JOINs each on the shared child PK and folds the sibling columns into the
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
	pk := dialect.QuoteIdent(child.PKColumn())
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
