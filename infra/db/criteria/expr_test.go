package criteria

import "testing"

// recordingVisitor is a hand-rolled Visitor that records which Visit method
// Accept dispatched to, so the double-dispatch wiring of every Expr node is
// observable without a real backend translator.
type recordingVisitor struct {
	comparison *Comparison
	logical    *Logical
	negation   *Negation
	subquery   *SubqueryComparison
	existence  *Existence
}

func (r *recordingVisitor) VisitComparison(c Comparison) error { r.comparison = &c; return nil }
func (r *recordingVisitor) VisitLogical(l Logical) error       { r.logical = &l; return nil }
func (r *recordingVisitor) VisitNot(n Negation) error          { r.negation = &n; return nil }
func (r *recordingVisitor) VisitSubquery(c SubqueryComparison) error {
	r.subquery = &c
	return nil
}
func (r *recordingVisitor) VisitExistence(e Existence) error { r.existence = &e; return nil }

// fakeSource is the smallest thing a SubQuery can read from — the Source
// interface names a table and nothing else, precisely so this package never has
// to know what a schema is.
type fakeSource struct{ table string }

func (f fakeSource) Table() string { return f.table }

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

// ─── Subquery tree ───────────────────────────────────────────────────────────
//
// This package builds the tree and validates nothing — every refusal belongs to
// the translator, which is the only layer that knows what a source can resolve.
// What IS this package's business: that each builder produces the node and the
// operator it claims, that the projection counter distinguishes none/one/many,
// and that Accept dispatches the two new nodes.

func TestSubBuilders_NodeAndOperator(t *testing.T) {
	src := fakeSource{table: "roles"}
	cases := []struct {
		name string
		expr Expr
		op   Operator
	}{
		{"InSub", InSub("A", Sub(src).Select("B")), OpIn},
		{"NinSub", NinSub("A", Sub(src).Select("B")), OpNin},
		{"EqSub", EqSub("A", Sub(src).Select("B")), OpEq},
		{"NeSub", NeSub("A", Sub(src).Select("B")), OpNe},
		{"GtSub", GtSub("A", Sub(src).Select("B")), OpGt},
		{"GteSub", GteSub("A", Sub(src).Select("B")), OpGte},
		{"LtSub", LtSub("A", Sub(src).Select("B")), OpLt},
		{"LteSub", LteSub("A", Sub(src).Select("B")), OpLte},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, ok := c.expr.(SubqueryComparison)
			if !ok {
				t.Fatalf("%s produced %T, want SubqueryComparison", c.name, c.expr)
			}
			if sc.Op != c.op {
				t.Errorf("op = %q, want %q", sc.Op, c.op)
			}
			if sc.Field != "A" || sc.Sub == nil || sc.Sub.Src.Table() != "roles" {
				t.Errorf("node lost its field or its source: %+v", sc)
			}
		})
	}
}

func TestSubBuilders_ProjectionCounter(t *testing.T) {
	src := fakeSource{table: "roles"}
	if n := Sub(src).Selects(); n != 0 {
		t.Errorf("a fresh Sub projects %d items, want 0", n)
	}
	if n := Sub(src).Select("B").Selects(); n != 1 {
		t.Errorf("Select recorded %d, want 1", n)
	}
	if n := Sub(src).Select("B").SelectMax("C").Selects(); n != 2 {
		t.Errorf("two projections recorded %d, want 2 (the translator refuses it)", n)
	}
	if n := (*SubQuery)(nil).Selects(); n != 0 {
		t.Errorf("a nil SubQuery reported %d projections, want 0", n)
	}
}

func TestSubBuilders_AggregateAndEnvelope(t *testing.T) {
	src := fakeSource{table: "roles"}
	for _, c := range []struct {
		name string
		sub  *SubQuery
		fn   AggFunc
	}{
		{"count", Sub(src).SelectCount(), AggCount},
		{"max", Sub(src).SelectMax("B"), AggMax},
		{"min", Sub(src).SelectMin("B"), AggMin},
		{"sum", Sub(src).SelectSum("B"), AggSum},
		{"avg", Sub(src).SelectAvg("B"), AggAvg},
		{"plain", Sub(src).Select("B"), AggNone},
	} {
		if c.sub.Func != c.fn {
			t.Errorf("%s: func = %q, want %q", c.name, c.sub.Func, c.fn)
		}
	}

	s := Sub(src).Select("B").OrderBy("B").OrderByDesc("C").Limit(3).OnlyArchived().All()
	if len(s.Order) != 2 || s.Order[0].Desc || !s.Order[1].Desc {
		t.Errorf("order terms lost their direction: %+v", s.Order)
	}
	if s.LimitN != 3 || s.Scope != ScopeOnlyArchived || s.Quant != QuantAll {
		t.Errorf("envelope not carried: %+v", s)
	}
	if Sub(src).IncludeArchived().Scope != ScopeIncludeArchived {
		t.Error("IncludeArchived did not move the scope")
	}
	if Sub(src).Any().Quant != QuantAny {
		t.Error("Any did not set the quantifier")
	}
}

func TestSubBuilders_WhereCarriesThePredicate(t *testing.T) {
	s := Sub(fakeSource{table: "roles"}).Select("B").Where(Eq("C", 1))
	if s.Predicate == nil {
		t.Fatal("Where did not carry the predicate")
	}
	if _, ok := s.Predicate.(Comparison); !ok {
		t.Errorf("predicate = %T, want the Comparison it was handed", s.Predicate)
	}
}

func TestSubBuilders_DefaultScopeIsActive(t *testing.T) {
	// The archive gate the framework applies to every other read applies inside a
	// subquery too, without the developer asking — which starts here.
	if s := Sub(fakeSource{table: "roles"}); s.Scope != ScopeActive {
		t.Errorf("a fresh Sub starts at scope %v, want ScopeActive", s.Scope)
	}
}

func TestAccept_DispatchesSubqueryNodes(t *testing.T) {
	src := fakeSource{table: "roles"}

	v := &recordingVisitor{}
	if err := InSub("A", Sub(src).Select("B")).Accept(v); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if v.subquery == nil || v.subquery.Field != "A" {
		t.Errorf("VisitSubquery not invoked with the node: %+v", v.subquery)
	}

	v = &recordingVisitor{}
	if err := NotExists(Sub(src)).Accept(v); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if v.existence == nil || !v.existence.Negated {
		t.Errorf("VisitExistence not invoked with the negated node: %+v", v.existence)
	}

	v = &recordingVisitor{}
	if err := Exists(Sub(src)).Accept(v); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if v.existence == nil || v.existence.Negated {
		t.Errorf("Exists dispatched as negated: %+v", v.existence)
	}
}

func TestOuter_IsAValue(t *testing.T) {
	// Correlation costs no builders precisely because an OuterRef travels as an
	// ordinary comparison value.
	c, ok := Eq("RoleID", Outer("ID")).(Comparison)
	if !ok {
		t.Fatalf("Eq with an Outer produced %T", Eq("RoleID", Outer("ID")))
	}
	ref, ok := c.Values[0].(OuterRef)
	if !ok || ref.Field != "ID" {
		t.Errorf("the outer reference did not survive as the value: %+v", c.Values)
	}
}
