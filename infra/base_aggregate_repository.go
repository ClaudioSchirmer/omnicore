package infra

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
)

// BaseAggregateRepository[T] composes the canonical building blocks of an
// aggregate-aware Repository: embeds BaseRepository[T] (5 writes + New()) and
// holds an internal (public) *AggregateLoader[T] already initialized. Promotes
// FindByID, FindArchivedByID, New() and Scope — satisfies
// persistence.ScopedRepository[T] and domain.ArchivedFinder[T] without the
// service writing manual wrappers.
//
// When to use (common case):
//
//   - Aggregate-aware Repository with symmetric universal cascade.
//   - FindByID via schema-driven auto-scan or a manual scanner on the Loader.
//   - Children declared on the TableSchema via Child(...); manual decoding via
//     WithChildScanner when a child needs a non-trivial scan.
//
// When NOT to use (keep the old pattern of direct BaseRepository[T] embed
// + a separate *AggregateLoader[T]):
//
//   - Custom magic pattern: two distinct read paths on the same aggregate
//     (e.g. read replica + main, or materialized view).
//   - Custom composition where the Loader is not the canonical source of
//     FindByID.
//
// Manual and composed coexist — choose per Repository. ConstraintBinding and
// Config still live on the embedded BaseRepository; setting them after New is
// equivalent to the direct struct-literal of BaseRepository[T].
type BaseAggregateRepository[T domain.Entity] struct {
	BaseRepository[T]
	Loader *AggregateLoader[T]
}

// NewBaseAggregateRepository creates the composition. The newEntity factory is
// shared as a single source between BaseRepository.NewEntity (for Repo.New())
// and AggregateLoader (for entity scan/init) — aligned with the Phase 18
// finding.
//
// ContextName stays empty in both by default — derived from type T via
// TypeName[T]() on first use (Phase: lazy ContextName). Explicit override
// remains available: r.ContextName = "..." + r.Loader.WithContextName("...").
func NewBaseAggregateRepository[T domain.Entity](pg *Postgres, newEntity func() T) BaseAggregateRepository[T] {
	return BaseAggregateRepository[T]{
		BaseRepository: BaseRepository[T]{
			Postgres:  pg,
			NewEntity: newEntity,
		},
		Loader: NewAggregateLoader[T](pg, newEntity),
	}
}

// WithSchema declares the mandatory TableSchema once and threads it into BOTH
// the write binding (BaseRepository.Schema) and the read loader
// (Loader.WithSchema) — one declaration feeds write, criteria and scan. The
// Modes() ⟺ SoftDelete and the aggregate-depth (no grandchildren) boot checks
// run here; the field-existence + bijection checks already ran while the
// TableSchema was built. A violation panics at construction, not on the first
// request.
func (r *BaseAggregateRepository[T]) WithSchema(schema *TableSchema) *BaseAggregateRepository[T] {
	// PK-declared + aggregate-depth + Modes() ⟺ SoftDelete run in the shared
	// BaseRepository.WithSchema (which also sets r.Schema). The aggregate path
	// adds the boundary cross-check below + threads the schema into the loader.
	r.BaseRepository.WithSchema(schema)
	r.validateDeclaredChildren(schema)
	r.Loader.WithSchema(schema)
	return r
}

// validateDeclaredChildren asserts the aggregate boundary the domain declares
// (root.AggregateChildren()) and the children the TableSchema declares
// (.Child(...)) name the SAME set of types. The two are independent
// declarations — one in domain vocabulary, one in infra — and both are known
// at construction. A drift between them slips through to runtime: a type in
// AggregateChildren() without a matching .Child(...) errors per-write deep in
// the persister; a .Child(...) without a matching AggregateChildren() entry is
// silently load-only (the loader bypasses the domain type-guard). Catch both
// at boot. A non-aggregate entity (no AggregateRootProvider) contributes the
// empty set, so a schema that declares children for a flat entity is flagged
// too.
func (r *BaseAggregateRepository[T]) validateDeclaredChildren(schema *TableSchema) {
	declared := map[string]struct{}{}
	if p, ok := any(r.New()).(domain.AggregateRootProvider); ok {
		for _, child := range p.AggregateChildren() {
			t := reflect.TypeOf(child)
			for t != nil && t.Kind() == reflect.Ptr {
				t = t.Elem()
			}
			if t != nil {
				declared[t.Name()] = struct{}{}
			}
		}
	}

	var missingSchema []string // declared in AggregateChildren() but no .Child(...)
	for name := range declared {
		if _, ok := schema.children[name]; !ok {
			missingSchema = append(missingSchema, name)
		}
	}
	var missingDomain []string // declared via .Child(...) but not in AggregateChildren()
	for name := range schema.children {
		if _, ok := declared[name]; !ok {
			missingDomain = append(missingDomain, name)
		}
	}
	if len(missingSchema) == 0 && len(missingDomain) == 0 {
		return
	}
	sort.Strings(missingSchema)
	sort.Strings(missingDomain)
	msg := fmt.Sprintf("infra.TableSchema(%s): aggregate boundary mismatch between the domain's "+
		"AggregateChildren() and the schema's Child(...) declarations", schema.Table())
	if len(missingSchema) > 0 {
		msg += fmt.Sprintf("\n  declared in AggregateChildren() but missing a .Child(...) schema: %s",
			strings.Join(missingSchema, ", "))
	}
	if len(missingDomain) > 0 {
		msg += fmt.Sprintf("\n  declared via .Child(...) but absent from AggregateChildren(): %s",
			strings.Join(missingDomain, ", "))
	}
	panic(msg)
}

// FindByID resolves the primary-key lookup through the entity search engine —
// criteria.ByID(id) is the single SQL-building path. Uses context.Background()
// (the contract carries no ctx); a caller that needs a custom context uses the
// Loader's FindOne directly.
func (r *BaseAggregateRepository[T]) FindByID(id domain.ID) (T, error) {
	return r.Loader.FindOne(context.Background(), criteria.ByID(id))
}

// FindArchivedByID loads the archived aggregate (deleted_at IS NOT NULL) via the
// same engine with the OnlyArchived scope. Satisfies domain.ArchivedFinder[T],
// which UnarchiveCommandHandler consumes to hydrate the archived aggregate
// (children loaded unfiltered under the archived scope) before cascading
// unarchive on the children.
func (r *BaseAggregateRepository[T]) FindArchivedByID(id domain.ID) (T, error) {
	return r.Loader.FindOne(context.Background(), criteria.ByID(id).OnlyArchived())
}
