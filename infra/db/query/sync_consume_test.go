package query

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// The consumer session and its supervisor, driven through the transport port
// with a scriptable subscriber — the read→dispatch→outcome plumbing that the
// processToOutcome tests take as given.

// scriptedMessage pairs one message with the completion the subscription hands
// out for it, so a test can assert the outcome the engine reported.
type scriptedMessage struct {
	msg        transport.Message
	completion *recordingCompletion
}

// scriptSubscriber delivers each session's scripted messages in order and then
// blocks until the context ends. sessionErrs[i] short-circuits session i's
// FIRST Read with an error, which is how a test provokes a session teardown.
type scriptSubscriber struct {
	sessions    [][]scriptedMessage
	sessionErrs []error
	session     int
}

func (s *scriptSubscriber) EnsureTopics(context.Context, []transport.TopicSpec) error { return nil }

func (s *scriptSubscriber) Subscribe(context.Context, transport.SubscribeConfig) (transport.Subscription, error) {
	i := s.session
	s.session++
	var msgs []scriptedMessage
	if i < len(s.sessions) {
		msgs = s.sessions[i]
	}
	var err error
	if i < len(s.sessionErrs) {
		err = s.sessionErrs[i]
	}
	return &scriptSubscription{msgs: msgs, readErr: err}, nil
}

type scriptSubscription struct {
	msgs    []scriptedMessage
	readErr error
	pos     int
}

func (s *scriptSubscription) Read(ctx context.Context) (transport.Message, transport.Completion, error) {
	if s.readErr != nil {
		err := s.readErr
		s.readErr = nil
		return transport.Message{}, nil, err
	}
	if s.pos < len(s.msgs) {
		m := s.msgs[s.pos]
		s.pos++
		return m.msg, m.completion, nil
	}
	<-ctx.Done()
	return transport.Message{}, nil, ctx.Err()
}

func (s *scriptSubscription) Close() error { return nil }

func convRoleMessage() scriptedMessage {
	return scriptedMessage{
		msg: transport.Message{
			Topic: "outbox.event.aluno",
			Key:   []byte("a1"),
			Value: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":2,"base_id":"p1","base_revision":3}}`),
			Headers: map[string]string{
				"aggregate_type": "aluno",
				"event_type":     "UPDATED",
			},
		},
		completion: &recordingCompletion{},
	}
}

// syncedStore wraps the fakeStore with a mutex-guarded write counter: the
// consume tests poll from the test goroutine while a WORKER writes, and the
// plain fakeColl recorders are not concurrency-safe.
type syncedStore struct {
	*fakeStore
	mu     sync.Mutex
	writes int
}

func newSyncedStore() *syncedStore {
	return &syncedStore{fakeStore: newFakeMongo(&fakeColl{})}
}

func (s *syncedStore) ApplyProjection(_ context.Context, _ PhysicalCollection, _ string, _ []Document, _ bool) error {
	// Count only — no delegation: the embedded fakeColl recorders are not
	// mutex-guarded, and this method is called from a worker goroutine.
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return nil
}

func (s *syncedStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// waitFor polls cond up to 5s — the tests assert convergence, never timing.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestConsume_DeliversAndConfirms: one valid message read from the port lands
// as a document and is confirmed by the WORKER that processed it.
func TestConsume_DeliversAndConfirms(t *testing.T) {
	store := newSyncedStore()
	view := convRoleView()
	m := convRoleMessage()
	sub := &scriptSubscriber{sessions: [][]scriptedMessage{{m}}}
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, sub, "g", []*ViewDefinition{view}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.consume(ctx) }()

	waitFor(t, "the document write", func() bool { return store.writeCount() > 0 })
	waitFor(t, "the Done confirmation", func() bool { d, _ := m.completion.counts(); return d == 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a context-driven exit must be clean, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consume did not exit on context cancellation")
	}
	if _, f := m.completion.counts(); f != 0 {
		t.Errorf("a delivered message must not also be handed back, failed=%d", f)
	}
}

// TestConsume_IncompleteMetadataAdvances: a message the producer never labeled
// is terminal by nature — no retry adds metadata. It must be completed (so the
// stream advances) and never projected.
func TestConsume_IncompleteMetadataAdvances(t *testing.T) {
	store := newSyncedStore()
	view := convRoleView()
	bare := scriptedMessage{
		msg:        transport.Message{Topic: "outbox.event.aluno", Key: []byte("a1"), Value: []byte(`{}`)},
		completion: &recordingCompletion{},
	}
	sub := &scriptSubscriber{sessions: [][]scriptedMessage{{bare}}}
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, sub, "g", []*ViewDefinition{view}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.consume(ctx) }()

	waitFor(t, "the Done on the unlabeled message", func() bool { d, _ := bare.completion.counts(); return d == 1 })
	cancel()
	<-done
	if store.writeCount() != 0 {
		t.Errorf("an unlabeled message must not be projected, got %d writes", store.writeCount())
	}
}

// TestConfigureParkedRetry_CadenceAndOff: the mongo.parkedRetry knob. With a
// custom cadence the replay driver stamps its liveness clock; turned off, the
// loop never starts and the clock stays zero — which is exactly the observable
// an operator alarms on when the driver is EXPECTED to run.
func TestConfigureParkedRetry_CadenceAndOff(t *testing.T) {
	build := func() *SyncEngine {
		identityResolver.lease = 0
		return NewSyncEngine(newFakeEngine(&fakeQuerier{}), newFakeMongo(&fakeColl{}), identityResolver,
			&scriptSubscriber{}, "g", []*ViewDefinition{convRoleView()}, 1)
	}

	on := build()
	on.ConfigureParkedRetry(true, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	on.Start(ctx)
	waitFor(t, "the first ledger sweep", func() bool { return !on.ProjectionHealth().LastLedgerSweep.IsZero() })

	off := build()
	off.ConfigureParkedRetry(false, 10*time.Millisecond)
	octx, ocancel := context.WithCancel(context.Background())
	defer ocancel()
	off.Start(octx)
	time.Sleep(100 * time.Millisecond)
	if !off.ProjectionHealth().LastLedgerSweep.IsZero() {
		t.Fatal("a disabled replay driver must never sweep — enabled: false was ignored")
	}
}

// TestRun_SupervisorRestartsAfterSessionError is §4.1's regression test at the
// loop level: a session torn down by a reader error must be REPLACED, not
// terminal — the next session delivers, and the restart is counted.
func TestRun_SupervisorRestartsAfterSessionError(t *testing.T) {
	store := newSyncedStore()
	view := convRoleView()
	m := convRoleMessage()
	sub := &scriptSubscriber{
		sessions:    [][]scriptedMessage{nil, {m}},
		sessionErrs: []error{errors.New("fake transport: session torn down")},
	}
	s := NewSyncEngine(composerEngine(convComposerRows), store, identityResolver, sub, "g", []*ViewDefinition{view}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()

	waitFor(t, "the second session's delivery", func() bool { d, _ := m.completion.counts(); return d == 1 })
	if got := s.ProjectionHealth().Counters[MetricProjectionSessionRestart]; got != 1 {
		t.Errorf("session restarts = %d, want 1", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit on context cancellation")
	}
}
