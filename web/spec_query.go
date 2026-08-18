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
// reserved pagination key). ResponseType captures TResp — the wire shape of
// one projected page item; the assembler envelopes it as
// `Response{Data: []TResp, Pagination: PaginationInfo}` for the success
// response (Paged:true on the RouteSpec).
//
// Strict is always false on the read side (no FullBody marker semantic
// applies). HasPathID is false: this wrapper has no :id binding. Paged
// is true: the runtime emits via fwweb.RespondPaged and the spec
// mirrors the shape.
func QueryWithParamsSpec[
	TReq HasToParamsQuery[TQ], TQ queries.QueryWithParams[TResult], TResult any, TResp any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TQ, queries.PageOf[TResult]],
) (fiber.Handler, openapi.RouteSpec) {
	handler := QueryWithParams[TReq, TQ, TResult, TResp](pipe, sample, responseProjection, h)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
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
	TReq HasToIDQuery[TQ], TQ queries.QueryByID[TResult], TResult any, TResp any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TQ, TResult],
) (fiber.Handler, openapi.RouteSpec) {
	handler := QueryByID[TReq, TQ, TResult, TResp](pipe, sample, responseProjection, h)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: fiber.StatusOK,
		HasPathID:     true,
	}
}
