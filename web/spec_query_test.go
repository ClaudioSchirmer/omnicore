package web

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────
//
// The read side is a three-type contract: an application-pure Result, the
// Query that fills it (FromQueryResult), and the wire Response the projector maps
// it into. The spec wrappers reflect TReq and TResp — the Result never
// reaches the OpenAPI assembler.

// specResult is the application-layer Result: tagless, field names matching
// the canonical document keys.
type specResult struct {
	ID *string
}

type specParamsQuery struct {
	pipeline.RequestBase
	pipeline.QueryBase
	criteria queries.ReadCriteria
}

func (q *specParamsQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.criteria, nil
}

func (q *specParamsQuery) FromQueryResult(_ *configuration.AppContext, r specResult) (specResult, error) {
	return r, nil
}

type specParamsReq struct {
	Name *string `query:"name" filter:"eq"`
}

func (specParamsReq) ToQuery(criteria queries.ReadCriteria) *specParamsQuery {
	return &specParamsQuery{criteria: criteria}
}

// specPageItem is the wire Response — the shape the RouteSpec advertises.
type specPageItem struct {
	ID string `json:"id"`
}

func (specPageItem) FromResult(r specResult) specPageItem {
	return responses.Map[specPageItem](r)
}

type specParamsHandler struct{}

func (specParamsHandler) Handle(_ *configuration.AppContext, _ *specParamsQuery) (queries.PageOf[specResult], error) {
	return queries.PageOf[specResult]{}, nil
}

type specByIDQuery struct {
	pipeline.RequestBase
	queries.QueryByIDBase
}

func (specByIDQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{}, nil
}

func (specByIDQuery) FromQueryResult(_ *configuration.AppContext, r specResult) (specResult, error) {
	return r, nil
}

func (specByIDQuery) ContextName() string { return "Spec" }

type specByIDReq struct {
	IncludeArchived *bool `query:"includeArchived"`
}

func (specByIDReq) ToQuery(_ queries.ReadCriteria) *specByIDQuery { return &specByIDQuery{} }

type specByIDHandler struct{}

func (specByIDHandler) Handle(_ *configuration.AppContext, _ *specByIDQuery) (specResult, error) {
	return specResult{}, nil
}

// Ensure the fixture wiring is sound so the package still compiles when the
// underlying queries.QueryByIDBase API drifts — touches both helpers we
// rely on.
var _ = domain.NewRandomID

// ─── QueryWithParamsSpec ─────────────────────────────────────────────

func TestHandleQueryWithParamsSpec_BasicShape(t *testing.T) {
	pipe := pipeline.New(translation.New())
	h, spec := QueryWithParamsSpec[
		specParamsReq, *specParamsQuery, specResult, specPageItem,
	](pipe, specParamsReq{}, specPageItem{}.FromResult, specParamsHandler{})

	if h == nil {
		t.Fatal("handler should not be nil")
	}
	if spec.RequestType == nil || spec.RequestType.Name() != "specParamsReq" {
		t.Fatalf("RequestType: got %+v, want specParamsReq", spec.RequestType)
	}
	if spec.ResponseType == nil || spec.ResponseType.Name() != "specPageItem" {
		t.Fatalf("ResponseType: got %+v, want specPageItem", spec.ResponseType)
	}
	if spec.SuccessStatus != fiber.StatusOK {
		t.Fatalf("SuccessStatus: got %d, want 200", spec.SuccessStatus)
	}
	if spec.HasPathID {
		t.Fatal("QueryWithParams has no :id binding; HasPathID must be false")
	}
	if spec.Strict {
		t.Fatal("read-side wrappers do not consult FullBody; Strict must be false")
	}
	if !spec.Paged {
		t.Fatal("QueryWithParams emits via RespondPaged; Paged must be true so the spec mirrors data:[]R + pagination")
	}
}

// ─── QueryByIDSpec ─────────────────────────────────────────────────

func TestHandleQueryByIDSpec_HasPathID(t *testing.T) {
	pipe := pipeline.New(translation.New())
	h, spec := QueryByIDSpec[
		specByIDReq, *specByIDQuery, specResult, specPageItem,
	](pipe, specByIDReq{}, specPageItem{}.FromResult, specByIDHandler{})

	if h == nil {
		t.Fatal("handler should not be nil")
	}
	if !spec.HasPathID {
		t.Fatal("QueryByID auto-binds :id; HasPathID must be true")
	}
	if spec.RequestType == nil || spec.RequestType.Name() != "specByIDReq" {
		t.Fatalf("RequestType: got %+v, want specByIDReq", spec.RequestType)
	}
	if spec.ResponseType == nil || spec.ResponseType.Name() != "specPageItem" {
		t.Fatalf("ResponseType: got %+v, want specPageItem", spec.ResponseType)
	}
	if spec.SuccessStatus != fiber.StatusOK {
		t.Fatalf("SuccessStatus: got %d, want 200", spec.SuccessStatus)
	}
	if spec.Paged {
		t.Fatal("QueryByID is single-item — Paged must stay false")
	}
}
