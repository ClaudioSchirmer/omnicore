package integration

import (
	"context"
	"testing"
	"time"
)

// Start's happy path: resolved receivers fold into groups and one consumer
// goroutine spins per group. The broker is unroutable and the context is
// cancelled, so each reader loop exits immediately — the goroutine spawn,
// the worker accounting, and the drain handshake are what is under test
// (the live message loop remains integration-only).
func TestConsumerPool_StartSpawnsGroupsAndDrains(t *testing.T) {
	reg := NewRegistry()
	reg.From("partners").On("onboarded", fakeRequest{}, &fakeHandler{})
	cfg := &Config{Subscribes: map[string]SubscribeEntry{
		"partners": {
			Topic:         "partners.events",
			ConsumerGroup: "orders-int",
			Workers:       1,
			StartFrom:     "latest",
			Events:        map[string]SubscribeEvent{"onboarded": {EventType: "PartnerOnboarded"}},
		},
	}}
	p := NewConsumerPool(reg, cfg, nil, []string{"127.0.0.1:1"}, nil, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if err := p.Shutdown(drainCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// A receiver resolving to workers<=0 defaults to NumCPU at Start.
func TestConsumerPool_StartDefaultsWorkers(t *testing.T) {
	reg := NewRegistry()
	reg.From("partners").On("onboarded", fakeRequest{}, &fakeHandler{})
	cfg := &Config{Subscribes: map[string]SubscribeEntry{
		"partners": {
			Topic:         "partners.events",
			ConsumerGroup: "orders-int",
			StartFrom:     "latest", // Workers omitted → 0 → NumCPU default
			Events:        map[string]SubscribeEvent{"onboarded": {EventType: "PartnerOnboarded"}},
		},
	}}
	p := NewConsumerPool(reg, cfg, nil, []string{"127.0.0.1:1"}, nil, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := reg.Receivers()[0].workers; got <= 0 {
		t.Errorf("workers must default to NumCPU, got %d", got)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	_ = p.Shutdown(drainCtx)
}
