package queryschema

import (
	"reflect"
	"testing"
)

type blankAddress struct {
	City    string
	ZipCode string
}

type blankResult struct {
	ID        string
	Name      string
	Nickname  *string
	Addresses []blankAddress
	Home      *blankAddress
}

func TestBlankResultPaths_EmptyPathsIsIdentity(t *testing.T) {
	in := blankResult{ID: "1", Name: "Ana"}
	if out := BlankResultPaths(in, nil); !reflect.DeepEqual(out, in) {
		t.Fatalf("no paths must be the identity, got %+v", out)
	}
}

func TestBlankResultPaths_TopLevelFields(t *testing.T) {
	nick := "aninha"
	in := blankResult{ID: "1", Name: "Ana", Nickname: &nick}
	out := BlankResultPaths(in, []string{"Name", "Nickname"})
	if out.Name != "" || out.Nickname != nil {
		t.Fatalf("top-level blanking: %+v", out)
	}
	if out.ID != "1" {
		t.Fatalf("unnamed fields must survive: %+v", out)
	}
	// Top-level fields are zeroed on the returned copy only — the caller's
	// value is untouched (r travels by value).
	if in.Name != "Ana" || in.Nickname == nil {
		t.Fatalf("the input's top-level fields must not change: %+v", in)
	}
}

func TestBlankResultPaths_NestedThroughSliceAndPointer(t *testing.T) {
	in := blankResult{
		Addresses: []blankAddress{{City: "POA", ZipCode: "90000"}, {City: "SP", ZipCode: "01000"}},
		Home:      &blankAddress{City: "POA", ZipCode: "90000"},
	}
	out := BlankResultPaths(in, []string{"Addresses.City", "Home.ZipCode"})
	for i, a := range out.Addresses {
		if a.City != "" {
			t.Fatalf("slice element %d must blank the leaf, got %+v", i, a)
		}
		if a.ZipCode == "" {
			t.Fatalf("slice element %d must keep the other leaf", i)
		}
	}
	if out.Home.ZipCode != "" || out.Home.City != "POA" {
		t.Fatalf("pointer segment: %+v", out.Home)
	}
	// Documented sharing: a leaf reached through a slice or pointer is
	// zeroed on the SHARED backing memory the input still references.
	if in.Addresses[0].City != "" || in.Home.ZipCode != "" {
		t.Fatalf("nested blanking writes the shared backing memory by contract: %+v %+v", in.Addresses, in.Home)
	}
}

func TestBlankResultPaths_NilPointerIsSkipped(t *testing.T) {
	in := blankResult{ID: "1"}
	out := BlankResultPaths(in, []string{"Home.City", "Nickname"})
	if out.Home != nil || out.ID != "1" {
		t.Fatalf("a nil pointer segment has nothing to blank: %+v", out)
	}
}

func TestBlankResultPaths_UnresolvablePathIsSkipped(t *testing.T) {
	in := blankResult{ID: "1", Name: "Ana"}
	out := BlankResultPaths(in, []string{"Bogus", "Addresses.Bogus", "Name.TooDeep"})
	if out.ID != "1" || out.Name != "Ana" {
		t.Fatalf("unresolvable paths must be no-ops, got %+v", out)
	}
}
