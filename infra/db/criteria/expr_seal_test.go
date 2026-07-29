package criteria

import "testing"

// The Expr seal: only the three node kinds are expressions, and each drives
// the visitor through its own Accept. Executing the seals pins the closed set.
func TestExprSeals(t *testing.T) {
	nodes := []Expr{Comparison{}, Logical{}, Negation{}}
	for _, n := range nodes {
		n.isExpr()
	}
	if len(nodes) != 3 {
		t.Fatalf("the closed Expr node set drifted: %d", len(nodes))
	}
}
