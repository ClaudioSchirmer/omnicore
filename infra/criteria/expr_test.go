package criteria

import "testing"

// recordingVisitor is a hand-rolled Visitor that records which Visit method
// Accept dispatched to, so the double-dispatch wiring of every Expr node is
// observable without a real backend translator.
type recordingVisitor struct {
	comparison *Comparison
	logical    *Logical
	negation   *Negation
}

func (r *recordingVisitor) VisitComparison(c Comparison) error { r.comparison = &c; return nil }
func (r *recordingVisitor) VisitLogical(l Logical) error       { r.logical = &l; return nil }
func (r *recordingVisitor) VisitNot(n Negation) error          { r.negation = &n; return nil }

// ─── Leaf builders not yet exercised elsewhere ───────────────────────────────

func TestLeafBuilders_OperatorAndCardinality(t *testing.T) {
	cases := []struct {
		name   string
		expr   Expr
		op     Operator
		values int
	}{
		{"Ne", Ne("A", 1), OpNe, 1},
		{"Gt", Gt("A", 1), OpGt, 1},
		{"Lt", Lt("A", 1), OpLt, 1},
		{"Gte", Gte("A", 1), OpGte, 1},
		{"Lte", Lte("A", 1), OpLte, 1},
		{"Nin", Nin("A", 1, 2), OpNin, 2},
		{"Like", Like("A", "x%"), OpLike, 1},
		{"ILike", ILike("A", "x%"), OpILike, 1},
		{"NotNull", NotNull("A"), OpNotNull, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmp, ok := c.expr.(Comparison)
			if !ok {
				t.Fatalf("%s did not produce a Comparison: %T", c.name, c.expr)
			}
			if cmp.Field != "A" {
				t.Errorf("field = %q, want A", cmp.Field)
			}
			if cmp.Op != c.op {
				t.Errorf("op = %q, want %q", cmp.Op, c.op)
			}
			if len(cmp.Values) != c.values {
				t.Errorf("values = %d, want %d", len(cmp.Values), c.values)
			}
		})
	}
}

// ─── Accept double-dispatch (drives isExpr sealing + Accept on each node) ─────

func TestAccept_DispatchesComparison(t *testing.T) {
	v := &recordingVisitor{}
	if err := Eq("Email", "a@b.c").Accept(v); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if v.comparison == nil || v.comparison.Field != "Email" {
		t.Errorf("VisitComparison not invoked with the leaf: %+v", v.comparison)
	}
}

func TestAccept_DispatchesLogical(t *testing.T) {
	v := &recordingVisitor{}
	if err := And(Eq("A", 1), Eq("B", 2)).Accept(v); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if v.logical == nil || v.logical.Op != LogicalAnd || len(v.logical.Operands) != 2 {
		t.Errorf("VisitLogical not invoked with the node: %+v", v.logical)
	}
}

func TestAccept_DispatchesNegation(t *testing.T) {
	v := &recordingVisitor{}
	if err := Not(Eq("A", 1)).Accept(v); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if v.negation == nil {
		t.Fatal("VisitNot not invoked")
	}
	if _, ok := v.negation.Inner.(Comparison); !ok {
		t.Errorf("negation inner = %T, want Comparison", v.negation.Inner)
	}
}
