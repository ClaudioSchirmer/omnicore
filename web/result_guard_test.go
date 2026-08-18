package web

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
)

// The read-side boot guards fired by the QueryWithParams / QueryByID
// constructors. The generic TResult→TResp mapper is NAME-based, so the
// constructors refuse to mount a route whose Response is not fully backed by
// its Result, whose Result leaks wire naming (json tags), or — on a
// `query:"fields"` endpoint — whose Result cannot express "absent". Each case
// asserts the panic message NAMES the offending field, which is what makes the
// diagnostic actionable at boot.

// ─── shared query plumbing for the aligned Result ──────────────────────────

// guardAlignResult is a well-formed application-layer Result: tagless, every
// field a pointer.
type guardAlignResult struct {
	ID   *string
	Name *string
}

type guardAlignQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *guardAlignQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (q *guardAlignQuery) FromQueryResult(_ *configuration.AppContext, r guardAlignResult) (guardAlignResult, error) {
	return r, nil
}

// guardAlignRequest declares no reserved control — the fields-side guards stay
// dormant so the alignment guard is the only thing under test.
type guardAlignRequest struct {
	Name *string `query:"name" filter:"eq"`
}

func (r guardAlignRequest) ToQuery(crit queries.ReadCriteria) *guardAlignQuery {
	return &guardAlignQuery{Criteria: crit}
}

type guardAlignHandler struct{}

func (guardAlignHandler) Handle(_ *configuration.AppContext, _ *guardAlignQuery) (queries.PageOf[guardAlignResult], error) {
	return queries.PageOf[guardAlignResult]{}, nil
}

// guardUnbackedResponse declares Nickname, which no Result field backs — the
// mapper would silently emit a permanently empty wire field.
type guardUnbackedResponse struct {
	responses.Auto
	ID       *string `json:"id,omitempty"`
	Nickname *string `json:"nickname,omitempty"`
}

func (guardUnbackedResponse) FromResult(r guardAlignResult) guardUnbackedResponse {
	return responses.AutoFromResult[guardUnbackedResponse](r)
}

func TestQueryWithParams_BootPanicsOnUnbackedResponseField(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected boot panic when a Response field has no same-named Result field")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Nickname") {
			t.Errorf("panic must name the offending field, got: %s", msg)
		}
		if !strings.Contains(msg, "guardAlignResult") {
			t.Errorf("panic must name the Result type, got: %s", msg)
		}
	}()
	_ = QueryWithParams(newTestPipeline(), guardAlignRequest{}, guardUnbackedResponse{}.FromResult, guardAlignHandler{})
}

// ─── the same guard on the by-id constructor ───────────────────────────────

type guardAlignByIDQuery struct {
	queries.QueryByIDBase
}

func (q *guardAlignByIDQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{}, nil
}
func (q *guardAlignByIDQuery) ContextName() string { return "" }
func (q *guardAlignByIDQuery) FromQueryResult(_ *configuration.AppContext, r guardAlignResult) (guardAlignResult, error) {
	return r, nil
}

type guardAlignByIDRequest struct{}

func (guardAlignByIDRequest) ToQuery(_ queries.ReadCriteria) *guardAlignByIDQuery {
	return &guardAlignByIDQuery{}
}

type guardAlignByIDHandler struct{}

func (guardAlignByIDHandler) Handle(_ *configuration.AppContext, _ *guardAlignByIDQuery) (guardAlignResult, error) {
	return guardAlignResult{}, nil
}

func TestQueryByID_BootPanicsOnUnbackedResponseField(t *testing.T) {
	resetPathSchemaCache()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected boot panic when a Response field has no same-named Result field")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Nickname") {
			t.Errorf("panic must name the offending field, got: %s", msg)
		}
	}()
	_ = QueryByID(newTestPipeline(), guardAlignByIDRequest{}, guardUnbackedResponse{}.FromResult, guardAlignByIDHandler{})
}

// ─── a json tag on the Result is a layering violation ──────────────────────

// guardTaggedResult carries a wire tag — the three-name model reserves json
// naming for the Response DTO, so the guard rejects it at boot.
type guardTaggedResult struct {
	ID   *string
	Name *string `json:"name"`
}

type guardTaggedQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *guardTaggedQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (q *guardTaggedQuery) FromQueryResult(_ *configuration.AppContext, r guardTaggedResult) (guardTaggedResult, error) {
	return r, nil
}

type guardTaggedRequest struct {
	Name *string `query:"name" filter:"eq"`
}

func (r guardTaggedRequest) ToQuery(crit queries.ReadCriteria) *guardTaggedQuery {
	return &guardTaggedQuery{Criteria: crit}
}

type guardTaggedHandler struct{}

func (guardTaggedHandler) Handle(_ *configuration.AppContext, _ *guardTaggedQuery) (queries.PageOf[guardTaggedResult], error) {
	return queries.PageOf[guardTaggedResult]{}, nil
}

// guardTaggedResponse is perfectly aligned — the violation is on the Result.
type guardTaggedResponse struct {
	responses.Auto
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
}

func (guardTaggedResponse) FromResult(r guardTaggedResult) guardTaggedResponse {
	return responses.AutoFromResult[guardTaggedResponse](r)
}

func TestQueryWithParams_BootPanicsOnJSONTaggedResultField(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected boot panic when a Result field carries a json tag")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Name") || !strings.Contains(msg, "json tag") {
			t.Errorf("panic must name the offending field and the json-tag reason, got: %s", msg)
		}
	}()
	_ = QueryWithParams(newTestPipeline(), guardTaggedRequest{}, guardTaggedResponse{}.FromResult, guardTaggedHandler{})
}

// ─── ValidateFieldsResult — sparse contract on the Result ──────────────────

// guardSparseResult aligns with its Response but declares Name as a VALUE
// string: under `?fields=` the reader may not return the key at all, and a
// non-pointer Result cannot tell "absent" from "empty".
type guardSparseResult struct {
	ID   *string
	Name string
}

type guardSparseQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *guardSparseQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (q *guardSparseQuery) FromQueryResult(_ *configuration.AppContext, r guardSparseResult) (guardSparseResult, error) {
	return r, nil
}

// guardSparseRequest opts into `?fields=`, which is what arms both sparse
// guards (Response-side ValidateFieldsResponse, Result-side ValidateFieldsResult).
type guardSparseRequest struct {
	Name   *string `query:"name" filter:"eq"`
	Fields *string `query:"fields"`
}

func (r guardSparseRequest) ToQuery(crit queries.ReadCriteria) *guardSparseQuery {
	return &guardSparseQuery{Criteria: crit}
}

type guardSparseHandler struct{}

func (guardSparseHandler) Handle(_ *configuration.AppContext, _ *guardSparseQuery) (queries.PageOf[guardSparseResult], error) {
	return queries.PageOf[guardSparseResult]{}, nil
}

// guardSparseResponse honors the wire-side sparse contract (every field *T +
// ,omitempty), so the Response-side guard passes and the Result-side guard is
// unambiguously the one that fires.
type guardSparseResponse struct {
	responses.Auto
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
}

func (guardSparseResponse) FromResult(r guardSparseResult) guardSparseResponse {
	return responses.AutoFromResult[guardSparseResponse](r)
}

func TestQueryWithParams_BootPanicsOnNonPointerResultFieldWithFields(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected boot panic when a query:\"fields\" endpoint has a non-pointer Result field")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Name") {
			t.Errorf("panic must name the offending field, got: %s", msg)
		}
		if !strings.Contains(msg, "guardSparseResult") {
			t.Errorf("panic must name the Result type, got: %s", msg)
		}
		if !strings.Contains(msg, "fields") {
			t.Errorf("panic must attribute the guard to the fields opt-in, got: %s", msg)
		}
	}()
	_ = QueryWithParams(newTestPipeline(), guardSparseRequest{}, guardSparseResponse{}.FromResult, guardSparseHandler{})
}

// TestQueryWithParams_NoFieldsOptInSkipsResultSparseGuard proves the Result
// sparse guard is scoped to the `?fields=` opt-in: the same offending Result
// mounts cleanly on an endpoint that does not accept the control.
func TestQueryWithParams_NoFieldsOptInSkipsResultSparseGuard(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("did not expect a panic without the fields opt-in, got: %v", r)
		}
	}()
	_ = QueryWithParams(newTestPipeline(), guardNoFieldsRequest{}, guardSparseResponse{}.FromResult, guardSparseHandler{})
}

type guardNoFieldsRequest struct {
	Name *string `query:"name" filter:"eq"`
}

func (r guardNoFieldsRequest) ToQuery(crit queries.ReadCriteria) *guardSparseQuery {
	return &guardSparseQuery{Criteria: crit}
}
