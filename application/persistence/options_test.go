package persistence

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// typedEntity is the minimal stand-in the Tier 3 tests use as T for the
// generic WriteOption[T] / hook function types. It does not need to
// satisfy domain.Entity — these tests only exercise the option-fold
// helper, not any persister code.
type typedEntity struct {
	Name string
}

// TestResolveWriteOptions_Empty proves the fast path: no options
// supplied → both resolved hooks are nil. Auto handler Cmds that
// implement neither provider land here, and the resolver should not
// allocate.
func TestResolveWriteOptions_Empty(t *testing.T) {
	ab, bc := ResolveWriteOptions[*typedEntity](nil)
	if ab != nil {
		t.Errorf("expected nil afterBegin on empty opts, got %v", ab)
	}
	if bc != nil {
		t.Errorf("expected nil beforeCommit on empty opts, got %v", bc)
	}
}

// TestResolveWriteOptions_BothApply proves the composition case
// covering Topic 5: WithAfterBegin + WithBeforeCommit fold into the
// same writeOptions value, and both surface on the resolved pair.
func TestResolveWriteOptions_BothApply(t *testing.T) {
	wantAB := AfterBeginHook[*typedEntity](func(*configuration.AppContext, *typedEntity, TxHandle) error {
		return nil
	})
	wantBC := BeforeCommitHook[*typedEntity](func(*configuration.AppContext, *typedEntity, domain.ID, TxHandle) error {
		return errors.New("expected")
	})

	ab, bc := ResolveWriteOptions[*typedEntity]([]WriteOption[*typedEntity]{
		WithAfterBegin[*typedEntity](wantAB),
		WithBeforeCommit[*typedEntity](wantBC),
	})

	if ab == nil {
		t.Error("expected afterBegin populated")
	}
	if bc == nil {
		t.Error("expected beforeCommit populated")
	}
	// Functional equality is hard to assert in Go; we exercise the
	// resolved closures to confirm they call through to the originals.
	if got := ab(nil, nil, nil); got != nil {
		t.Errorf("afterBegin should run the supplied closure (nil expected); got %v", got)
	}
	if got := bc(nil, nil, domain.ID{}, nil); got == nil || got.Error() != "expected" {
		t.Errorf("beforeCommit should run the supplied closure; got %v", got)
	}
}

// TestResolveWriteOptions_NilFn proves the silent-drop policy on
// WithAfterBegin(nil) / WithBeforeCommit(nil): the option is accepted
// (so call sites that conditionally build options stay simple) but the
// resolved slot stays nil.
func TestResolveWriteOptions_NilFn(t *testing.T) {
	ab, bc := ResolveWriteOptions[*typedEntity]([]WriteOption[*typedEntity]{
		WithAfterBegin[*typedEntity](nil),
		WithBeforeCommit[*typedEntity](nil),
	})
	if ab != nil || bc != nil {
		t.Errorf("nil fn should drop silently; got ab=%v bc=%v", ab, bc)
	}
}

// TestResolveWriteOptions_LastWins matches the documented contract:
// when two non-nil closures land for the same slot, the second one
// wins. Lets the manual path overlay an Auto-derived option deliberately
// without needing a "reset" knob.
func TestResolveWriteOptions_LastWins(t *testing.T) {
	first := AfterBeginHook[*typedEntity](func(*configuration.AppContext, *typedEntity, TxHandle) error {
		return errors.New("first")
	})
	second := AfterBeginHook[*typedEntity](func(*configuration.AppContext, *typedEntity, TxHandle) error {
		return errors.New("second")
	})

	ab, _ := ResolveWriteOptions[*typedEntity]([]WriteOption[*typedEntity]{
		WithAfterBegin[*typedEntity](first),
		WithAfterBegin[*typedEntity](second),
	})
	if got := ab(nil, nil, nil); got == nil || got.Error() != "second" {
		t.Errorf("last-write-wins: expected second, got %v", got)
	}
}

// TestResolveWriteOptions_NilOptionIsSafe exercises the defensive nil
// guard inside ResolveWriteOptions — a caller that builds the slice
// dynamically may end up with one nil entry; the resolver skips it
// without panicking.
func TestResolveWriteOptions_NilOptionIsSafe(t *testing.T) {
	ab, bc := ResolveWriteOptions[*typedEntity]([]WriteOption[*typedEntity]{nil, nil})
	if ab != nil || bc != nil {
		t.Errorf("nil option entries should drop silently; got ab=%v bc=%v", ab, bc)
	}
}
