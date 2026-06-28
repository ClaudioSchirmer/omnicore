package core

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestSortedKeys(t *testing.T) {
	got := SortedKeys(domain.Fields{"name": 1, "email": 2, "age": 3})
	want := []string{"age", "email", "name"}
	if len(got) != len(want) {
		t.Fatalf("SortedKeys len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SortedKeys = %v, want %v", got, want)
		}
	}
	if n := len(SortedKeys(domain.Fields{})); n != 0 {
		t.Errorf("empty Fields → %d keys, want 0", n)
	}
}
