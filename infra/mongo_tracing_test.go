package infra

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/event"
)

func succeeded(id int64, name string) *event.CommandSucceededEvent {
	return &event.CommandSucceededEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: id, CommandName: name}}
}

func failed(id int64, name string, err error) *event.CommandFailedEvent {
	return &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: id, CommandName: name}, Failure: err}
}

// The monitor's span start/end are no-ops without an installed provider, but the
// RequestID match-and-evict bookkeeping is real and unit-testable.
func TestMongoCommandMonitorLifecycle(t *testing.T) {
	m := newMongoCommandMonitor()
	ctx := context.Background()

	// Started → Succeeded evicts cleanly.
	m.Started(ctx, &event.CommandStartedEvent{CommandName: "find", DatabaseName: "db", RequestID: 1})
	m.Succeeded(ctx, succeeded(1, "find"))

	// Started → Failed records the error and evicts.
	m.Started(ctx, &event.CommandStartedEvent{CommandName: "insert", DatabaseName: "db", RequestID: 2})
	m.Failed(ctx, failed(2, "insert", errors.New("boom")))

	// Unknown RequestID on completion is a safe no-op (never started / double fire).
	m.Succeeded(ctx, succeeded(999, "x"))
	m.Failed(ctx, failed(998, "x", errors.New("x")))
}

func TestTakeSpanMissing(t *testing.T) {
	m := newMongoCommandMonitor()
	m.Started(context.Background(), &event.CommandStartedEvent{CommandName: "x", RequestID: 7})
	m.Succeeded(context.Background(), succeeded(7, "x"))
	// A second completion for the same id must not panic.
	m.Succeeded(context.Background(), succeeded(7, "x"))
}
