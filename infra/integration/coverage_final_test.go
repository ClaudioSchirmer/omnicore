package integration

import (
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"

	"github.com/google/uuid"
)

// --- Dispatch remaining branches -------------------------------------------

// TestDispatch_ConfigNotInitialized drives the snapshot()==nil branch: no
// Configure (Reset clears the singleton) → ErrIntegrationConfigNotInitialized.
func TestDispatch_ConfigNotInitialized(t *testing.T) {
	Reset()
	defer Reset()
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "k", map[string]any{"x": 1})
	if !errors.Is(err, ErrIntegrationConfigNotInitialized) {
		t.Fatalf("expected ErrIntegrationConfigNotInitialized, got %v", err)
	}
}

// TestDispatch_NilCfgInClient drives the c.cfg == nil branch: Configure with a
// nil Config leaves the client present but cfg nil.
func TestDispatch_NilCfgInClient(t *testing.T) {
	Reset()
	defer Reset()
	Configure(nil, nil, nil)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "k", map[string]any{"x": 1})
	if !errors.Is(err, ErrIntegrationConfigNotInitialized) {
		t.Fatalf("expected ErrIntegrationConfigNotInitialized for nil cfg, got %v", err)
	}
}

// TestDispatch_EventKeyNotConfigured drives the LookupPublish miss branch.
func TestDispatch_EventKeyNotConfigured(t *testing.T) {
	Reset()
	defer Reset()
	Configure(&Config{Publishes: PublishConfig{Events: map[string]PublishEvent{
		"known": {EventType: "T"},
	}}}, nil, nil)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "unknown", map[string]any{"x": 1})
	if !errors.Is(err, ErrIntegrationEventNotConfigured) {
		t.Fatalf("expected ErrIntegrationEventNotConfigured, got %v", err)
	}
}

// TestDispatch_AggregateIDRequired drives the aggregate-declared-but-missing
// branch: the YAML entry names an aggregate, so WithAggregateID is mandatory.
func TestDispatch_AggregateIDRequired(t *testing.T) {
	Reset()
	defer Reset()
	Configure(&Config{Publishes: PublishConfig{Events: map[string]PublishEvent{
		"k": {EventType: "T", Aggregate: "User"},
	}}}, nil, nil)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	err := Dispatch(ctx, "k", map[string]any{"x": 1})
	if !errors.Is(err, ErrIntegrationAggregateIDRequired) {
		t.Fatalf("expected ErrIntegrationAggregateIDRequired, got %v", err)
	}
}

// --- config.Validate remaining branch (workers < 0) ------------------------

func TestConfigValidate_NegativeWorkers(t *testing.T) {
	c := &Config{
		Subscribes: map[string]SubscribeEntry{
			"src": {
				Topic:     "t",
				StartFrom: "latest",
				Workers:   -2, // per-source negative
				Events:    map[string]SubscribeEvent{"y": {EventType: "Y"}},
			},
		},
		Defaults: SubscriberDefaults{StartFrom: "latest", Workers: -5}, // defaults negative
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation errors for negative workers")
	}
	if !strings.Contains(err.Error(), "subscribes.src.workers") {
		t.Errorf("expected per-source workers diagnostic, got: %v", err)
	}
	if !strings.Contains(err.Error(), "defaults.workers") {
		t.Errorf("expected defaults workers diagnostic, got: %v", err)
	}
}

// Guard: a valid domain.ID round-trips through WithAggregateID into the row so
// the aggregate path's happy validation (entry.Aggregate set + flag present)
// reaches the marshal step, isolating the marshal-error branch already tested.
func TestDispatch_AggregatePresentReachesMarshal(t *testing.T) {
	Reset()
	defer Reset()
	Configure(&Config{Publishes: PublishConfig{Events: map[string]PublishEvent{
		"k": {EventType: "T", Aggregate: "User"},
	}}}, nil, nil)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	// Aggregate present + unmarshalable payload → marshal error (not the
	// aggregate-required error), proving the aggregate guard passed.
	err := Dispatch(ctx, "k", make(chan int), WithAggregateID(domain.NewID(uuid.New().String())))
	if err == nil || !strings.Contains(err.Error(), "marshal payload") {
		t.Fatalf("expected marshal error after aggregate guard passed, got %v", err)
	}
}
