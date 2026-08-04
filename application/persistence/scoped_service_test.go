package persistence

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// plainService satisfies domain.Service (the marker) WITHOUT the ctx-binding
// capability — the pass-through shape ScopeService must return unchanged.
type plainService struct{ domain.ServiceBase }

// scopedSvc adds ScopedServiceProvider. ScopedService returns a per-request
// VIEW (a distinct value) closing over ctx and never mutates the receiver.
type scopedSvc struct {
	domain.ServiceBase
	boundCtx *configuration.AppContext
}

func (s *scopedSvc) ScopedService(c *configuration.AppContext) domain.Service {
	return &scopedSvc{boundCtx: c}
}

func TestScopeService_NilStaysNil(t *testing.T) {
	if got := ScopeService(nil, ctx()); got != nil {
		t.Errorf("ScopeService(nil, ctx) = %v, want nil", got)
	}
}

func TestScopeService_PlainServiceIsPassedThroughUnchanged(t *testing.T) {
	svc := &plainService{}
	if got := ScopeService(svc, ctx()); got != domain.Service(svc) {
		t.Error("ScopeService must return a non-provider service unchanged (same value)")
	}
}

func TestScopeService_ProviderBindsRequestCtxWithoutMutatingReceiver(t *testing.T) {
	c := ctx()
	svc := &scopedSvc{}

	got := ScopeService(svc, c)

	scoped, ok := got.(*scopedSvc)
	if !ok {
		t.Fatalf("ScopeService returned %T, want *scopedSvc", got)
	}
	if scoped == svc {
		t.Error("ScopeService must return a per-request VIEW, not the singleton receiver")
	}
	if scoped.boundCtx != c {
		t.Errorf("scoped view did not close over the request ctx; got %v, want %v", scoped.boundCtx, c)
	}
	if svc.boundCtx != nil {
		t.Error("ScopeService must NOT mutate the receiver (the wired singleton)")
	}
}
