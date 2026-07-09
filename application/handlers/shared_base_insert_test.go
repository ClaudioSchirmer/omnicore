package handlers

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
)

// testSBInsertCmd is a SharedBaseInsertCommand: ApplyTo (the single mutator) +
// FromEntity. applyCalls records how many times the framework invoked ApplyTo
// (once cold, twice warm — once to read the natural key, once to merge).
type testSBInsertCmd struct {
	pipeline.CommandBase
	Name       string
	applyCalls int
}

func (c *testSBInsertCmd) ApplyTo(_ *configuration.AppContext, e *testEntity) error {
	c.applyCalls++
	e.Name = c.Name
	return nil
}
func (c *testSBInsertCmd) FromEntity(_ *configuration.AppContext, e *testEntity) (*testEntity, error) {
	return e, nil
}

// mockSharedBaseRepo is a mockRepo that ALSO implements
// persistence.SharedBaseInsertLoader[*testEntity] — i.e. a SharedBase-backed repo.
type mockSharedBaseRepo struct {
	*mockRepo
	existed bool
	loaded  *testEntity
}

func (r *mockSharedBaseRepo) LoadForSharedBaseInsert(_ *configuration.AppContext, fresh *testEntity) (*testEntity, bool, error) {
	if r.existed {
		return r.loaded, true, nil
	}
	return fresh, false, nil
}

// Direction 1: the plain InsertCommandHandler refuses a SharedBase-backed repo.
func TestInsertCommandHandler_RefusesSharedBaseRepo(t *testing.T) {
	repo := &mockSharedBaseRepo{mockRepo: newMockRepo()}
	h := &InsertCommandHandler[*testEntity, *testInsertCmd, *testEntity]{Repo: repo}
	if _, err := h.Handle(testCtx(), &testInsertCmd{Name: "x"}); err == nil {
		t.Fatal("InsertCommandHandler must refuse a SharedBase-backed repo (direction 1)")
	}
	if repo.insertCalled != 0 {
		t.Errorf("must not insert when refused, got %d", repo.insertCalled)
	}
}

// Direction 2: the SharedBaseInsertCommandHandler refuses a non-SharedBase repo.
func TestSharedBaseInsertHandler_RefusesPlainRepo(t *testing.T) {
	repo := newMockRepo()
	h := &SharedBaseInsertCommandHandler[*testEntity, *testSBInsertCmd, *testEntity]{Repo: repo}
	if _, err := h.Handle(testCtx(), &testSBInsertCmd{Name: "x"}); err == nil {
		t.Fatal("SharedBaseInsertCommandHandler must refuse a non-SharedBase repo (direction 2)")
	}
}

// Warm: the identity exists → load it, ApplyTo runs twice (nk + merge), Insert.
func TestSharedBaseInsertHandler_Warm(t *testing.T) {
	repo := &mockSharedBaseRepo{mockRepo: newMockRepo(), existed: true, loaded: &testEntity{Name: "loaded"}}
	cmd := &testSBInsertCmd{Name: "alice"}
	h := &SharedBaseInsertCommandHandler[*testEntity, *testSBInsertCmd, *testEntity]{Repo: repo}
	got, err := h.Handle(testCtx(), cmd)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if repo.insertCalled != 1 {
		t.Errorf("Insert once, got %d", repo.insertCalled)
	}
	if cmd.applyCalls != 2 {
		t.Errorf("warm: ApplyTo twice (read key + merge onto loaded), got %d", cmd.applyCalls)
	}
	if got == nil || got.Name != "alice" {
		t.Errorf("FromEntity must project the merged loaded entity, got %+v", got)
	}
}

// Cold: no identity → ApplyTo once (onto fresh), Insert.
func TestSharedBaseInsertHandler_Cold(t *testing.T) {
	repo := &mockSharedBaseRepo{mockRepo: newMockRepo(), existed: false}
	cmd := &testSBInsertCmd{Name: "bob"}
	h := &SharedBaseInsertCommandHandler[*testEntity, *testSBInsertCmd, *testEntity]{Repo: repo}
	if _, err := h.Handle(testCtx(), cmd); err != nil {
		t.Fatalf("cold: %v", err)
	}
	if repo.insertCalled != 1 {
		t.Errorf("Insert once, got %d", repo.insertCalled)
	}
	if cmd.applyCalls != 1 {
		t.Errorf("cold: ApplyTo once, got %d", cmd.applyCalls)
	}
}
