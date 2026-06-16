package integration

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/google/uuid"
)

func TestDispatchLazyValidationUnknownEventKey(t *testing.T) {
	Reset()
	defer Reset()
	Configure(&Config{}, nil, nil)

	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "unknownKey", map[string]any{"x": 1})
	if err == nil {
		t.Fatal("expected error for unknown eventKey")
	}
	if !errors.Is(err, ErrIntegrationEventNotConfigured) {
		t.Fatalf("expected ErrIntegrationEventNotConfigured, got %v", err)
	}
}

func TestDispatchWithoutConfigure(t *testing.T) {
	Reset()
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "anything", map[string]any{})
	if !errors.Is(err, ErrIntegrationConfigNotInitialized) {
		t.Fatalf("expected ErrIntegrationConfigNotInitialized, got %v", err)
	}
}

func TestDispatchAggregateIDRequiredWhenYAMLDeclaresAggregate(t *testing.T) {
	Reset()
	defer Reset()
	Configure(&Config{
		Publishes: PublishConfig{
			Events: map[string]PublishEvent{
				"userActivated": {
					EventType: "UserActivated",
					Aggregate: "User",
					Version:   1,
				},
			},
		},
	}, nil, nil)

	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "userActivated", map[string]any{"email": "x@y"})
	if !errors.Is(err, ErrIntegrationAggregateIDRequired) {
		t.Fatalf("expected ErrIntegrationAggregateIDRequired, got %v", err)
	}
}

func TestDispatchOptions(t *testing.T) {
	o := &dispatchOpts{}
	WithCorrelation(uuid.New())(o)
	if !o.hasCorrelation {
		t.Fatal("WithCorrelation should set hasCorrelation")
	}
	WithCausation(uuid.New())(o)
	if !o.hasCausation {
		t.Fatal("WithCausation should set hasCausation")
	}
}

func TestResolveCorrelationFallsBackToCtx(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	id := uuid.New()
	ctx.SetCorrelationID(id)
	got := resolveCorrelation(ctx, &dispatchOpts{})
	if got != id {
		t.Fatalf("expected fallback to ctx correlation %v, got %v", id, got)
	}
}

func TestResolveCausationFallsBackToCtx(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	id := uuid.New()
	ctx.SetCausationID(id)
	got := resolveCausation(ctx, &dispatchOpts{})
	if got != id {
		t.Fatalf("expected fallback to ctx causation %v, got %v", id, got)
	}
}

func TestResolveActorAnonymousSentinel(t *testing.T) {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	got := resolveActor(ctx)
	if got != "anonymous" {
		t.Fatalf("expected anonymous sentinel, got %q", got)
	}
}
