package persistence

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// readEntity is the minimal T the scoped-reader tests load. It does not need to
// satisfy domain.Entity — LoadForWrite / LoadArchivedForWrite are generic on
// `any` and only thread the value through.
type readEntity struct{ tag string }

// plainRepo satisfies ScopedRepository[*readEntity] with the ctx-less read port
// only — no ScopedReader/ScopedArchivedReader. It is the escape-hatch shape the
// helpers must degrade to.
type plainRepo struct {
	findByIDCalled bool
	ret            *readEntity
	err            error
}

func (r *plainRepo) FindByID(domain.ID) (*readEntity, error) {
	r.findByIDCalled = true
	return r.ret, r.err
}
func (r *plainRepo) New() *readEntity { return &readEntity{tag: "new"} }
func (r *plainRepo) Scope(*configuration.AppContext, ...WriteOption[*readEntity]) domain.Writer {
	return nil // not exercised by these tests
}

// scopedReaderRepo adds the ctx-bound read capability.
type scopedReaderRepo struct {
	plainRepo
	scopedCtx *configuration.AppContext
	scopedRet *readEntity
	scopedErr error
}

func (r *scopedReaderRepo) ScopedReader(ctx *configuration.AppContext) domain.Reader[*readEntity] {
	r.scopedCtx = ctx
	return boundFake{ret: r.scopedRet, err: r.scopedErr}
}

// archivedScopedRepo adds the ctx-bound archived read capability.
type archivedScopedRepo struct {
	plainRepo
	scopedCtx *configuration.AppContext
	ret       *readEntity
	err       error
}

func (r *archivedScopedRepo) ScopedArchivedReader(ctx *configuration.AppContext) domain.ArchivedFinder[*readEntity] {
	r.scopedCtx = ctx
	return boundFake{ret: r.ret, err: r.err}
}

// archivedFinderRepo has the ctx-less archived finder but no scoped variant.
type archivedFinderRepo struct {
	plainRepo
	finderCalled bool
	ret          *readEntity
	err          error
}

func (r *archivedFinderRepo) FindArchivedByID(domain.ID) (*readEntity, error) {
	r.finderCalled = true
	return r.ret, r.err
}

// boundFake stands in for the request-scoped read handle a real ScopedReader /
// ScopedArchivedReader returns (infra.boundReader).
type boundFake struct {
	ret *readEntity
	err error
}

func (b boundFake) FindByID(domain.ID) (*readEntity, error)         { return b.ret, b.err }
func (b boundFake) FindArchivedByID(domain.ID) (*readEntity, error) { return b.ret, b.err }
func (b boundFake) New() *readEntity                                { return &readEntity{tag: "new"} }

func ctx() *configuration.AppContext {
	return configuration.NewAppContextWithRandomID(configuration.LangPTBR)
}

// TestLoadForWrite_UsesScopedReader proves the canonical path: a repo providing
// ScopedReader loads through it (so the request ctx reaches the SELECT) and the
// ctx-less FindByID is NOT touched.
func TestLoadForWrite_UsesScopedReader(t *testing.T) {
	want := &readEntity{tag: "scoped"}
	repo := &scopedReaderRepo{scopedRet: want}
	c := ctx()

	got, err := LoadForWrite[*readEntity](repo, c, domain.NewRandomID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("expected scoped result, got %+v", got)
	}
	if repo.findByIDCalled {
		t.Error("ctx-less FindByID must not be called when ScopedReader is provided")
	}
	if repo.scopedCtx != c {
		t.Error("request ctx was not threaded into ScopedReader")
	}
}

// TestLoadForWrite_FallsBackToFindByID proves the escape-hatch degradation: a
// repo without ScopedReader loads through the ctx-less domain.Reader.FindByID.
func TestLoadForWrite_FallsBackToFindByID(t *testing.T) {
	want := &readEntity{tag: "plain"}
	repo := &plainRepo{ret: want}

	got, err := LoadForWrite[*readEntity](repo, ctx(), domain.NewRandomID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.findByIDCalled {
		t.Error("expected fallback to ctx-less FindByID")
	}
	if got != want {
		t.Errorf("expected plain result, got %+v", got)
	}
}

// TestLoadForWrite_PropagatesError confirms a load error surfaces unchanged on
// the ctx-bound path.
func TestLoadForWrite_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	repo := &scopedReaderRepo{scopedErr: sentinel}

	if _, err := LoadForWrite[*readEntity](repo, ctx(), domain.NewRandomID()); !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// TestLoadArchivedForWrite_UsesScopedArchivedReader proves the canonical
// unarchive hydration path is ctx-bound when provided.
func TestLoadArchivedForWrite_UsesScopedArchivedReader(t *testing.T) {
	want := &readEntity{tag: "archived-scoped"}
	repo := &archivedScopedRepo{ret: want}
	c := ctx()

	got, found, err := LoadArchivedForWrite[*readEntity](repo, c, domain.NewRandomID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true on the scoped archived path")
	}
	if got != want {
		t.Errorf("expected archived-scoped result, got %+v", got)
	}
	if repo.scopedCtx != c {
		t.Error("request ctx was not threaded into ScopedArchivedReader")
	}
}

// TestLoadArchivedForWrite_FallsBackToArchivedFinder proves a repo with only the
// ctx-less ArchivedFinder still hydrates (found=true) via that path.
func TestLoadArchivedForWrite_FallsBackToArchivedFinder(t *testing.T) {
	want := &readEntity{tag: "archived-plain"}
	repo := &archivedFinderRepo{ret: want}

	got, found, err := LoadArchivedForWrite[*readEntity](repo, ctx(), domain.NewRandomID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found || !repo.finderCalled {
		t.Errorf("expected ctx-less ArchivedFinder fallback (found=%v, called=%v)", found, repo.finderCalled)
	}
	if got != want {
		t.Errorf("expected archived-plain result, got %+v", got)
	}
}

// TestLoadArchivedForWrite_NeitherReportsNotFound proves a repo providing no
// archived-finder capability returns found=false (caller then uses Repo.New()).
func TestLoadArchivedForWrite_NeitherReportsNotFound(t *testing.T) {
	got, found, err := LoadArchivedForWrite[*readEntity](&plainRepo{}, ctx(), domain.NewRandomID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected found=false when no archived finder is provided")
	}
	if got != nil {
		t.Errorf("expected zero value when not found, got %+v", got)
	}
}

// TestLoadArchivedForWrite_PropagatesError confirms an archived-load error
// surfaces unchanged with found=true (so the handler returns it, not New()).
func TestLoadArchivedForWrite_PropagatesError(t *testing.T) {
	sentinel := errors.New("kaboom")
	repo := &archivedScopedRepo{err: sentinel}

	_, found, err := LoadArchivedForWrite[*readEntity](repo, ctx(), domain.NewRandomID())
	if !found {
		t.Error("expected found=true even on error so the caller does not fall back to New()")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}
