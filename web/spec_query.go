package web

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// QueryWithParamsSpec is the openapi-aware sibling of
// QueryWithParams. RequestType captures TReq (carries the
// `query:"X" filter:"ops"` allowlist tags the spec assembler reads to
// emit one OpenAPI parameter per declared filter + operator and per
// reserved pagination key). ResponseType captures R — the wire shape of
// one projected page item; the assembler envelopes it as
// `Response{Data: []R, Pagination: PaginationInfo}` for the success
// response (Paged:true on the RouteSpec).
//
// Strict is always false on the read side (no FullBody marker semantic
// applies). HasPathID is false: this wrapper has no :id binding. Paged
// is true: the runtime emits via fwweb.RespondPaged and the spec
// mirrors the shape.
func QueryWithParamsSpec[
	TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams, R any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	projector func(map[string]any) R,
	h pipeline.Handler[TQ, queries.Page],
) (fiber.Handler, openapi.RouteSpec) {
	handler := QueryWithParams[TReq, TQ, R](pipe, sample, projector, h)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*R)(nil)).Elem(),
		SuccessStatus: fiber.StatusOK,
		Paged:         true,
	}
}

// QueryByIDSpec is the openapi-aware sibling of
// QueryByID. HasPathID is true (the wrapper auto-binds the
// Fiber :id segment via the queries.QueryByID interface).
// RequestType still captures TReq so the assembler emits the optional
// ?includeArchived query parameter declared on the DTO.
func QueryByIDSpec[
	TReq HasToIDQuery[TQ], TQ queries.QueryByID, R any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	projector func(map[string]any) R,
	h pipeline.Handler[TQ, map[string]any],
) (fiber.Handler, openapi.RouteSpec) {
	handler := QueryByID[TReq, TQ, R](pipe, sample, projector, h)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*R)(nil)).Elem(),
		SuccessStatus: fiber.StatusOK,
		HasPathID:     true,
	}
}
