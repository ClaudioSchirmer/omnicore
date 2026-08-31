package read

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// AggregateLoader[T] loads an aggregate root + its children from the configured
// relational backend. The
// root and children tables/columns come from the TableSchema attached via
// WithSchema (the same schema the write side uses) — no convention inference.
//
// There is ONE scan path: the framework reads each declared schema and scans the
// columns straight into the struct. The mapping is the TableSchema's and nobody
// else's — a read that needs a different shape declares a different schema over a
// different type, which is cheaper to write and impossible to get half-wired.
//
// Typical usage:
//
//	loader := infra.NewAggregateLoader[*appdomain.User](pg, func() *appdomain.User {
//	    return &appdomain.User{}
//	}).WithContextName("User").WithSchema(userSchema)
//
//	func (r *UserRepository) FindByID(id domain.ID) (*appdomain.User, error) {
//	    return loader.FindOne(context.Background(), criteria.ByID(id))
//	}
type AggregateLoader[T domain.Entity] struct {
	// directCore carries everything a read can answer WITHOUT an entity: the
	// schema, the engine, the declared traversals, the criteria compilation, the
	// existence probe and the aggregate DSL. What stays here is the part that is
	// genuinely about T — hydrating the aggregate (children, siblings, shared
	// base), stamping the id and the old-state snapshot.
	directCore
	newEntity func() T
}

// NewAggregateLoader initializes a loader over a RelationalEngine. Every read
// runs through the engine's neutral Querier + Dialect, so the loader works on any
// backend without a driver type ever surfacing.
func NewAggregateLoader[T domain.Entity](eng RelationalEngine, newEntity func() T) *AggregateLoader[T] {
	return &AggregateLoader[T]{
		directCore: directCore{eng: eng, name: TypeName[T]()},
		newEntity:  newEntity,
	}
}

// WithContextName overrides the default. When not called, contextName is
// derived from type T via TypeName[T]() (e.g. T=*User → "User"). Set only
// for a custom magic pattern (legacy schema, two Repositories over the same
// entity, etc.).
func (l *AggregateLoader[T]) WithContextName(name string) *AggregateLoader[T] {
	l.name = name
	return l
}

// WithSchema attaches the repository's TableSchema (root + child schemas) so the
// loader resolves table/ID/ParentID/column from the same explicit map the write side
// uses. Shared between write and read sides of a Repository.
func (l *AggregateLoader[T]) WithSchema(schema *TableSchema) *AggregateLoader[T] {
	l.schema = schema
	return l
}

// WithJoins declares the READ-ONLY traversals this aggregate may reach across in
// a query — see join.go for what a join is and why it lives here rather than on
// the TableSchema. Every declaration is validated against the schema NOW: a join
// naming a column, a child or a Go field that does not exist panics at
// construction, never on the first request. Call WithSchema first.
//
// It takes the WHOLE set and may be called ONCE — see directCore.declareJoins,
// which both this loader and a Direct repository declare through.
//
// A chain that reaches past the first hop files a boot advisory here, and only
// here: the cost it names is the AGGREGATE's — these joins ride FindByID, and
// FindByID is the write path's load. The Direct repository declares through the
// same core and reports nothing, at any depth, because it has no write path to
// charge. See join_advisory.go.
func (l *AggregateLoader[T]) WithJoins(bindings ...*JoinBinding) *AggregateLoader[T] {
	l.declareJoins(bindings...)
	reportDeepChains(l.contextName(), l.schema, l.joins)
	return l
}

// FindOne loads exactly one aggregate matching the criteria — root + children,
// the live domain entity ready for a command. Returns a *DomainError with
// RecordNotFoundNotification (HTTP 404) when nothing matches, and an error when
// the criteria matches more than one row (the contract is "expected one"; >1 is
// a developer mistake surfaced loudly rather than silently picking the first).
// A LIMIT 2 probe bounds the >1 check; any Limit set on the Query is overridden.
//
// This is the framework's BIRTH point for a write-side entity: once the whole
// aggregate is hydrated (root + siblings + shared base + children), the load
// stamps the old-state snapshot via domain.CaptureOld, so domain.Old[T] answers
// the PERSISTED state for every state-changing verb. Because every single-entity
// load funnels here — the canonical repository's FindByID / FindArchivedByID and
// any custom repository finder built on this loader — a hand-written handler gets
// the same guarantee as an Auto one without doing anything. FindAll deliberately
// does NOT snapshot: it is the read-side list path, where no verb mutates and the
// per-row clone would be pure cost.
func (l *AggregateLoader[T]) FindOne(ctx context.Context, q *criteria.Query) (T, error) {
	entities, ids, err := l.findRoots(ctx, q, 2)
	if err != nil {
		return *new(T), err
	}
	if len(entities) == 0 {
		return *new(T), domain.NotFoundError(l.contextName(), "criteria", "")
	}
	if len(entities) > 1 {
		return *new(T), fmt.Errorf(
			"AggregateLoader[%s].FindOne: criteria matched more than one row — use FindAll or a more specific criterion",
			l.contextName(),
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
	domain.CaptureOld(entities[0])
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

// findRoots compiles the criteria into a root SELECT and scans the matched
// roots, recovering each row's id (FindOne/FindAll do not know the id a priori).
// limitOverride > 0 replaces the Query's limit (FindOne uses 2).
//
// The framework controls the SELECT: it prepends `id` and reads it back
// positionally, since the root struct does not expose the id as a field.
func (l *AggregateLoader[T]) findRoots(ctx context.Context, q *criteria.Query, limitOverride int64) ([]T, []string, error) {
	sample := l.newEntity()
	table := l.schema.Table()
	// A sibling-aware resolver: anchor fields resolve as before; a field that
	// lives in a sibling resolves to the sibling's column AND records the sibling
	// so it is LEFT JOINed below. Sibling columns are unique across the node
	// (the schema's bijection), so they stay unqualified and unambiguous; only
	// the shared ID is qualified (in the JOIN ON + the SELECT leading key).
	joins := newJoinedTables(l.joins)
	resolve := l.resolverRecordingJoins(joins)
	dialect := l.eng.Dialect()

	// The owner qualification (every anchor-side column carries its own table) is
	// a static property of the loader — a declared join is always in the FROM —
	// so it is decided before the first compile, not discovered from the criteria.
	qual := core.ColQual{Owner: len(rootJoins(l.joins)) > 0}
	where, args, err := core.CompileWhereQualified(q.Condition(), resolve, dialect, l.idKindResolver(), qual)
	if err != nil {
		return nil, nil, err
	}
	orderSQL, err := core.CompileOrderQualified(q.OrderFields(), resolve, dialect, qual)
	if err != nil {
		return nil, nil, err
	}
	// When the criteria pulled in a sibling/base LEFT JOIN, the root's DeletedAt
	// column must be table-qualified (the base carries its own deleted_at) — the
	// same disambiguation the leading ID gets below.
	rootQualifier := ""
	if joins.any() {
		rootQualifier = dialect.QuoteIdent(table)
		// The shared id column lives in BOTH the anchor and the joined 1:1 table,
		// so a bare "id" in the predicate or the ORDER BY … , id tiebreak is
		// ambiguous. Recompile WHERE/ORDER with the anchor id qualified (every
		// other column is unique across the node, so only the id needs it).
		qual.IDCol, qual.IDQualifier = l.schema.IDColumn(), rootQualifier
		where, args, err = core.CompileWhereQualified(q.Condition(), resolve, dialect, l.idKindResolver(), qual)
		if err != nil {
			return nil, nil, err
		}
		orderSQL, err = core.CompileOrderQualified(q.OrderFields(), resolve, dialect, qual)
		if err != nil {
			return nil, nil, err
		}
	}
	clause := core.BuildWhereClause(where, core.ScopeGate(q.Scope(), l.schema, dialect, rootQualifier))
	limit := q.LimitValue()
	offset := q.OffsetValue()
	if limitOverride > 0 {
		// An internal cap (FindOne's uniqueness probe) replaces the Query's window
		// wholesale — a "first match" lookup is single-row by definition, so a
		// caller's offset never shifts it.
		limit = limitOverride
		offset = 0
	}

	// SELECT <pk>, <cols> + positional scan with the ID read back.
	cols, byCol := l.schema.ScanPlan()
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf(
			"AggregateLoader[%s]: schema declares no columns — %T exposes no persisted fields. "+
				"Declare them with TableSchema.Field(...)",
			l.contextName(), sample,
		)
	}
	// When the criteria referenced a sibling field, LEFT JOIN those tables so the
	// WHERE/ORDER can resolve against them. Their VALUES are loaded separately by
	// hydrateSiblings, so the SELECT list carries no sibling column — that join
	// exists only for reachability. A DECLARED join is different: it is always in
	// the FROM and its fields ride THIS SELECT, so they cost no extra round trip.
	// The leading ID is qualified to the anchor table because the shared ID is the
	// one ambiguous column under any join.
	joinSQL := l.joinClause(joins, dialect)
	leadingPK := dialect.QuoteIdent(l.schema.IDColumn())
	if joinSQL != "" {
		leadingPK = dialect.QuoteIdent(table) + "." + dialect.QuoteIdent(l.schema.IDColumn())
	}
	// Trailing framework-managed columns (created_at/updated_at/deleted_at/
	// revision) go into the carrier via ms.apply, not into struct fields.
	ms := newManagedScan(l.schema)
	// Under ANY join the anchor's own columns are qualified — a joined aggregate
	// is a foreign namespace, free to carry a "name" or a "code" of its own, and
	// a bare one in the SELECT list is ambiguous exactly like the id is.
	anchorQualifier := ""
	if joinSQL != "" {
		anchorQualifier = dialect.QuoteIdent(table)
	}
	selectCols := strings.Join(qualifyIdentifiers(cols, dialect, anchorQualifier), ", ")
	if ms.has() {
		selectCols += ", " + strings.Join(qualifyIdentifiers(ms.cols, dialect, anchorQualifier), ", ")
	}
	if exprs := joinSelectExprs(l.rootJoinNodes(), dialect); len(exprs) > 0 {
		selectCols += ", " + strings.Join(exprs, ", ")
	}
	sql := "SELECT " + leadingPK + ", " + selectCols + " FROM " + dialect.QuoteIdent(table) + joinSQL
	sql += tailClause(clause, orderSQL)
	sql, err = applyWindow(dialect, sql, limit, offset, orderSQL)
	if err != nil {
		return nil, nil, err
	}
	rows, err := l.rows().Query(ctx, sql, args...)
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
		// The declared joins' values ride the same row, AFTER the managed columns
		// — the order joinSelectExprs listed them in.
		trailing := ms.targets()
		joinTargets, err := joinScanTargets(any(root), l.rootJoinNodes())
		if err != nil {
			return nil, nil, err
		}
		trailing = append(trailing, joinTargets...)
		raw, err := core.ScanLeadingKeyTrailing(rows, any(root), cols, byCol, trailing...)
		if err != nil {
			return nil, nil, err
		}
		id, err := dialect.DecodeID(raw)
		if err != nil {
			return nil, nil, err
		}
		root.SetID(domain.NewID(id))
		if err := ms.apply(root, dialect); err != nil {
			return nil, nil, err
		}
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

// tailClause renders the " WHERE … [ORDER BY …]" suffix shared by the root
// SELECTs this loader builds (each part already validated). The row cap is NOT
// part of the tail: the caller applies it over the complete statement via
// Dialect.ApplyLimit, so each engine caps in its native position.
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

// hydrateChildren loads + attaches children for the given roots: one batched
// SELECT per child type (WHERE fk IN (...)), grouped by ParentID in memory.
// Child rows honor the scope the same way
// the root gate does — see core.ChildScopeFilter (active → deleted_at IS NULL; any
// archived scope → unfiltered, so the unarchive cascade sees every child via
// AllAggregateItems()).
func (l *AggregateLoader[T]) hydrateChildren(ctx context.Context, entities []T, ids []string, scope criteria.Scope) error {
	if len(entities) == 0 || len(l.schema.ChildSchemaNames()) == 0 {
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

	// The schema declares the aggregate children, and the schema alone: every
	// declared child is scanned from its own TableSchema.
	for _, typeName := range l.schema.ChildSchemaNames() {
		// typeName came from ChildSchemaNames(), which reads the very map this
		// looks up — the schema is always there.
		child := l.schema.ChildSchema(typeName)
		fkCol := child.ParentIDColumn()

		childCols, childByCol := child.ScanPlan()
		if len(childCols) == 0 {
			return fmt.Errorf(
				"AggregateLoader[%s] child %q: schema declares no columns",
				l.contextName(), typeName,
			)
		}
		placeholders := make([]string, len(rootIDs))
		qargs := make([]any, len(rootIDs))
		for i, id := range rootIDs {
			placeholders[i] = dialect.Placeholder(i + 1)
			qargs[i] = dialect.EncodeArg(domain.NewID(id))
		}
		// The child's carrier columns (own id + managed timestamps/revision) ride
		// as trailing columns: the id left the scan plan when it moved into
		// domain.Managed, so it is no longer a struct field — it and the managed
		// columns are stamped onto the carrier via ms.apply after the scan.
		ms := newChildManagedScan(child)
		cJoins := resolveJoins(childJoinsOf(l.joins, child.Table()), child.Table())
		sql, scanCols, scanByCol := childScanSQL(child, fkCol, childCols, childByCol, placeholders, scope, dialect, ms.cols, cJoins)
		rows, err := l.rows().Query(ctx, sql, qargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			vp := reflect.New(child.GoType())
			trailing := ms.targets()
			if len(cJoins) > 0 {
				jt, jerr := joinScanTargets(vp.Interface(), cJoins)
				if jerr != nil {
					rows.Close()
					return jerr
				}
				trailing = append(trailing, jt...)
			}
			fkRaw, err := core.ScanLeadingKeyTrailing(rows, vp.Interface(), scanCols, scanByCol, trailing...)
			if err != nil {
				rows.Close()
				return err
			}
			// The ParentID is the leading key (decoded here); the child's own id and
			// managed columns are the trailing targets, stamped by ms.apply (which
			// decodes the id through the dialect — BINARY(16) → canonical uuid on
			// MySQL, passthrough on Postgres).
			fk, err := dialect.DecodeID(fkRaw)
			if err != nil {
				rows.Close()
				return err
			}
			if err := ms.apply(vp.Interface(), dialect); err != nil {
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
				l.contextName(), bc.GoType().Name())
		}
		bcTbl := d.QuoteIdent(bc.Table())
		// SELECT role.pk (leading key → groups by aggregate) + base-child columns +
		// the base-child's carrier columns (own id + managed), joining the
		// base-child to the role on the shared base id.
		ms := newChildManagedScan(bc)
		sel := roleTbl + "." + rolePK
		for _, c := range bcCols {
			sel += ", " + bcTbl + "." + d.QuoteIdent(c)
		}
		for _, c := range ms.cols {
			sel += ", " + bcTbl + "." + d.QuoteIdent(c)
		}
		sql := "SELECT " + sel + " FROM " + bcTbl +
			" JOIN " + roleTbl + " ON " + bcTbl + "." + d.QuoteIdent(bc.ParentIDColumn()) + " = " + roleTbl + "." + roleFK +
			// bcTbl-qualified: the JOIN to the role table brings a second deleted_at
			// into scope, so the base-child's active gate must name its own table.
			" WHERE " + roleTbl + "." + rolePK + " IN (" + strings.Join(placeholders, ", ") + ") " + core.ChildScopeFilter(scope, bc, d, bcTbl)
		rows, err := l.rows().Query(ctx, sql, qargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			vp := reflect.New(bc.GoType())
			rootRaw, err := core.ScanLeadingKeyTrailing(rows, vp.Interface(), bcCols, bcByCol, ms.targets()...)
			if err != nil {
				rows.Close()
				return err
			}
			rootID, err := d.DecodeID(rootRaw)
			if err != nil {
				rows.Close()
				return err
			}
			if err := ms.apply(vp.Interface(), d); err != nil {
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
			"AggregateLoader[%s]: shared base natural key %q has no Go field", l.contextName(), nkCol)
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
	rows, err := l.rows().Query(ctx, sql, nkVal)
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
	// is excluded — DeletedAt is delete, so the insert falls through and the
	// schema's constraints arbitrate the collision with the remnant (there is no
	// revival on POST; /unarchive is the explicit path back). The persister's
	// active-role probe + UNIQUE(fk) remain the in-TX race backstop.
	roleExists, err := l.activeRoleExists(ctx, fkCol, baseID)
	if err != nil {
		return fresh, false, err
	}
	if roleExists {
		return fresh, false, core.SingleNotificationError(
			l.contextName(), l.schema.IDColumn(), domain.EntityAlreadyAddedNotification{})
	}
	if err := l.loadBaseChildrenConstructor(ctx, newE, base, baseID); err != nil {
		return fresh, false, err
	}
	return newE, true, nil
}

// activeRoleExists reports whether a live (non-archived) specialization role
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
	if sd, ok := l.schema.DeletedAtColumn(); ok {
		where += " AND " + d.QuoteIdent(sd) + " IS NULL"
	}
	return l.probeExists(ctx, d.QuoteIdent(l.schema.Table())+where, d.EncodeArg(domain.NewID(baseID)))
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
		ms := newChildManagedScan(bc)
		sel := bcFK
		for _, c := range bcCols {
			sel += ", " + d.QuoteIdent(c)
		}
		for _, c := range ms.cols {
			sel += ", " + d.QuoteIdent(c)
		}
		sql := "SELECT " + sel + " FROM " + d.QuoteIdent(bc.Table()) +
			" WHERE " + bcFK + " = " + d.Placeholder(1)
		rows, err := l.rows().Query(ctx, sql, d.EncodeArg(domain.NewID(baseID)))
		if err != nil {
			return err
		}
		for rows.Next() {
			vp := reflect.New(bc.GoType())
			if _, err := core.ScanLeadingKeyTrailing(rows, vp.Interface(), bcCols, bcByCol, ms.targets()...); err != nil {
				rows.Close()
				return err
			}
			if err := ms.apply(vp.Interface(), d); err != nil {
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
	// A value-object natural key (e.g. Document) reads as its underlying scalar.
	if u, ok := domain.ValueObjectValue(f.Interface()); ok {
		return fmt.Sprintf("%v", u)
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

func (l *AggregateLoader[T]) scanSiblingInto(ctx context.Context, sql, id string, ent T, sibCols []string, sibByCol map[string]core.FieldPath) error {
	dialect := l.eng.Dialect()
	rows, err := l.rows().Query(ctx, sql, dialect.EncodeArg(domain.NewID(id)))
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

// joinedTables records which 1:1 tables a criteria
// referenced so findRoots can LEFT JOIN exactly those: sibling tables (joined on
// the shared ID) and/or the shared base (joined on the role's ParentID → base ID).
type joinedTables struct {
	siblings map[string]*TableSchema
	base     *TableSchema
	baseFK   string
	// hasDeclared reports whether the owner declares any ROOT join. Unlike the
	// 1:1 satellites above — pulled in only when a criteria names one of their
	// fields — a declared join is ALWAYS in the FROM, because its Fields are
	// loaded on every read. A field that appeared only when a filter happened to
	// mention it would leave the same entity populated on one call and blank on
	// the next, silently.
	hasDeclared bool
}

// any reports whether ANY join was pulled in — sibling, shared base or declared.
// It is what makes the anchor id ambiguous: every joined table has an "id" of its
// own, so a bare "id" in the predicate or the ORDER BY tiebreak must be qualified
// to the anchor once any of them is in the FROM.
func (j *joinedTables) any() bool {
	return len(j.siblings) > 0 || j.base != nil || j.hasDeclared
}

// rootJoins is the declared traversals that hang off the root, in declaration
// order — the order the FROM, the SELECT list and the scan targets all follow, so
// the three cannot drift.
func rootJoins(joins []Join) []Join {
	out := make([]Join, 0, len(joins))
	for _, j := range joins {
		if j.Child == nil {
			out = append(out, j)
		}
	}
	return out
}

// joinSelectExprs renders the alias-qualified column list declared traversals
// contribute to the root SELECT, in the same PRE-ORDER joinScanTargets builds its
// destinations — a chain's own columns first, then everything that continues from
// it, so a column and its destination stay paired at any depth.
func joinSelectExprs(nodes []joinNode, dialect Dialect) []string {
	var out []string
	for _, n := range flattenJoins(nodes) {
		alias := dialect.QuoteIdent(n.alias)
		for _, f := range n.j.Fields {
			out = append(out, alias+"."+dialect.QuoteIdent(f.Column))
		}
	}
	return out
}

// joinScanTargets returns a destination per join field, in the same order
// joinSelectExprs listed them. The fields were proven to exist, be exported and
// carry no domain type at construction (validateJoins), so a violation here
// would be a framework bug, not a declaration mistake — it surfaces as an error
// rather than a panic in a row loop.
//
// One kind of column does not scan into its field's raw address: an IDENTITY of
// the joined aggregate. A join field carries no value object — domain.ID
// included — so the target's id form (BINARY(16) on mysql and sqlserver, RAW(16)
// on oracle) has to be decoded into the plain string the declaration was forced
// to be, which core.IDTextScanTarget does. Every other column keeps the raw
// address it always had.
func joinScanTargets(target any, nodes []joinNode) ([]any, error) {
	rv := reflect.ValueOf(target)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("join scan: destination must be a struct, got %s", rv.Kind())
	}
	var out []any
	for _, n := range flattenJoins(nodes) {
		j := n.j
		for _, f := range j.Fields {
			fv := rv.FieldByName(f.GoField)
			if !fv.IsValid() || !fv.CanAddr() {
				return nil, fmt.Errorf("join scan: field %q is not addressable on %s", f.GoField, rv.Type())
			}
			if targetIDKindOf(j, f) != core.IDNone {
				tgt, ok := core.IDTextScanTarget(fv)
				if !ok {
					return nil, fmt.Errorf(
						"join scan: field %q maps an identity column of %s but is %s, not a string",
						f.GoField, j.Target.Table(), fv.Type())
				}
				out = append(out, tgt)
				continue
			}
			out = append(out, fv.Addr().Interface())
		}
	}
	return out, nil
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
// trailingCols (the child's carrier columns: id + managed) are appended to the
// SELECT after the scanned struct columns, in the same order the caller's
// trailing scan targets expect. They are read into external destinations, not
// struct fields, so they never enter scanCols/scanByCol.
//
// The soft-delete gate is rendered HERE, from the scope, rather than handed in
// ready-made: the branch that decides whether anything else is in the FROM is
// the only one that can decide whether the gate's column needs qualifying, and
// a caller that guessed would guess wrong the day a join was declared.
func childScanSQL(child *TableSchema, fkCol string, childCols []string, childByCol map[string]core.FieldPath, placeholders []string, scope criteria.Scope, dialect Dialect, trailingCols []string, joins []joinNode) (string, []string, map[string]core.FieldPath) {
	ct := dialect.QuoteIdent(child.Table())
	sibs := child.Siblings()
	if len(sibs) == 0 && len(joins) == 0 {
		childFilter := core.ChildScopeFilter(scope, child, dialect, "")
		sel := dialect.QuoteIdent(fkCol) + ", " + strings.Join(quoteIdentifiers(childCols, dialect), ", ")
		for _, c := range trailingCols {
			sel += ", " + dialect.QuoteIdent(c)
		}
		sql := "SELECT " + sel +
			" FROM " + ct + " WHERE " + dialect.QuoteIdent(fkCol) + " IN (" + strings.Join(placeholders, ", ") + ") " + childFilter
		return sql, childCols, childByCol
	}
	// Something else IS in the FROM: the child's own deleted_at is ambiguous the
	// moment a join target carries one too (deleted_at/created_at/updated_at are
	// the framework's OWN columns — every archivable entity has them).
	childFilter := core.ChildScopeFilter(scope, child, dialect, ct)
	pk := dialect.QuoteIdent(child.IDColumn())
	sel := ct + "." + dialect.QuoteIdent(fkCol)
	for _, c := range childCols {
		sel += ", " + ct + "." + dialect.QuoteIdent(c)
	}
	scanCols := append([]string{}, childCols...)
	scanByCol := make(map[string]core.FieldPath, len(childByCol))
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
	// The child's own carrier columns are unambiguous under the sibling join
	// (the schema bijection), qualified to the child table like the rest.
	for _, c := range trailingCols {
		sel += ", " + ct + "." + dialect.QuoteIdent(c)
	}
	// Declared child joins, rendered from the SAME resolved nodes the child's scan
	// targets read. They come LAST in the SELECT, in the PRE-ORDER those targets
	// follow, and after the carrier columns, so the trailing target list reads
	// carrier-then-joins on both sides — at any depth.
	join.WriteString(renderJoins(joins, dialect))
	for _, n := range flattenJoins(joins) {
		qa := dialect.QuoteIdent(n.alias)
		for _, f := range n.j.Fields {
			sel += ", " + qa + "." + dialect.QuoteIdent(f.Column)
		}
	}
	sql := "SELECT " + sel + " FROM " + ct + join.String() +
		" WHERE " + ct + "." + dialect.QuoteIdent(fkCol) + " IN (" + strings.Join(placeholders, ", ") + ") " + childFilter
	return sql, scanCols, scanByCol
}

// childJoinsOf is the declared traversals hanging off ONE child, in declaration
// order — the order the child's FROM, SELECT list and scan targets follow.
func childJoinsOf(joins []Join, childTable string) []Join {
	out := make([]Join, 0, len(joins))
	for _, j := range joins {
		if j.Child != nil && j.Child.Table() == childTable {
			out = append(out, j)
		}
	}
	return out
}

// qualifyIdentifiers is quoteIdentifiers with an already-quoted table prefix on
// every name — the form every column reference takes once a join is in the FROM.
// An empty qualifier makes it quoteIdentifiers.
func qualifyIdentifiers(cols []string, dialect Dialect, qualifier string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = dialect.QuoteIdent(c)
		if qualifier != "" {
			out[i] = qualifier + "." + out[i]
		}
	}
	return out
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
