package openapi

import (
	"testing"
)

func TestRegistry_AddPreservesInsertionOrder(t *testing.T) {
	r := NewRegistry()
	r.add(Operation{Method: "GET", Path: "/a"})
	r.add(Operation{Method: "POST", Path: "/a"})
	r.add(Operation{Method: "GET", Path: "/b"})

	ops := r.Operations()
	if len(ops) != 3 {
		t.Fatalf("got %d ops, want 3", len(ops))
	}
	want := []struct{ m, p string }{
		{"GET", "/a"},
		{"POST", "/a"},
		{"GET", "/b"},
	}
	for i, w := range want {
		if ops[i].Method != w.m || ops[i].Path != w.p {
			t.Fatalf("op[%d]: got %s %s, want %s %s", i, ops[i].Method, ops[i].Path, w.m, w.p)
		}
	}
}

func TestRegistry_AddSameKeyOverwritesNotDuplicates(t *testing.T) {
	r := NewRegistry()
	r.add(Operation{Method: "GET", Path: "/a", Doc: Doc{Summary: "first"}})
	r.add(Operation{Method: "GET", Path: "/a", Doc: Doc{Summary: "second"}})

	ops := r.Operations()
	if len(ops) != 1 {
		t.Fatalf("duplicate (METHOD, Path) should collapse; got %d ops", len(ops))
	}
	if ops[0].Doc.Summary != "second" {
		t.Fatalf("last write should win; got summary %q", ops[0].Doc.Summary)
	}
}

func TestRegistry_AddNormalizesMethodCase(t *testing.T) {
	r := NewRegistry()
	r.add(Operation{Method: "get", Path: "/a"})
	r.add(Operation{Method: "GET", Path: "/a"})

	if len(r.Operations()) != 1 {
		t.Fatalf("method case differences should collapse via uppercasing the key")
	}
}

func TestRegistry_ComponentsExposed(t *testing.T) {
	r := NewRegistry()
	if r.Components() == nil {
		t.Fatal("Components should be initialized by NewRegistry")
	}
	if r.Components().Schemas == nil {
		t.Fatal("Components.Schemas should be a usable map")
	}
}

func TestRegistry_OperationsReturnsSnapshot(t *testing.T) {
	r := NewRegistry()
	r.add(Operation{Method: "GET", Path: "/a"})
	snapshot := r.Operations()
	// Mutating the returned slice must not affect the registry.
	snapshot[0].Path = "/mutated"
	if r.Operations()[0].Path != "/a" {
		t.Fatal("Operations() must return a snapshot, not the live slice")
	}
}
