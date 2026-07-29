package core

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// The engine registry — the database/sql-style seam that lets each engine live
// behind a build tag without bootstrap importing it.

func TestEngineRegistry_RegisterLookupAndUnknown(t *testing.T) {
	called := false
	RegisterEngine("fake-dialect-for-test", func(_ context.Context, _ EngineConfig) (RelationalEngine, error) {
		called = true
		return nil, nil
	})
	t.Cleanup(func() { delete(engineFactories, "fake-dialect-for-test") })

	if _, err := NewEngine("fake-dialect-for-test", context.Background(), EngineConfig{}); err != nil {
		t.Fatalf("registered dialect must resolve: %v", err)
	}
	if !called {
		t.Fatal("the registered factory must be invoked")
	}

	// An unknown dialect is the "no build tag" mistake — the error must say so.
	if _, err := NewEngine("no-such-dialect", context.Background(), EngineConfig{}); err == nil ||
		!strings.Contains(err.Error(), "build tag") {
		t.Fatalf("unknown dialect must produce the actionable build-tag error, got %v", err)
	}
}

func TestEngineRegistry_DuplicatePanics(t *testing.T) {
	RegisterEngine("dup-dialect-for-test", func(context.Context, EngineConfig) (RelationalEngine, error) { return nil, nil })
	t.Cleanup(func() { delete(engineFactories, "dup-dialect-for-test") })
	defer func() {
		if recover() == nil {
			t.Fatal("two engines claiming one dialect is a build bug — it must panic")
		}
	}()
	RegisterEngine("dup-dialect-for-test", func(context.Context, EngineConfig) (RelationalEngine, error) { return nil, nil })
}

// SafeIdentifier is the SQL-injection allowlist every engine quoter leans on.
func TestSafeIdentifier(t *testing.T) {
	for _, good := range []string{"users", "omnicore_projection_failures", "A1_b2"} {
		if !SafeIdentifier(good) {
			t.Errorf("%q must be safe", good)
		}
	}
	for _, bad := range []string{"", "users; DROP", "na-me", "café", `qu"oted`} {
		if SafeIdentifier(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// TypeName strips the pointer — notification/translation key resolution
// depends on *T and T yielding the same name (gotcha #4).
func TestTypeName_StripsPointer(t *testing.T) {
	type sample struct{}
	if got := TypeName[sample](); got != "sample" {
		t.Errorf("TypeName[T] = %q", got)
	}
	if got := TypeName[*sample](); got != "sample" {
		t.Errorf("TypeName[*T] = %q", got)
	}
	if got := TypeName[any](); got != "" {
		t.Errorf("an unnamed interface must yield the empty fallback, got %q", got)
	}
}

// UnwrapTx panics loudly on a foreign TxHandle — a nil exec that NPEs deep in
// a SQL call would be far worse than failing at the unwrap site.
func TestUnwrapTx_ForeignHandlePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a TxHandle without a neutral Tx must panic at the unwrap site")
		}
	}()
	UnwrapTx(foreignTxHandle{})
}

// foreignTxHandle is sealed (embeds the token) but carries no neutral Tx — the
// exact shape the unwrap panic guards against.
type foreignTxHandle struct{ persistence.SealedTxHandle }
