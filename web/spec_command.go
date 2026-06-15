package web

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/gofiber/fiber/v3"
)

// HandleCommandWithIDSpec is the openapi-aware sibling of
// HandleCommandWithID. The fiber.Handler returned is functionally
// identical — same closure, same dispatch path, same response semantics
// — paired with a RouteSpec that documents the route's shape so
// openapi.Mount can register it on the spec assembler.
//
// HasPathID is true: this wrapper auto-binds the Fiber :id segment via
// the pipeline.CommandWithID interface. RequestType is nil: bodyless
// route. ResponseType is the wire shape of the response projection
// (TResp); detection of responses.None for the "envelope without data"
// case happens during spec assembly, not here.
func HandleCommandWithIDSpec[
	T any,
	TCmd interface {
		*T
		pipeline.CommandWithID
	},
	TResult any,
	TResp any,
](
	pipe *pipeline.Pipeline,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TCmd, TResult],
	successStatus int,
) (fiber.Handler, openapi.RouteSpec) {
	handler := HandleCommandWithID[T, TCmd, TResult, TResp](pipe, responseProjection, h, successStatus)
	return handler, openapi.RouteSpec{
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: successStatus,
		HasPathID:     true,
	}
}

// HandleCommandWithBodySpec is the openapi-aware sibling of
// HandleCommandWithBody. RouteSpec.Strict mirrors the handler's
// pipeline.FullBody marker via the same type-assertion the wrapper
// performs internally — strict handlers (typically PUT in the canonical
// vocabulary) produce a schema where every kept field is required;
// lenient handlers produce a schema where pointer / `,omitempty` fields
// are optional.
func HandleCommandWithBodySpec[
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.Command
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
	handler := HandleCommandWithBody[TReq, TCmd, TCmdPtr, TResult, TResp](pipe, sample, responseProjection, h, successStatus)
	_, isStrict := any(h).(pipeline.FullBodyEnforcer)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: successStatus,
		Strict:        isStrict,
	}
}

// HandleCommandWithBodyIDSpec is the openapi-aware sibling of
// HandleCommandWithBodyID. Combines body-carrying semantics with
// HasPathID — the canonical PUT / PATCH on /resource/:id surface. Strict
// is detected the same way as HandleCommandWithBodySpec.
func HandleCommandWithBodyIDSpec[
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithID
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
	handler := HandleCommandWithBodyID[TReq, TCmd, TCmdPtr, TResult, TResp](pipe, sample, responseProjection, h, successStatus)
	_, isStrict := any(h).(pipeline.FullBodyEnforcer)
	return handler, openapi.RouteSpec{
		RequestType:   reflect.TypeOf((*TReq)(nil)).Elem(),
		ResponseType:  reflect.TypeOf((*TResp)(nil)).Elem(),
		SuccessStatus: successStatus,
		Strict:        isStrict,
		HasPathID:     true,
	}
}
