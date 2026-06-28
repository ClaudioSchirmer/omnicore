package relational

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Compile-time: BaseAggregateRepository[T] provides the ctx-bound read
// capabilities the write-command handlers probe, so the canonical aggregate
// path is covered automatically (no consumer code).
var (
	_ persistence.ScopedReaderProvider[*barTestEntity]         = (*barUserRepo)(nil)
	_ persistence.ScopedArchivedReaderProvider[*barTestEntity] = (*barUserRepo)(nil)
)

// TestScopedReader_WiresLoaderAndCtx proves ScopedReader returns a usable
// request-scoped reader closing over the Loader and the request ctx (the DB-
// hitting FindByID is exercised by the integration suite; here we assert the
// wiring and the promoted New() factory).
func TestScopedReader_WiresLoaderAndCtx(t *testing.T) {
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)
	c := configuration.NewAppContextWithRandomID(configuration.LangPTBR)

	reader := bar.ScopedReader(c)
	if reader == nil {
		t.Fatal("ScopedReader must return a non-nil domain.Reader[T]")
	}
	br, ok := reader.(boundReader[*barTestEntity])
	if !ok {
		t.Fatalf("expected boundReader, got %T", reader)
	}
	if br.loader != bar.Loader {
		t.Error("boundReader must close over the repository Loader")
	}
	if br.ctx != c {
		t.Error("boundReader must close over the request ctx")
	}
	if got := reader.New(); got == nil {
		t.Error("promoted New() must build an entity via the factory")
	}
}

// TestScopedArchivedReader_Wires proves the OnlyArchived twin returns a
// domain.ArchivedFinder[T] over the same Loader + ctx.
func TestScopedArchivedReader_Wires(t *testing.T) {
	bar := NewBaseAggregateRepository[*barTestEntity](nil, newBARTestEntity)
	c := configuration.NewAppContextWithRandomID(configuration.LangPTBR)

	finder := bar.ScopedArchivedReader(c)
	br, ok := finder.(boundReader[*barTestEntity])
	if !ok {
		t.Fatalf("expected boundReader, got %T", finder)
	}
	if br.loader != bar.Loader || br.ctx != c {
		t.Error("ScopedArchivedReader must close over the Loader + request ctx")
	}
}

// TestBoundReader_NewPanicsWithoutFactory mirrors BaseRepository.New: a missing
// factory is a config-time bug caught loudly.
func TestBoundReader_NewPanicsWithoutFactory(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when newEntity factory is nil")
		}
	}()
	br := boundReader[*barTestEntity]{}
	_ = br.New()
}

// Ensure domain stays the import anchor (boundReader returns domain ports).
var _ domain.Reader[*barTestEntity] = boundReader[*barTestEntity]{}
