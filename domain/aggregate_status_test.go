package domain

import "testing"

// OperationOf is the single source of truth for how an aggregate child's
// (originalStatus, currentStatus) pair maps to a persistence operation — crossing
// BOTH statuses (mirrors the reference ddd-kernel). The three mixed cases are the
// ones a currentStatus-only reading got wrong.
func TestOperationOf(t *testing.T) {
	cases := []struct {
		name              string
		original, current AggregateItemStatus
		want              AggregateItemOp
	}{
		{"new item present", StatusAdded, StatusAdded, OpInsert},
		{"new item changed (still insert its final state)", StatusAdded, StatusChanged, OpInsert},
		{"new item added then removed (never persisted)", StatusAdded, StatusRemoved, OpNoop},
		{"db item untouched", StatusConstructor, StatusConstructor, OpNoop},
		{"db item re-added (update, not insert)", StatusConstructor, StatusAdded, OpUpdate},
		{"db item changed", StatusConstructor, StatusChanged, OpUpdate},
		{"db item removed", StatusConstructor, StatusRemoved, OpDelete},
	}
	for _, c := range cases {
		if got := OperationOf(c.original, c.current); got != c.want {
			t.Errorf("%s: OperationOf(%v, %v) = %v, want %v", c.name, c.original, c.current, got, c.want)
		}
	}
}
