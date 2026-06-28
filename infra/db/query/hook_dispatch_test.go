package query

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The Tier 1 unit-level coverage exercises the persister-side hook
// dispatch helpers in isolation:
//
//   - core.AdaptWriteOptions translates typed persistence.WriteOption[T] into
//     the type-erased core.WriteHook the pg.Postgres methods consume.
//   - The closure layer pays a single per-call type assertion against T
//     and *configuration.AppContext.
//
// In-TX semantics (rollback, slog.Warn emission, audit/outbox row
// counts) require a real pgx.Tx — those branches live in the
// integration suite (postgres_integration_hooks_test.go +
// aggregate_persister_integration_hooks_test.go).

// hookFlatEntity is the test stand-in T for core.AdaptWriteOptions[*hookFlatEntity].
type hookFlatEntity struct {
	domain.BaseEntity
	Name string
}

func (*hookFlatEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (*hookFlatEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// --- AdaptWriteOptions: empty fast path -------------------------------------

func TestAdaptWriteOptions_Empty(t *testing.T) {
	hook := core.AdaptWriteOptions[*hookFlatEntity](nil)
	if hook.AfterBegin != nil {
		t.Error("expected nil AfterBegin on empty opts")
	}
	if hook.BeforeCommit != nil {
		t.Error("expected nil BeforeCommit on empty opts")
	}
}

// --- AdaptWriteOptions: typed → erased closure layer -----------------------

func TestAdaptWriteOptions_AfterBeginFires(t *testing.T) {
	var calledWith *hookFlatEntity
	typed := persistence.AfterBeginHook[*hookFlatEntity](func(_ *configuration.AppContext, e *hookFlatEntity, _ persistence.TxHandle) error {
		calledWith = e
		return nil
	})
	hook := core.AdaptWriteOptions[*hookFlatEntity]([]persistence.WriteOption[*hookFlatEntity]{
		persistence.WithAfterBegin[*hookFlatEntity](typed),
	})
	if hook.AfterBegin == nil {
		t.Fatal("expected AfterBegin populated")
	}
	want := &hookFlatEntity{Name: "alice"}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	if err := hook.AfterBegin(ctx, want, nil); err != nil {
		t.Fatalf("AfterBegin returned %v", err)
	}
	if calledWith != want {
		t.Errorf("expected closure to receive the entity verbatim; got %+v want %+v", calledWith, want)
	}
}

func TestAdaptWriteOptions_BeforeCommitFiresWithID(t *testing.T) {
	wantID := domain.NewRandomID()
	var calledWith domain.ID
	typed := persistence.BeforeCommitHook[*hookFlatEntity](func(_ *configuration.AppContext, _ *hookFlatEntity, id domain.ID, _ persistence.TxHandle) error {
		calledWith = id
		return nil
	})
	hook := core.AdaptWriteOptions[*hookFlatEntity]([]persistence.WriteOption[*hookFlatEntity]{
		persistence.WithBeforeCommit[*hookFlatEntity](typed),
	})
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	if err := hook.BeforeCommit(ctx, &hookFlatEntity{}, wantID, nil); err != nil {
		t.Fatalf("BeforeCommit returned %v", err)
	}
	if calledWith != wantID {
		t.Errorf("BeforeCommit closure received id %v, want %v", calledWith, wantID)
	}
}

// --- AdaptWriteOptions: error propagates verbatim --------------------------

func TestAdaptWriteOptions_AfterBeginErrorPropagates(t *testing.T) {
	wantErr := errors.New("rejects")
	typed := persistence.AfterBeginHook[*hookFlatEntity](func(*configuration.AppContext, *hookFlatEntity, persistence.TxHandle) error {
		return wantErr
	})
	hook := core.AdaptWriteOptions[*hookFlatEntity]([]persistence.WriteOption[*hookFlatEntity]{
		persistence.WithAfterBegin[*hookFlatEntity](typed),
	})
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	if got := hook.AfterBegin(ctx, &hookFlatEntity{}, nil); !errors.Is(got, wantErr) {
		t.Errorf("expected verbatim error, got %v", got)
	}
}

func TestAdaptWriteOptions_BeforeCommitErrorPropagates(t *testing.T) {
	wantErr := errors.New("rejects")
	typed := persistence.BeforeCommitHook[*hookFlatEntity](func(*configuration.AppContext, *hookFlatEntity, domain.ID, persistence.TxHandle) error {
		return wantErr
	})
	hook := core.AdaptWriteOptions[*hookFlatEntity]([]persistence.WriteOption[*hookFlatEntity]{
		persistence.WithBeforeCommit[*hookFlatEntity](typed),
	})
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	if got := hook.BeforeCommit(ctx, &hookFlatEntity{}, domain.NewRandomID(), nil); !errors.Is(got, wantErr) {
		t.Errorf("expected verbatim error, got %v", got)
	}
}

// --- assertAppContext / assertEntity: panic on wiring bugs ----------------

func TestAssertAppContext_PanicsOnNonAppContext(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when ctx is not *AppContext")
		}
	}()
	// Trigger by calling through the adapter closure with a non-AppContext ctx.
	typed := persistence.AfterBeginHook[*hookFlatEntity](func(*configuration.AppContext, *hookFlatEntity, persistence.TxHandle) error {
		return nil
	})
	hook := core.AdaptWriteOptions[*hookFlatEntity]([]persistence.WriteOption[*hookFlatEntity]{
		persistence.WithAfterBegin[*hookFlatEntity](typed),
	})
	_ = hook.AfterBegin(nonAppContext{}, &hookFlatEntity{}, nil)
}

func TestAssertEntity_PanicsOnTypeMismatch(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when source does not match T")
		}
	}()
	typed := persistence.AfterBeginHook[*hookFlatEntity](func(*configuration.AppContext, *hookFlatEntity, persistence.TxHandle) error {
		return nil
	})
	hook := core.AdaptWriteOptions[*hookFlatEntity]([]persistence.WriteOption[*hookFlatEntity]{
		persistence.WithAfterBegin[*hookFlatEntity](typed),
	})
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	_ = hook.AfterBegin(ctx, &wrongEntity{}, nil)
}

// nonAppContext satisfies persistence.RequestContext without being the configured
// *AppContext type — used to drive the assertAppContext panic path.
type nonAppContext struct {
	persistence.RequestContext
}

// wrongEntity satisfies domain.Entity but does not match the T the
// adapter was parameterized with — drives the assertEntity panic path.
type wrongEntity struct {
	domain.BaseEntity
	Other string
}

func (*wrongEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert}
}
func (*wrongEntity) BuildRules(string, domain.Service, *domain.Rules) {}
