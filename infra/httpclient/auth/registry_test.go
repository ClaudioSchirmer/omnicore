package auth

import "testing"

func TestRegistry_HasRemoveClone(t *testing.T) {
	r := NewRegistry()
	r.Register("a", NewNoneProvider("a"))
	r.Register("b", NewNoneProvider("b"))

	if !r.Has("a") || r.Has("missing") {
		t.Fatalf("Has: a=%v missing=%v", r.Has("a"), r.Has("missing"))
	}

	// Clone shares instances but is an independent map: mutating the clone
	// must not affect the source.
	clone := r.Clone()
	if !clone.Has("a") || !clone.Has("b") {
		t.Fatal("clone missing entries")
	}
	if pa, _ := r.Lookup("a"); pa != mustLookup(t, clone, "a") {
		t.Error("clone must share the same provider instance")
	}
	clone.Remove("a")
	if clone.Has("a") {
		t.Error("Remove did not delete from clone")
	}
	if !r.Has("a") {
		t.Error("Remove on clone must not affect the source registry")
	}

	clone.Register("c", NewNoneProvider("c"))
	if r.Has("c") {
		t.Error("Register on clone must not affect the source registry")
	}
}

func TestRegistry_NilReceiverSafe(t *testing.T) {
	var r *Registry
	if r.Has("x") {
		t.Error("nil registry Has must be false")
	}
	r.Remove("x") // must not panic
	c := r.Clone()
	if c == nil || c.Len() != 0 {
		t.Errorf("nil registry Clone must yield an empty registry, got %v", c)
	}
}

func mustLookup(t *testing.T, r *Registry, name string) AuthProvider {
	t.Helper()
	p, err := r.Lookup(name)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", name, err)
	}
	return p
}
