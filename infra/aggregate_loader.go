package infra

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/jackc/pgx/v5"
)

// RootScanner deserializes a Postgres row into a populated entity T.
// The loader passes pgx.Row (single row). The scanner does row.Scan(&t.field1, ...)
// and returns the entity. It does not need to set the ID — the loader does it via t.SetID(id).
type RootScanner[T domain.Entity] func(pgx.Row) (T, error)

// ChildScanner deserializes a Postgres row into an AggregateValueObject.
type ChildScanner func(pgx.Rows) (domain.AggregateValueObject, error)

// childAutoSpec stores, per type name, the configuration for loading a child
// via auto-scan: reflect type + computed columns + factory that creates an
// empty AggregateValueObject that can be filled by scan.
//
// Phase 19: table is NO LONGER stored here — it is resolved at Load time via
// resolveChildTable(typeName, cfg), where cfg.ChildTableOverrides[typeName]
// takes priority over InferTableName(typeName).
type childAutoSpec struct {
	columns  []string
	scanInto func(pgx.Rows) (domain.AggregateValueObject, error)
}

// AggregateLoader[T] loads an aggregate root + active children from Postgres.
// Phase 19: convention-based — root and children tables inferred from the
// Go type via InferTableName; child FK inferred from the root type via
// InferForeignKey. Overrides via WithConfig(RepoConfig).
//
// Two paths coexist:
//
//  1. Auto-scan (default) — framework discovers columns via reflection on the
//     exported fields of T (and of child types), generates explicit SELECT,
//     scans directly into the addresses. Children declared via WithChild[V](loader).
//
//  2. Manual scanners — service provides RootScanner/ChildScanner via
//     WithRootScanner/WithChildScanner. Required for non-trivial queries.
//
// Typical usage (auto-scan):
//
//	loader := infra.NewAggregateLoader[*appdomain.User](pg, func() *appdomain.User {
//	    return &appdomain.User{}
//	}).WithContextName("User")
//	loader = infra.WithChild[appdomain.Address](loader)
//
//	func (r *UserRepository) FindByID(id domain.ID) (*appdomain.User, error) {
//	    return loader.Load(context.Background(), id)
//	}
type AggregateLoader[T domain.Entity] struct {
	pg            *Postgres
	newEntity     func() T
	contextName   string
	config        RepoConfig
	rootScanner   RootScanner[T]
	childScanners map[string]ChildScanner
	childAuto     map[string]childAutoSpec
}

// NewAggregateLoader initializes a loader.
func NewAggregateLoader[T domain.Entity](pg *Postgres, newEntity func() T) *AggregateLoader[T] {
	return &AggregateLoader[T]{
		pg:            pg,
		newEntity:     newEntity,
		childScanners: map[string]ChildScanner{},
		childAuto:     map[string]childAutoSpec{},
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

// WithConfig attaches the BaseRepository's RepoConfig so the loader can resolve
// table/FK/column overrides. Calling with the same &r.Config shares the
// configuration between write and read sides of a Repository.
func (l *AggregateLoader[T]) WithConfig(cfg RepoConfig) *AggregateLoader[T] {
	l.config = cfg
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

// WithChild registers a child via auto-scan. Phase 19: table and FK are inferred
// at Load time — no need to declare the table here.
//
//	infra.WithChild[Address](loader)   // typeName="Address", inferred table "addresses"
//
// V must be the concrete AggregateValueObject type (not an interface).
// Address is a value type: WithChild[Address](loader).
func WithChild[V domain.AggregateValueObject, T domain.Entity](
	l *AggregateLoader[T],
) *AggregateLoader[T] {
	t := reflect.TypeOf((*V)(nil)).Elem()
	cols := domainColumns(t)
	typeName := t.Name()
	l.childAuto[typeName] = childAutoSpec{
		columns: cols,
		scanInto: func(rows pgx.Rows) (domain.AggregateValueObject, error) {
			vp := reflect.New(t)
			if err := scanRowIntoStruct(rows, vp.Interface(), cols); err != nil {
				return nil, err
			}
			return vp.Elem().Interface().(domain.AggregateValueObject), nil
		},
	}
	return l
}

// Load executes SELECT root + N SELECT children and assembles the entity. Filters
// deleted_at IS NULL; missing root → *DomainError with
// RecordNotFoundNotification (HTTP 404).
//
// Phase 19: root table inferred via resolveTable(sample, &l.config); same
// for children via resolveChildTable. RepoConfig overrides applied when
// present.
func (l *AggregateLoader[T]) Load(ctx context.Context, id domain.ID) (T, error) {
	return l.load(ctx, id, false)
}

// LoadIncludingArchived returns the *archived* aggregate — root whose deleted_at
// IS NOT NULL. Used by UnarchiveCommandHandler to hydrate the archived snapshot
// before dispatch; the cascade SQL in aggregate_persister sees the children's
// typeNames via root.AllAggregateItems() and restores the corresponding
// archived child rows.
//
// Phase 20.3: an active root (deleted_at IS NULL) surfaces as NotFound — the
// method name is literal ("archived"), and attempting to unarchive an active
// record is semantically "that archived does not exist", not a silent no-op.
// Children continue to be loaded without the deleted_at filter so that
// AllAggregateItems() has at least one item per typeName that the persister
// needs to cascade (the cascade SQL already filters `AND deleted_at IS NOT NULL`
// in the UPDATE).
func (l *AggregateLoader[T]) LoadIncludingArchived(ctx context.Context, id domain.ID) (T, error) {
	return l.load(ctx, id, true)
}

func (l *AggregateLoader[T]) load(ctx context.Context, id domain.ID, includeArchived bool) (T, error) {
	sample := l.newEntity()
	table := resolveTable(sample, &l.config)

	root, err := l.loadRoot(ctx, table, id, sample, includeArchived)
	if err != nil {
		return *new(T), err
	}
	root.SetID(id)

	provider, ok := any(root).(domain.AggregateRootProvider)
	if !ok {
		return root, nil
	}
	if err := l.loadChildren(ctx, id, provider, reflect.TypeOf(sample), includeArchived); err != nil {
		return *new(T), err
	}
	return root, nil
}

// loadRoot dispatches to the manual rootScanner or auto-scan. includeArchived
// inverts the filter: `deleted_at IS NULL` (default, Load) → `deleted_at IS NOT
// NULL` (LoadIncludingArchived). The method is "find archived", so active rows
// surface as pgx.ErrNoRows → NotFound — blocks unarchive of an active record
// at the source level (loader), without needing a check in the upper layer.
func (l *AggregateLoader[T]) loadRoot(ctx context.Context, table string, id domain.ID, sample T, includeArchived bool) (T, error) {
	filter := "AND deleted_at IS NULL"
	if includeArchived {
		filter = "AND deleted_at IS NOT NULL"
	}
	if l.rootScanner != nil {
		sql := fmt.Sprintf(
			"SELECT * FROM %s WHERE id = $1 %s",
			validIdentifier(table), filter,
		)
		row := l.pg.Pool().QueryRow(ctx, sql, id.Value())
		root, err := l.rootScanner(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return *new(T), domain.NotFoundError(l.effectiveContextName(), "id", id.Value())
			}
			return *new(T), err
		}
		return root, nil
	}

	cols := domainColumnsOf(sample)
	if len(cols) == 0 {
		return *new(T), fmt.Errorf(
			"AggregateLoader[%s]: auto-scan found no columns — %T exposes no domain fields. "+
				"Use WithRootScanner to provide a manual scanner",
			l.effectiveContextName(), sample,
		)
	}
	sql := fmt.Sprintf(
		"SELECT %s FROM %s WHERE id = $1 %s",
		strings.Join(quoteIdentifiers(cols), ", "),
		validIdentifier(table), filter,
	)
	root := l.newEntity()
	row := l.pg.Pool().QueryRow(ctx, sql, id.Value())
	if err := scanRowIntoStruct(row, any(root), cols); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return *new(T), domain.NotFoundError(l.effectiveContextName(), "id", id.Value())
		}
		return *new(T), err
	}
	return root, nil
}

// loadChildren executes one SELECT per configured typeName (manual or auto) and
// aggregates results via AggregateConstructor. Phase 19: table/FK inferred.
func (l *AggregateLoader[T]) loadChildren(
	ctx context.Context, rootID domain.ID,
	provider domain.AggregateRootProvider,
	rootType reflect.Type,
	includeArchived bool,
) error {
	filter := "AND deleted_at IS NULL"
	if includeArchived {
		filter = ""
	}
	if len(l.childScanners) == 0 && len(l.childAuto) == 0 {
		return nil
	}

	var avos []domain.AggregateValueObject

	seen := map[string]bool{}
	for typeName := range l.childScanners {
		seen[typeName] = true
	}
	for typeName := range l.childAuto {
		seen[typeName] = true
	}

	for typeName := range seen {
		childTable := resolveChildTable(typeName, &l.config)
		fkCol := resolveChildFK(rootType, typeName, &l.config)

		var (
			sql     string
			scanner func(pgx.Rows) (domain.AggregateValueObject, error)
		)
		if manual, ok := l.childScanners[typeName]; ok {
			sql = fmt.Sprintf(
				"SELECT * FROM %s WHERE %s = $1 %s",
				validIdentifier(childTable),
				validIdentifier(fkCol),
				filter,
			)
			scanner = manual
		} else {
			auto := l.childAuto[typeName]
			if len(auto.columns) == 0 {
				return fmt.Errorf(
					"AggregateLoader[%s] child %q: auto-scan found no columns — type has no domain fields",
					l.effectiveContextName(), typeName,
				)
			}
			sql = fmt.Sprintf(
				"SELECT %s FROM %s WHERE %s = $1 %s",
				strings.Join(quoteIdentifiers(auto.columns), ", "),
				validIdentifier(childTable),
				validIdentifier(fkCol),
				filter,
			)
			scanner = auto.scanInto
		}

		rows, err := l.pg.Pool().Query(ctx, sql, rootID.Value())
		if err != nil {
			return err
		}
		for rows.Next() {
			avo, err := scanner(rows)
			if err != nil {
				rows.Close()
				return err
			}
			avos = append(avos, avo)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	if len(avos) > 0 {
		provider.GetAggregateRoot().AggregateConstructor(avos)
	}
	return nil
}

// domainColumnsOf extracts columns from an entity instance (which may be a pointer
// or value). Wrapper over domainColumns.
func domainColumnsOf(v any) []string {
	t := reflect.TypeOf(v)
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return domainColumns(t)
}

// quoteIdentifiers escapes names via validIdentifier (SQL-injection defense).
func quoteIdentifiers(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = validIdentifier(c)
	}
	return out
}
