package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestNewConsumerPool_DefaultsLogger(t *testing.T) {
	// nil logger must fall back to slog.Default(); nil pg/pipe are allowed
	// (the struct is just wired here, no IO).
	p := NewConsumerPool(NewRegistry(), &Config{}, nil, []string{"localhost:9092"}, nil, nil)
	if p == nil {
		t.Fatal("NewConsumerPool returned nil")
	}
	if p.logger == nil {
		t.Fatal("nil logger must default to slog.Default()")
	}
	if p.stop == nil {
		t.Fatal("stop channel must be initialized")
	}
}

func TestConsumerPool_Start_NilOrEmptyRegistry(t *testing.T) {
	// nil pool
	var nilPool *ConsumerPool
	if err := nilPool.Start(context.Background()); err != nil {
		t.Fatalf("nil pool Start must be a no-op nil, got %v", err)
	}
	// empty registry → no consumer goroutines, returns nil
	p := NewConsumerPool(NewRegistry(), &Config{}, nil, nil, nil, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("empty-registry Start must return nil, got %v", err)
	}
}

func TestConsumerPool_Start_ResolveErrorAborts(t *testing.T) {
	// A receiver whose source key is absent from YAML must abort Start at
	// resolveAgainstYAML — before any Kafka reader is opened.
	reg := NewRegistry()
	reg.From("missing-source").On("evt", fakeRequest{}, &fakeHandler{})
	p := NewConsumerPool(reg, &Config{Subscribes: map[string]SubscribeEntry{}}, nil, nil, nil, nil)
	err := p.Start(context.Background())
	if err == nil {
		t.Fatal("Start must abort when a receiver's source is undeclared")
	}
}

func TestConsumerPool_Shutdown(t *testing.T) {
	// nil pool
	var nilPool *ConsumerPool
	if err := nilPool.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil pool Shutdown must return nil, got %v", err)
	}
	// never-started pool: no workers, no inflight → drains immediately.
	p := NewConsumerPool(NewRegistry(), &Config{}, nil, nil, nil, nil)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown of an idle pool must return nil, got %v", err)
	}
	// Idempotent across calls (stopOnce).
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown must remain nil, got %v", err)
	}
}

func TestBuildReceiverAppContext_CorrelationBranches(t *testing.T) {
	eventID := uuid.New()

	t.Run("valid-correlation-header", func(t *testing.T) {
		corr := uuid.New()
		ctx := buildReceiverAppContext(map[string]string{
			"actor":          "user-9",
			"correlation_id": corr.String(),
		}, eventID)
		if ctx.CorrelationID() != corr {
			t.Fatalf("correlation = %v, want header value %v", ctx.CorrelationID(), corr)
		}
		if ctx.CausationID() != eventID {
			t.Fatalf("causation must be the event id, got %v", ctx.CausationID())
		}
		if ctx.ActorSubject() != "user-9" {
			t.Fatalf("actor = %q, want user-9", ctx.ActorSubject())
		}
	})

	t.Run("invalid-correlation-falls-back-to-eventid", func(t *testing.T) {
		ctx := buildReceiverAppContext(map[string]string{
			"correlation_id": "not-a-uuid",
		}, eventID)
		if ctx.CorrelationID() != eventID {
			t.Fatalf("invalid correlation must fall back to event id, got %v", ctx.CorrelationID())
		}
		// Missing actor header → anonymous sentinel.
		if ctx.ActorSubject() != "anonymous" {
			t.Fatalf("missing actor must be anonymous, got %q", ctx.ActorSubject())
		}
	})

	t.Run("absent-correlation-uses-eventid", func(t *testing.T) {
		ctx := buildReceiverAppContext(map[string]string{}, eventID)
		if ctx.CorrelationID() != eventID {
			t.Fatalf("absent correlation must use event id, got %v", ctx.CorrelationID())
		}
	})
}
