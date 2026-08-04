package handlers

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// scopedProbeService implements persistence.ScopedServiceProvider: ScopedService
// returns a per-request VIEW carrying the request ctx. An Auto handler must pass
// that VIEW (not the wired singleton) into the entity's BuildRules, so a
// ctx-dependent probe runs under the request deadline / cancellation / trace.
type scopedProbeService struct {
	domain.ServiceBase
	boundCtx *configuration.AppContext
}

func (s *scopedProbeService) ScopedService(c *configuration.AppContext) domain.Service {
	return &scopedProbeService{boundCtx: c}
}

func TestInsertCommandHandler_ScopesServiceWithRequestCtx(t *testing.T) {
	repo := newMockRepo()
	svc := &scopedProbeService{}
	entity := &testEntity{Name: "scoped"}
	ctx := testCtx()
	h := &InsertCommandHandler[*testEntity, *capturingInsertCmd, fwresults.None]{
		Repo: repo, Service: svc,
	}

	if _, err := h.Handle(ctx, &capturingInsertCmd{entity: entity}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	seen, ok := entity.BuildRulesSeenService.(*scopedProbeService)
	if !ok {
		t.Fatalf("BuildRules saw %T, want the scoped *scopedProbeService view", entity.BuildRulesSeenService)
	}
	if seen == svc {
		t.Error("handler passed the wired singleton to BuildRules; expected the per-request scoped view")
	}
	if seen.boundCtx != ctx {
		t.Errorf("scoped view did not carry the request ctx into BuildRules; got %v, want %v", seen.boundCtx, ctx)
	}
	if svc.boundCtx != nil {
		t.Error("the wired singleton was mutated; ScopedService must return a copy")
	}
}

// A Service that does NOT implement ScopedServiceProvider must still reach
// BuildRules unchanged (the pass-through path in persistence.ScopeService).
func TestInsertCommandHandler_NonScopedServiceReachesBuildRulesUnchanged(t *testing.T) {
	repo := newMockRepo()
	svc := &testService{}
	entity := &testEntity{Name: "plain"}
	h := &InsertCommandHandler[*testEntity, *capturingInsertCmd, fwresults.None]{
		Repo: repo, Service: svc,
	}

	if _, err := h.Handle(testCtx(), &capturingInsertCmd{entity: entity}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if entity.BuildRulesSeenService != domain.Service(svc) {
		t.Errorf("a non-provider Service must reach BuildRules unchanged; got %v", entity.BuildRulesSeenService)
	}
}
