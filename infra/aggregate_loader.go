package infra

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/jackc/pgx/v5"
)

// RootScanner deserializes a Postgres row into a populated entity T.
// The loader passes pgx.Row (single row). The scanner does row.Scan(&t.field1, ...)
// and returns the entity. It does not need to set the ID — the loader does it via t.SetID(id).
type RootScanner[T domain.Entity] func(pgx.Row) (T, error)

// ChildScanner deserializes a Postgres row into an AggregateValueObject.
type ChildScanner func(pgx.Rows) (domain.AggregateValueObject, error)

// AggregateLoader[T] loads an aggregate root + its children from Postgres. The
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
	pg            *Postgres
	newEntity     func() T
	contextName   string
	schema        *TableSchema
	rootScanner   RootScanner[T]
	childScanners map[string]ChildScanner
}

// NewAggregateLoader initializes a loader.
func NewAggregateLoader[T domain.Entity](pg *Postgres, newEntity func() T) *AggregateLoader[T] {
	return &AggregateLoader[T]{
		pg:            pg,
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
	resolve := l.schema.fieldResolver()

	where, args, err := compileWhere(q.Condition(), resolve)
	if err != nil {
		return nil, nil, err
	}
	orderSQL, err := compileOrder(q.OrderFields(), resolve)
	if err != nil {
		return nil, nil, err
	}
	clause := buildWhereClause(where, scopeGate(q.Scope(), l.schema))
	limit := q.LimitValue()
	if limitOverride > 0 {
		limit = limitOverride
	}

	// Manual root scanner: SELECT * + dev-controlled scan; id via GetID().
	if l.rootScanner != nil {
		sql := "SELECT * FROM " + validIdentifier(table)
		sql += tailClause(clause, orderSQL, limit)
		rows, err := l.pg.Pool().Query(ctx, sql, args...)
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
	cols, byCol := l.schema.scanPlan()
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf(
			"AggregateLoader[%s]: schema declares no columns — %T exposes no persisted fields. "+
				"Use WithRootScanner to provide a manual scanner",
			l.effectiveContextName(), sample,
		)
	}
	sql := "SELECT " + validIdentifier(l.schema.PKColumn()) + ", " + strings.Join(quoteIdentifiers(cols), ", ") + " FROM " + validIdentifier(table)
	sql += tailClause(clause, orderSQL, limit)
	rows, err := l.pg.Pool().Query(ctx, sql, args...)
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
		id, err := scanLeadingKey(rows, any(root), cols, byCol)
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
	if len(entities) == 0 || (len(l.childScanners) == 0 && len(l.schema.children) == 0) {
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

	avosByRoot := make(map[string][]domain.AggregateValueObject, len(providersByID))

	// The schema declares the aggregate children; a child with a manual scanner
	// (WithChildScanner) uses it, every other declared child is auto-scanned
	// from its TableSchema.
	seen := map[string]bool{}
	for typeName := range l.childScanners {
		seen[typeName] = true
	}
	for typeName := range l.schema.children {
		seen[typeName] = true
	}

	for typeName := range seen {
		child := l.schema.childSchema(typeName)
		if child == nil {
			return fmt.Errorf(
				"AggregateLoader[%s] child %q: no TableSchema declared (root.Child(...))",
				l.effectiveContextName(), typeName,
			)
		}
		childTable := child.Table()
		fkCol := child.FKColumn()
		childFilter := childScopeFilter(scope, child)

		if manual, ok := l.childScanners[typeName]; ok {
			sql := fmt.Sprintf(
				"SELECT * FROM %s WHERE %s = $1 %s",
				validIdentifier(childTable), validIdentifier(fkCol), childFilter,
			)
			for _, id := range rootIDs {
				rows, err := l.pg.Pool().Query(ctx, sql, id)
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

		childCols, childByCol := child.scanPlan()
		if len(childCols) == 0 {
			return fmt.Errorf(
				"AggregateLoader[%s] child %q: schema declares no columns",
				l.effectiveContextName(), typeName,
			)
		}
		placeholders := make([]string, len(rootIDs))
		qargs := make([]any, len(rootIDs))
		for i, id := range rootIDs {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			qargs[i] = id
		}
		sql := fmt.Sprintf(
			"SELECT %s, %s FROM %s WHERE %s IN (%s) %s",
			validIdentifier(fkCol),
			strings.Join(quoteIdentifiers(childCols), ", "),
			validIdentifier(childTable),
			validIdentifier(fkCol),
			strings.Join(placeholders, ", "),
			childFilter,
		)
		rows, err := l.pg.Pool().Query(ctx, sql, qargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			vp := reflect.New(child.typ)
			fk, err := scanLeadingKey(rows, vp.Interface(), childCols, childByCol)
			if err != nil {
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

// quoteIdentifiers escapes names via validIdentifier (SQL-injection defense).
func quoteIdentifiers(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = validIdentifier(c)
	}
	return out
}
