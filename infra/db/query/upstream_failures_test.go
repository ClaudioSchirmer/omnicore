package query

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePgExec captures the last Exec invocation so the helpers' SQL + arg
// shape can be asserted without a real database. It satisfies the neutral
// infra.Querier; Query / QueryRow / QueryMaps are not exercised by these
// unit tests (List shape is covered by the integration suite).
type fakePgExec struct {
	lastSQL  string
	lastArgs []any
	execErr  error
}

func (f *fakePgExec) Exec(_ context.Context, sql string, args ...any) error {
	f.lastSQL = sql
	f.lastArgs = args
	return f.execErr
}

func (f *fakePgExec) Query(_ context.Context, _ string, _ ...any) (core.Rows, error) {
	return nil, errors.New("not implemented in fakePgExec")
}

func (f *fakePgExec) QueryRow(_ context.Context, _ string, _ ...any) core.Row {
	return nil
}

func (f *fakePgExec) QueryMaps(_ context.Context, _ string, _ ...any) ([]map[string]any, error) {
	return nil, errors.New("not implemented in fakePgExec")
}

func TestRecordUpstreamFailure_RejectsEmptyTopic(t *testing.T) {
	err := RecordUpstreamFailure(context.Background(), &fakePgExec{}, fakeDialect{}, UpstreamFailureRecord{
		ViewName:   "orders",
		UpstreamID: "u1",
		Stage:      UpstreamFailureStageCompose,
	})
	if err == nil || !strings.Contains(err.Error(), "subscription_topic") {
		t.Errorf("expected subscription_topic-required error, got %v", err)
	}
}

func TestRecordUpstreamFailure_RejectsEmptyView(t *testing.T) {
	err := RecordUpstreamFailure(context.Background(), &fakePgExec{}, fakeDialect{}, UpstreamFailureRecord{
		SubscriptionTopic: "users.events",
		UpstreamID:        "u1",
		Stage:             UpstreamFailureStageCompose,
	})
	if err == nil || !strings.Contains(err.Error(), "view_name") {
		t.Errorf("expected view_name-required error, got %v", err)
	}
}

func TestRecordUpstreamFailure_RejectsEmptyUpstreamID(t *testing.T) {
	err := RecordUpstreamFailure(context.Background(), &fakePgExec{}, fakeDialect{}, UpstreamFailureRecord{
		SubscriptionTopic: "users.events",
		ViewName:          "orders",
		Stage:             UpstreamFailureStageCompose,
	})
	if err == nil || !strings.Contains(err.Error(), "upstream_id") {
		t.Errorf("expected upstream_id-required error, got %v", err)
	}
}

func TestRecordUpstreamFailure_RejectsInvalidStage(t *testing.T) {
	err := RecordUpstreamFailure(context.Background(), &fakePgExec{}, fakeDialect{}, UpstreamFailureRecord{
		SubscriptionTopic: "users.events",
		ViewName:          "orders",
		UpstreamID:        "u1",
		Stage:             UpstreamFailureStage("garbage"),
	})
	if err == nil || !strings.Contains(err.Error(), "stage") {
		t.Errorf("expected stage-invalid error, got %v", err)
	}
}

func TestRecordUpstreamFailure_PassesArgsInOrder(t *testing.T) {
	fake := &fakePgExec{}
	err := RecordUpstreamFailure(context.Background(), fake, fakeDialect{}, UpstreamFailureRecord{
		SubscriptionTopic: "users.events",
		ViewName:          "orders",
		UpstreamID:        "u1",
		LocalID:           "ord-7",
		Stage:             UpstreamFailureStageCompose,
		Error:             "boom",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(fake.lastSQL, "INSERT INTO omnicore_upstream_failures") ||
		!strings.Contains(fake.lastSQL, "ON CONFLICT") {
		t.Errorf("SQL shape wrong: %s", fake.lastSQL)
	}
	want := []any{"users.events", "orders", "u1", "ord-7", "compose", "boom"}
	if len(fake.lastArgs) != len(want) {
		t.Fatalf("args len = %d, want %d (%v)", len(fake.lastArgs), len(want), fake.lastArgs)
	}
	for i, v := range want {
		if fake.lastArgs[i] != v {
			t.Errorf("arg[%d] = %v, want %v", i, fake.lastArgs[i], v)
		}
	}
}

func TestRecordUpstreamFailure_AcceptsEmptyLocalID(t *testing.T) {
	// Discover-stage failures legitimately carry LocalID == "" because the
	// FindIDsByField call failed before any local id was known. Schema
	// declares local_id NOT NULL DEFAULT '' to keep the natural key intact.
	fake := &fakePgExec{}
	err := RecordUpstreamFailure(context.Background(), fake, fakeDialect{}, UpstreamFailureRecord{
		SubscriptionTopic: "users.events",
		ViewName:          "orders",
		UpstreamID:        "u1",
		LocalID:           "",
		Stage:             UpstreamFailureStageDiscover,
		Error:             "find failed",
	})
	if err != nil {
		t.Fatalf("empty LocalID on discover stage must be accepted: %v", err)
	}
	if fake.lastArgs[3] != "" {
		t.Errorf("local_id arg should be empty string, got %v", fake.lastArgs[3])
	}
}

func TestRecordUpstreamFailure_PropagatesExecError(t *testing.T) {
	fake := &fakePgExec{execErr: errors.New("connection lost")}
	err := RecordUpstreamFailure(context.Background(), fake, fakeDialect{}, UpstreamFailureRecord{
		SubscriptionTopic: "users.events",
		ViewName:          "orders",
		UpstreamID:        "u1",
		Stage:             UpstreamFailureStageUpsert,
	})
	if err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Errorf("expected wrapped exec error, got %v", err)
	}
}

func TestResolveUpstreamFailures_RejectsEmptyInputs(t *testing.T) {
	cases := []struct {
		name              string
		subscriptionTopic string
		viewName          string
		upstreamID        string
	}{
		{"empty topic", "", "orders", "u1"},
		{"empty view", "users.events", "", "u1"},
		{"empty upstream id", "users.events", "orders", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ResolveUpstreamFailures(context.Background(), &fakePgExec{}, fakeDialect{},
				tc.subscriptionTopic, tc.viewName, tc.upstreamID)
			if err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestResolveUpstreamFailures_PassesArgsToExec(t *testing.T) {
	fake := &fakePgExec{}
	err := ResolveUpstreamFailures(context.Background(), fake, fakeDialect{}, "users.events", "orders", "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(fake.lastSQL, "UPDATE omnicore_upstream_failures") ||
		!strings.Contains(fake.lastSQL, "resolved_at IS NULL") {
		t.Errorf("SQL shape wrong: %s", fake.lastSQL)
	}
	want := []any{"users.events", "orders", "u1"}
	for i, v := range want {
		if fake.lastArgs[i] != v {
			t.Errorf("arg[%d] = %v, want %v", i, fake.lastArgs[i], v)
		}
	}
}

func TestResolveUpstreamFailures_PropagatesExecError(t *testing.T) {
	fake := &fakePgExec{execErr: errors.New("boom")}
	err := ResolveUpstreamFailures(context.Background(), fake, fakeDialect{}, "users.events", "orders", "u1")
	if err == nil {
		t.Fatal("expected the Exec error to surface")
	}
	if !strings.Contains(err.Error(), "resolve upstream failures") {
		t.Errorf("error not wrapped: %v", err)
	}
}

func TestListPendingUpstreamFailuresByTopic_RejectsEmpty(t *testing.T) {
	_, err := ListPendingUpstreamFailuresByTopic(context.Background(), &fakePgExec{}, fakeDialect{}, "")
	if err == nil || !strings.Contains(err.Error(), "subscription_topic") {
		t.Errorf("expected subscription_topic-required error, got %v", err)
	}
}

func TestUpstreamSubscriber_RetryPendingFailures_NilPGFailsFast(t *testing.T) {
	s := &UpstreamSubscriber{
		cfg:    UpstreamSubscriberConfig{Topic: "users.events"},
		logger: discardLogger(),
	}
	n, err := s.RetryPendingFailures(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no relational engine handle") {
		t.Errorf("expected no-engine-handle error, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 retried, got %d", n)
	}
}

func TestUpstreamSubscriber_RecordFailure_NilSafeWithoutPG(t *testing.T) {
	// s.pg == nil is the legitimate shape for unit tests that drive the
	// subscriber's helpers without a real Postgres handle. recordFailure /
	// resolveFailures MUST short-circuit silently — never panic.
	s := &UpstreamSubscriber{
		cfg:    UpstreamSubscriberConfig{Topic: "users.events"},
		logger: discardLogger(),
	}
	s.recordFailure(context.Background(), "orders", "u1", "ord-7",
		UpstreamFailureStageCompose, errors.New("boom"))
	s.resolveFailures(context.Background(), "orders", "u1")
}
