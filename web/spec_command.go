package web

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// CommandByIDSpec is the openapi-aware sibling of
// CommandByID. The fiber.Handler returned is functionally
// identical — same closure, same dispatch path, same response semantics
// — paired with a RouteSpec that documents the route's shape so
// openapi.Mount can register it on the spec assembler.
//
// HasPathID is true: this wrapper auto-binds the Fiber :id segment via
// the pipeline.CommandByID interface. RequestType is nil: bodyless
// route. ResponseType is the wire shape of the response projection
// (TResp); detection of responses.None for the "envelope without data"
// case happens during spec assembly, not here.
func CommandByIDSpec[
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandByID
	},
	TResult any,
	TResp any,
](
	pipe *pipeline.Pipeline,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TCmdPtr, TResult],
	successStatus int,
) (fiber.Handler, openapi.RouteSpec) {
	handler := CommandByID[TCmd, TCmdPtr, TResult, TResp](pipe, responseProjection, h, successStatus)
	return handler, openapi.RouteSpec{
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: successStatus,
		HasPathID:     true,
	}
}

// CommandWithBodySpec is the openapi-aware sibling of
// CommandWithBody. RouteSpec.Strict mirrors the handler's
// pipeline.FullBody marker via the same type-assertion the wrapper
// performs internally — strict handlers (typically PUT in the canonical
// vocabulary) produce a schema where every kept field is required;
// lenient handlers produce a schema where pointer / `,omitempty` fields
// are optional.
func CommandWithBodySpec[
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBody
	},
	TResult any,
	TResp any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TCmdPtr, TResult],
	successStatus int,
) (fiber.Handler, openapi.RouteSpec) {
	handler := CommandWithBody[TReq, TCmd, TCmdPtr, TResult, TResp](pipe, sample, responseProjection, h, successStatus)
	_, isStrict := any(h).(pipeline.FullBodyEnforcer)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: successStatus,
		Strict:        isStrict,
	}
}

// CommandWithBodyIDSpec is the openapi-aware sibling of
// CommandWithBodyID. Combines body-carrying semantics with
// HasPathID — the canonical PUT / PATCH on /resource/:id surface. Strict
// is detected the same way as CommandWithBodySpec.
func CommandWithBodyIDSpec[
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBodyID
	},
	TResult any,
	TResp any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TCmdPtr, TResult],
	successStatus int,
) (fiber.Handler, openapi.RouteSpec) {
	handler := CommandWithBodyID[TReq, TCmd, TCmdPtr, TResult, TResp](pipe, sample, responseProjection, h, successStatus)
	_, isStrict := any(h).(pipeline.FullBodyEnforcer)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: successStatus,
		Strict:        isStrict,
		HasPathID:     true,
	}
}
