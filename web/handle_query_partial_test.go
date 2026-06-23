package web

import (
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// testFindPartialRequest declares the full partial-match allowlist on Name so
// the cases below exercise every operator at the wire boundary. Email keeps a
// narrower set (eq + ieq + iin) so the cross-field rejection cases can prove
// each field independently honors its declared operators.
type testFindPartialRequest struct {
	Name  *string `query:"name"  filter:"eq,startswith,contains,ieq,ine,iin,inin,istartswith,icontains"`
	Email *string `query:"email" filter:"eq,ieq,iin"`

	Limit *int64 `query:"limit"`
}

type testFindPartialQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *testFindPartialQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (r testFindPartialRequest) ToQuery(crit queries.ReadCriteria) *testFindPartialQuery {
	return &testFindPartialQuery{Criteria: crit}
}

type capturingPartialHandler struct {
	got *testFindPartialQuery
}

func (h *capturingPartialHandler) Handle(_ *configuration.AppContext, q *testFindPartialQuery) (queries.Page, error) {
	h.got = q
	return queries.Page{}, nil
}

// dispatchPartial wires the wrapper end to end and returns the criteria the
// handler observed plus the HTTP status. Lets each operator case stay focused
// on the assembled Filter shape.
func dispatchPartial(t *testing.T, query string) (queries.ReadCriteria, int) {
	t.Helper()
	app := fiber.New()
	pipe := newTestPipeline()
	h := &capturingPartialHandler{}
	app.Get("/users", HandleQueryWithParams(pipe, testFindPartialRequest{}, responses.RawDoc, h))

	resp, err := app.Test(httptest.NewRequest("GET", "/users"+query, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if h.got == nil {
		return queries.ReadCriteria{}, resp.StatusCode
	}
	return h.got.Criteria, resp.StatusCode
}

func TestPartialOps_StartsWith_EmitsAnchoredRegex(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.startswith=Bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["Name"].(map[string]any)
	if !ok {
		t.Fatalf("expected name to be a map, got %T (%v)", crit.Filter["Name"], crit.Filter["Name"])
	}
	if got["$regex"] != "^Bob" {
		t.Errorf("expected $regex='^Bob', got %v", got["$regex"])
	}
	if _, has := got["$options"]; has {
		t.Errorf("startswith must NOT carry $options, got %v", got)
	}
}

func TestPartialOps_Contains_EmitsUnanchoredRegex(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.contains=ob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, _ := crit.Filter["Name"].(map[string]any)
	if got["$regex"] != "ob" {
		t.Errorf("expected $regex='ob', got %v", got["$regex"])
	}
}

func TestPartialOps_IStartsWith_AddsInsensitiveOption(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.istartswith=bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, _ := crit.Filter["Name"].(map[string]any)
	if got["$regex"] != "^bob" {
		t.Errorf("expected $regex='^bob', got %v", got["$regex"])
	}
	if got["$options"] != "i" {
		t.Errorf("expected $options='i', got %v", got["$options"])
	}
}

func TestPartialOps_IContains_AddsInsensitiveOption(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.icontains=OB")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, _ := crit.Filter["Name"].(map[string]any)
	if got["$regex"] != "OB" {
		t.Errorf("expected $regex='OB', got %v", got["$regex"])
	}
	if got["$options"] != "i" {
		t.Errorf("expected $options='i', got %v", got["$options"])
	}
}

func TestPartialOps_IEq_AnchoredRegexWithInsensitive(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.ieq=bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, _ := crit.Filter["Name"].(map[string]any)
	if got["$regex"] != "^bob$" {
		t.Errorf("expected $regex='^bob$', got %v", got["$regex"])
	}
	if got["$options"] != "i" {
		t.Errorf("expected $options='i', got %v", got["$options"])
	}
}

func TestPartialOps_INe_WrapsRegexInNot(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.ine=bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, _ := crit.Filter["Name"].(map[string]any)
	inner, ok := got["$not"].(map[string]any)
	if !ok {
		t.Fatalf("expected $not sub-document, got %v", got["$not"])
	}
	if inner["$regex"] != "^bob$" || inner["$options"] != "i" {
		t.Errorf("expected $not={$regex:'^bob$', $options:'i'}, got %v", inner)
	}
}

func TestPartialOps_IIn_EmitsRegexMatchListSentinel(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.iin=Bob,Alice")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["Name"].(queries.RegexMatchList)
	if !ok {
		t.Fatalf("expected RegexMatchList sentinel, got %T", crit.Filter["Name"])
	}
	if !got.CaseInsensitive || got.Negate {
		t.Errorf("expected CaseInsensitive=true Negate=false, got %+v", got)
	}
	if len(got.Patterns) != 2 || got.Patterns[0] != "^Bob$" || got.Patterns[1] != "^Alice$" {
		t.Errorf("expected anchored patterns [^Bob$, ^Alice$], got %v", got.Patterns)
	}
}

func TestPartialOps_INin_EmitsNegatedSentinel(t *testing.T) {
	crit, status := dispatchPartial(t, "?name.inin=Bob,Alice")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, ok := crit.Filter["Name"].(queries.RegexMatchList)
	if !ok {
		t.Fatalf("expected RegexMatchList sentinel, got %T", crit.Filter["Name"])
	}
	if !got.Negate {
		t.Errorf("expected Negate=true, got %+v", got)
	}
}

func TestPartialOps_MetacharsAreEscaped(t *testing.T) {
	// A user-supplied "a.b*c" must be treated as literal — not as a regex
	// that matches "a<any>b<any times>c". applyFilterParam runs
	// regexp.QuoteMeta on the value before emitting it.
	crit, status := dispatchPartial(t, "?name.contains=a.b*c")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, _ := crit.Filter["Name"].(map[string]any)
	if got["$regex"] != `a\.b\*c` {
		t.Errorf("expected metacharacters escaped, got %v", got["$regex"])
	}
}

func TestPartialOps_OperatorOutsideDeclaredListReturns400(t *testing.T) {
	// Email declares only eq,ieq,iin — using .startswith must 400.
	_, status := dispatchPartial(t, "?email.startswith=jane")
	if status != fiber.StatusBadRequest {
		t.Fatalf("expected 400 for undeclared operator, got %d", status)
	}
}

func TestPartialOps_CoexistWithEqOnSameField(t *testing.T) {
	// Name declares eq alongside the partial ops — exact match still works.
	crit, status := dispatchPartial(t, "?name=Bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if crit.Filter["Name"] != "Bob" {
		t.Errorf("expected plain value Bob, got %v (%T)", crit.Filter["Name"], crit.Filter["Name"])
	}
}

func TestPartialOps_MultipleOpsOnSameField_FoldIntoMultiClause(t *testing.T) {
	// The wire delivers four operators on the same field. Before the fix
	// each new operator overwrote the previous one on the criteria map and
	// only the last one survived, so `?name=Bob Smith&name.icontains=smh`
	// silently dropped the icontains constraint. The wrapper now folds the
	// clauses into a queries.MultiClause sentinel; the canonical
	// MongoViewReader expands it into a top-level `$and` array.
	crit, status := dispatchPartial(t, "?name=Bob%20Smith&name.startswith=Bob&name.icontains=smh&name.istartswith=bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	mc, ok := crit.Filter["Name"].(queries.MultiClause)
	if !ok {
		t.Fatalf("expected MultiClause for name, got %T (%v)", crit.Filter["Name"], crit.Filter["Name"])
	}
	if len(mc.Clauses) != 4 {
		t.Fatalf("expected 4 folded clauses, got %d (%v)", len(mc.Clauses), mc.Clauses)
	}
	// First clause is the eq value (plain string, no operator sub-document).
	if mc.Clauses[0] != "Bob Smith" {
		t.Errorf("clause 0: expected 'Bob Smith', got %v (%T)", mc.Clauses[0], mc.Clauses[0])
	}
	// Subsequent clauses are sub-documents carrying their declared operator.
	startsWith, ok := mc.Clauses[1].(map[string]any)
	if !ok || startsWith["$regex"] != "^Bob" {
		t.Errorf("clause 1: expected {$regex:'^Bob'}, got %v", mc.Clauses[1])
	}
	if _, has := startsWith["$options"]; has {
		t.Errorf("clause 1: startswith must not carry $options, got %v", startsWith)
	}
	icontains, ok := mc.Clauses[2].(map[string]any)
	if !ok || icontains["$regex"] != "smh" || icontains["$options"] != "i" {
		t.Errorf("clause 2: expected {$regex:'smh', $options:'i'}, got %v", mc.Clauses[2])
	}
	istartsWith, ok := mc.Clauses[3].(map[string]any)
	if !ok || istartsWith["$regex"] != "^bob" || istartsWith["$options"] != "i" {
		t.Errorf("clause 3: expected {$regex:'^bob', $options:'i'}, got %v", mc.Clauses[3])
	}
}

func TestPartialOps_TwoOpsOnSameField_PromotesToMultiClause(t *testing.T) {
	// A common real-world range query: `?age.gte=18&age.lte=65`. The two
	// numeric operators must both reach the store as AND-ed constraints.
	// Asserts the promotion path (single value → MultiClause) on exactly
	// two clauses, without the noise of the four-operator stress case.
	crit, status := dispatchPartial(t, "?name.startswith=Bob&name.icontains=ob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	mc, ok := crit.Filter["Name"].(queries.MultiClause)
	if !ok {
		t.Fatalf("expected MultiClause for name, got %T", crit.Filter["Name"])
	}
	if len(mc.Clauses) != 2 {
		t.Fatalf("expected 2 clauses, got %d", len(mc.Clauses))
	}
}

func TestPartialOps_SingleOpStillPlainValue(t *testing.T) {
	// Regression: when only one operator targets a field, the criteria
	// map must keep the canonical `{field: clause}` shape (not promote
	// to MultiClause) so Mongo indexes are usable without an outer $and.
	crit, status := dispatchPartial(t, "?name.startswith=Bob")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if _, ok := crit.Filter["Name"].(queries.MultiClause); ok {
		t.Fatalf("expected single-clause to stay plain, got MultiClause")
	}
	if _, ok := crit.Filter["Name"].(map[string]any); !ok {
		t.Fatalf("expected sub-document, got %T", crit.Filter["Name"])
	}
}

func TestPartialOps_DifferentFieldsStayFlat(t *testing.T) {
	// AND across two distinct fields stays as separate top-level entries
	// on the criteria map — MultiClause only kicks in for collisions on
	// the same field name.
	crit, status := dispatchPartial(t, "?name.startswith=Bob&email=jane@x")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if _, ok := crit.Filter["Name"].(queries.MultiClause); ok {
		t.Fatalf("name must not promote to MultiClause when alone")
	}
	if _, ok := crit.Filter["Email"].(queries.MultiClause); ok {
		t.Fatalf("email must not promote to MultiClause when alone")
	}
	if crit.Filter["Email"] != "jane@x" {
		t.Errorf("expected email='jane@x', got %v", crit.Filter["Email"])
	}
}

func TestPartialOps_BlankValueStillEscapes(t *testing.T) {
	// Empty input becomes an empty regex, which Mongo treats as "match
	// anything". We do not special-case this — it is the caller's choice
	// whether to allow it via DTO validation; the wrapper just escapes.
	crit, status := dispatchPartial(t, "?name.contains=")
	if status != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	got, _ := crit.Filter["Name"].(map[string]any)
	if got["$regex"] != "" {
		t.Errorf("expected empty $regex, got %v", got["$regex"])
	}
}
