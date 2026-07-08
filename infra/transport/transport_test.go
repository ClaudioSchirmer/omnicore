package transport

import (
	"context"
	"testing"
)

type stubSubscriber struct{}

func (stubSubscriber) Subscribe(context.Context, SubscribeConfig) (Subscription, error) {
	return nil, nil
}
func (stubSubscriber) EnsureTopics(context.Context, []TopicSpec) error { return nil }

func TestRegisterAndNewSubscriber(t *testing.T) {
	// Use a unique name so this test never collides with an adapter that
	// registered "kafka"/"nats" in init() under the active build tag.
	const name = "stub-under-test"
	RegisterSubscriber(name, func(Config) (Subscriber, error) { return stubSubscriber{}, nil })

	got, err := NewSubscriber(name, Config{Brokers: []string{"b1"}})
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	if _, ok := got.(stubSubscriber); !ok {
		t.Fatalf("NewSubscriber returned %T, want stubSubscriber", got)
	}
}

func TestRegisterSubscriber_DuplicatePanics(t *testing.T) {
	const name = "dup-under-test"
	RegisterSubscriber(name, func(Config) (Subscriber, error) { return stubSubscriber{}, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("registering the same transport twice must panic")
		}
	}()
	RegisterSubscriber(name, func(Config) (Subscriber, error) { return stubSubscriber{}, nil })
}

func TestNewSubscriber_Unknown(t *testing.T) {
	if _, err := NewSubscriber("no-such-transport", Config{}); err == nil {
		t.Fatal("NewSubscriber must error for an unregistered transport")
	}
}
