package core

import (
	"context"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// AdaptWriteOptions coverage: the typed WriteOption[T] → type-erased WriteHook
// translation, the adapted closures' single type assertions, and the two
// wiring-bug panic paths. All in-memory, no database.

// hookTestTxHandle is a framework-shaped TxHandle for the closures to carry
// through — achievable only by embedding SealedTxHandle.
type hookTestTxHandle struct{ persistence.SealedTxHandle }

// otherHookEntity is a second Entity type, distinct from *builderTestEntity, to
// drive the assertEntity mismatch panic.
type otherHookEntity struct{ domain.BaseEntity }

func (e *otherHookEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert}
}
func (e *otherHookEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// notAppContext satisfies persistence.RequestContext without being a
// *configuration.AppContext — the wiring bug assertAppContext must reject.
type notAppContext struct{ context.Context }

func (notAppContext) ID() uuid.UUID               { return uuid.Nil }
func (notAppContext) ActorSubject() string        { return persistence.AnonymousActor }
func (notAppContext) ActorIssuer() string         { return "" }
func (notAppContext) ActorClaims() map[string]any { return nil }

func TestAdaptWriteOptions_EmptyIsZeroHook(t *testing.T) {
	hook := AdaptWriteOptions[*builderTestEntity](nil)
	if hook.AfterBegin != nil || hook.BeforeCommit != nil {
		t.Error("no options must yield the zero WriteHook (both slots nil)")
	}
	hook = AdaptWriteOptions([]persistence.WriteOption[*builderTestEntity]{})
	if hook.AfterBegin != nil || hook.BeforeCommit != nil {
		t.Error("an empty slice must yield the zero WriteHook")
	}
}

func TestAdaptWriteOptions_AfterBegin(t *testing.T) {
	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	entity := &builderTestEntity{Name: "bob"}
	tx := hookTestTxHandle{}
	wantErr := errors.New("afterBegin failed")

	var gotCtx *configuration.AppContext
	var gotEntity *builderTestEntity
	var gotTx persistence.TxHandle
	hook := AdaptWriteOptions([]persistence.WriteOption[*builderTestEntity]{
		persistence.WithAfterBegin(func(ctx *configuration.AppContext, e *builderTestEntity, txh persistence.TxHandle) error {
			gotCtx, gotEntity, gotTx = ctx, e, txh
			return wantErr
		}),
	})
	if hook.AfterBegin == nil {
		t.Fatal("WithAfterBegin must populate the AfterBegin slot")
	}
	if hook.BeforeCommit != nil {
		t.Error("BeforeCommit must stay nil when only afterBegin is registered")
	}
	if err := hook.AfterBegin(appCtx, entity, tx); !errors.Is(err, wantErr) {
		t.Errorf("AfterBegin error = %v, want %v", err, wantErr)
	}
	if gotCtx != appCtx || gotEntity != entity || gotTx != persistence.TxHandle(tx) {
		t.Error("the adapted closure must pass the typed ctx/entity/tx straight through")
	}
}

func TestAdaptWriteOptions_BeforeCommit(t *testing.T) {
	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	entity := &builderTestEntity{Name: "alice"}
	id := domain.NewID("abc-123")

	var gotID domain.ID
	hook := AdaptWriteOptions([]persistence.WriteOption[*builderTestEntity]{
		persistence.WithBeforeCommit(func(ctx *configuration.AppContext, e *builderTestEntity, got domain.ID, txh persistence.TxHandle) error {
			gotID = got
			return nil
		}),
	})
	if hook.BeforeCommit == nil {
		t.Fatal("WithBeforeCommit must populate the BeforeCommit slot")
	}
	if hook.AfterBegin != nil {
		t.Error("AfterBegin must stay nil when only beforeCommit is registered")
	}
	if err := hook.BeforeCommit(appCtx, entity, id, hookTestTxHandle{}); err != nil {
		t.Errorf("BeforeCommit error = %v, want nil", err)
	}
	if gotID.Value() != "abc-123" {
		t.Errorf("closure received id %q, want abc-123", gotID.Value())
	}
}

func TestAdaptWriteOptions_WrongContextPanics(t *testing.T) {
	hook := AdaptWriteOptions([]persistence.WriteOption[*builderTestEntity]{
		persistence.WithAfterBegin(func(*configuration.AppContext, *builderTestEntity, persistence.TxHandle) error {
			return nil
		}),
	})
	assertPanics(t, "hook fired without *configuration.AppContext", func() {
		_ = hook.AfterBegin(notAppContext{Context: context.Background()}, &builderTestEntity{}, hookTestTxHandle{})
	})
}

func TestAdaptWriteOptions_WrongEntityTypePanics(t *testing.T) {
	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	hook := AdaptWriteOptions([]persistence.WriteOption[*builderTestEntity]{
		persistence.WithBeforeCommit(func(*configuration.AppContext, *builderTestEntity, domain.ID, persistence.TxHandle) error {
			return nil
		}),
	})
	assertPanics(t, "hook fired on the wrong entity type", func() {
		_ = hook.BeforeCommit(appCtx, &otherHookEntity{}, domain.NewRandomID(), hookTestTxHandle{})
	})
}
