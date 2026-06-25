package web

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

type specParamsQuery struct {
	pipeline.RequestBase
	pipeline.QueryBase
	criteria queries.ReadCriteria
}

func (q *specParamsQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.criteria, nil
}

type specParamsReq struct {
	Name *string `query:"name" filter:"eq"`
}

func (specParamsReq) ToQuery(criteria queries.ReadCriteria) *specParamsQuery {
	return &specParamsQuery{criteria: criteria}
}

type specPageItem struct {
	ID string `json:"id"`
}

func (specPageItem) FromDoc(doc map[string]any) specPageItem {
	if id, ok := doc["id"].(string); ok {
		return specPageItem{ID: id}
	}
	return specPageItem{}
}

type specParamsHandler struct{}

func (specParamsHandler) Handle(_ *configuration.AppContext, _ *specParamsQuery) (queries.Page, error) {
	return queries.Page{}, nil
}

type specByIDQuery struct {
	pipeline.RequestBase
	queries.QueryBaseWithID
}

func (specByIDQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{}, nil
}

func (specByIDQuery) ContextName() string { return "Spec" }

type specByIDReq struct {
	IncludeArchived *bool `query:"includeArchived"`
}

func (specByIDReq) ToQuery() *specByIDQuery { return &specByIDQuery{} }

type specByIDHandler struct{}

func (specByIDHandler) Handle(_ *configuration.AppContext, _ *specByIDQuery) (map[string]any, error) {
	return map[string]any{}, nil
}

func projectRaw(doc map[string]any) specPageItem { return specPageItem{}.FromDoc(doc) }

// Ensure the fixture wiring is sound so the package still compiles when the
// underlying queries.QueryBaseWithID API drifts — touches both helpers we
// rely on.
var _ = domain.NewRandomID

// ─── HandleQueryWithParamsSpec ─────────────────────────────────────────────

func TestHandleQueryWithParamsSpec_BasicShape(t *testing.T) {
	pipe := pipeline.New(translation.New())
	h, spec := HandleQueryWithParamsSpec[
		specParamsReq, *specParamsQuery, specPageItem,
	](pipe, specParamsReq{}, projectRaw, specParamsHandler{})

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
		t.Fatal("HandleQueryWithParams has no :id binding; HasPathID must be false")
	}
	if spec.Strict {
		t.Fatal("read-side wrappers do not consult FullBody; Strict must be false")
	}
	if !spec.Paged {
		t.Fatal("HandleQueryWithParams emits via RespondPaged; Paged must be true so the spec mirrors data:[]R + pagination")
	}
}

// ─── HandleQueryByIDSpec ─────────────────────────────────────────────────

func TestHandleQueryByIDSpec_HasPathID(t *testing.T) {
	pipe := pipeline.New(translation.New())
	h, spec := HandleQueryByIDSpec[
		specByIDReq, *specByIDQuery, specPageItem,
	](pipe, specByIDReq{}, projectRaw, specByIDHandler{})

	if h == nil {
		t.Fatal("handler should not be nil")
	}
	if !spec.HasPathID {
		t.Fatal("HandleQueryByID auto-binds :id; HasPathID must be true")
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
		t.Fatal("HandleQueryByID is single-item — Paged must stay false")
	}
}
