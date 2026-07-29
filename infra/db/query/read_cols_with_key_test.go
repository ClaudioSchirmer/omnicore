package query

import (
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// readColsWithKey guarantees the join/group key is always selected, even when it
// is a column the schema does not list among its own (a sibling's shared-ID join
// column, owned by its parent). fetchInGrouped reads row[keyCol] to bucket
// results, so dropping the key silently loses the whole segment — the child
// sibling regression this locks against.
func TestReadColsWithKey(t *testing.T) {
	s := core.NewExternalSchema("dependent_health_plans").
		ID("id").
		Field("Provider", "health_plan_provider")

	base := []string{"id", "health_plan_provider"}
	if got := s.ReadColumns(); !reflect.DeepEqual(got, base) {
		t.Fatalf("ReadColumns() = %v, want %v", got, base)
	}

	// A join key the schema does not carry among its own columns is unioned in.
	if got := readColsWithKey(s, "dependent_id"); !reflect.DeepEqual(got, []string{"id", "health_plan_provider", "dependent_id"}) {
		t.Fatalf("readColsWithKey(dependent_id) = %v, want the key appended", got)
	}
	// A key already present is not duplicated.
	if got := readColsWithKey(s, "id"); !reflect.DeepEqual(got, base) {
		t.Fatalf("readColsWithKey(id) = %v, want unchanged", got)
	}
	// Empty key (fetchAll) adds nothing.
	if got := readColsWithKey(s, ""); !reflect.DeepEqual(got, base) {
		t.Fatalf("readColsWithKey(\"\") = %v, want unchanged", got)
	}
}
