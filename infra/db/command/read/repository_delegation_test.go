package read

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Delegation coverage for the thin read-side handles: the ctx-less
// BaseAggregateRepository verbs, the request-scoped boundReader,
// SharedBaseRoleRepository's capability surface, and the audit-reader shim.
// The heavy lifting under them (FindOne / LoadSharedBaseIdentity) is covered
// by the aggregate-loader suites; here each wrapper is proven to route.

func covRepo(queryFn func(sql string, args []any) (Rows, error)) BaseAggregateRepository[*covAgg] {
	r := NewBaseAggregateRepository[*covAgg](fakeEngine(queryFn), func() *covAgg { return &covAgg{} })
	r.WithSchema(covAggSchema)
	return r
}

func TestBaseAggregateRepository_CtxlessFinders(t *testing.T) {
	var rootSQL string
	r := covRepo(covAggQuery(covAggRootRow("r1", "Ana"), noRows, &rootSQL))
	e, err := r.FindByID(domain.NewID("r1"))
	if err != nil || e.Name != "Ana" {
		t.Fatalf("FindByID: %+v, %v", e, err)
	}

	// FindArchivedByID flips the soft-delete gate on the same SQL path.
	r = covRepo(covAggQuery(covAggRootRow("r1", "Ana"), noRows, &rootSQL))
	if _, err := r.FindArchivedByID(domain.NewID("r1")); err != nil {
		t.Fatalf("FindArchivedByID: %v", err)
	}
	if !strings.Contains(rootSQL, "deleted_at IS NOT NULL") {
		t.Errorf("archived load must gate on deleted_at IS NOT NULL, got %q", rootSQL)
	}
}

func TestBoundReader_ScopedFinders(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	r := covRepo(covAggQuery(covAggRootRow("r1", "Ana"), noRows, nil))
	reader := r.ScopedReader(ctx)
	if e, err := reader.FindByID(domain.NewID("r1")); err != nil || e.Name != "Ana" {
		t.Fatalf("scoped FindByID: %+v, %v", e, err)
	}
	if got := reader.New(); got == nil {
		t.Fatal("New must build via the factory")
	}

	r = covRepo(covAggQuery(covAggRootRow("r1", "Ana"), noRows, nil))
	archived := r.ScopedArchivedReader(ctx)
	if _, err := archived.FindArchivedByID(domain.NewID("r1")); err != nil {
		t.Fatalf("scoped FindArchivedByID: %v", err)
	}
}

func TestBoundReader_NewWithoutFactoryPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected the missing-factory panic")
		}
	}()
	b := boundReader[*covAgg]{}
	_ = b.New()
}

func TestSharedBaseRoleRepository(t *testing.T) {
	t.Run("withSchemaRequiresSharedBase", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(r.(string), "requires a schema declaring .SharedBase") {
				t.Fatalf("expected the missing-SharedBase panic, got %v", r)
			}
		}()
		repo := NewSharedBaseRoleRepository[*covAgg](fakeEngine(nil), func() *covAgg { return &covAgg{} })
		repo.WithSchema(covAggSchema) // no SharedBase declared → boot panic
	})
	t.Run("loadForSharedBaseInsertDelegates", func(t *testing.T) {
		// The identity probe misses → fresh insert (existed=false), proving the
		// call routed into LoadSharedBaseIdentity.
		query := func(sql string, _ []any) (Rows, error) { return &fakeDBRows{}, nil }
		repo := NewSharedBaseRoleRepository[*roleAggLoad](fakeEngine(query), func() *roleAggLoad { return &roleAggLoad{} })
		repo.WithSchema(roleAggLoadSchemaSD())
		ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
		fresh := &roleAggLoad{Name: "Ana", Matricula: "M1"}
		got, existed, err := repo.LoadForSharedBaseInsert(ctx, fresh)
		if err != nil || existed || got == nil {
			t.Fatalf("LoadForSharedBaseInsert: %v existed=%v err=%v", got, existed, err)
		}
	})
}

func TestNewAuditReader_RoutesThroughEngineQuerier(t *testing.T) {
	// The shim adapts the engine Querier into the audit reader; a miss on the
	// audit_events SELECT surfaces as the reader's not-found contract.
	query := func(sql string, _ []any) (Rows, error) {
		if !strings.Contains(sql, "audit_events") {
			t.Errorf("unexpected SQL through the audit shim: %q", sql)
		}
		return &fakeDBRows{}, nil
	}
	reader := NewAuditReader(fakeEngine(query))
	if _, err := reader.FindByID(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected the not-found error on an empty result")
	}
}
