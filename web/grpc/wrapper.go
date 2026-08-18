package grpc

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// handleCommandWithBody adapts a pipeline command handler to a Connect RPC
// — the protobuf sibling of web.CommandWithBody (create-style: the
// full input rides the request message). Connect decodes the wire;
// the wrapper then runs the strict presence check (when configured), maps
// the message to the command via toCommand (the RequestDTO.ToCommand seat),
// dispatches through the SAME pipeline.Handler the REST/GraphQL surfaces
// use, and projects the result back via fromResult (the responseProjection
// seat). Failures travel as *connect.Error with the notification envelope
// in google.rpc details (see ErrorFromNotifications).
//
// The returned function has exactly the generated service interface's
// method signature, so the consumer's implementation is a one-line
// delegation:
//
//	type usersRPC struct{ createUser func(...) (...) }
//	func (s *usersRPC) CreateUser(ctx context.Context, req *connect.Request[pb.CreateUserRequest]) (*connect.Response[pb.CreateUserResponse], error) {
//	    return s.createUser(ctx, req)
//	}
//
// A toCommand error carrying domain notifications (domain.NewDomainError /
// any NotificationCarrier) is translated and mapped by Semantic; any other
// toCommand error is a wire-contract violation → SchemaViolationNotification
// (INVALID_ARGUMENT), mirroring the REST body-parse rejection.
func handleCommandWithBody[
	PB, RPB any,
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBody
	},
	TResult any,
](
	pipe *pipeline.Pipeline,
	toCommand func(*PB) (TCmdPtr, error),
	h pipeline.Handler[TCmdPtr, TResult],
	fromResult func(TResult) (*RPB, error),
	strictFields []string,
) func(context.Context, *connect.Request[PB]) (*connect.Response[RPB], error) {
	return func(ctx context.Context, req *connect.Request[PB]) (*connect.Response[RPB], error) {
		appCtx := AppContextFrom(ctx)
		if len(strictFields) > 0 {
			if pm, ok := any(req.Msg).(proto.Message); ok {
				if missing := missingFields(pm, strictFields); len(missing) > 0 {
					return nil, missingFieldsError[TResult](pipe, appCtx, missing)
				}
			}
		}
		cmd, err := toCommand(req.Msg)
		if err != nil {
			return nil, conversionError[TResult](pipe, appCtx, err)
		}
		result := pipeline.Dispatch(pipe, appCtx, cmd, h)
		return responseFromResult(result, fromResult)
	}
}

// handleQueryWithParams is the read-side sibling of
// web.QueryWithParams: same flow, queries.QueryWithParams constraint and
// a typed queries.PageOf result, no strict presence (queries have no
// FullBody contract). toQuery additionally answers the Result Go paths that
// were read ONLY to feed a masked computed field — the wrapper blanks them
// on each page item before projection, so a read_mask shapes the wire
// exactly as `?fields=` does on REST and the exports.
func handleQueryWithParams[
	PB, RPB any,
	TQ queries.QueryWithParams[TResult],
	TResult any,
](
	pipe *pipeline.Pipeline,
	toQuery func(*PB) (TQ, []string, error),
	h pipeline.Handler[TQ, queries.PageOf[TResult]],
	fromResult func(queries.PageOf[TResult]) (*RPB, error),
) func(context.Context, *connect.Request[PB]) (*connect.Response[RPB], error) {
	return func(ctx context.Context, req *connect.Request[PB]) (*connect.Response[RPB], error) {
		appCtx := AppContextFrom(ctx)
		q, hidden, err := toQuery(req.Msg)
		if err != nil {
			return nil, conversionError[queries.PageOf[TResult]](pipe, appCtx, err)
		}
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		project := fromResult
		if len(hidden) > 0 {
			project = func(p queries.PageOf[TResult]) (*RPB, error) {
				for i := range p.Items {
					p.Items[i] = queryschema.BlankResultPaths(p.Items[i], hidden)
				}
				return fromResult(p)
			}
		}
		return responseFromResult(result, project)
	}
}

// responseFromResult is the connect sibling of web.RespondFromResult:
// success projects, failure carries the translated envelope, exception is
// an opaque INTERNAL. A projection error is a response-side bridge failure
// — a server-side contract bug, never the caller's fault — and surfaces as
// the same opaque INTERNAL.
func responseFromResult[T, RPB any](result pipeline.Result[T], fromResult func(T) (*RPB, error)) (*connect.Response[RPB], error) {
	switch {
	case result.IsSuccess():
		out, err := fromResult(result.Value())
		if err != nil {
			return nil, errInternal()
		}
		return connect.NewResponse(out), nil
	case result.IsFailure():
		return nil, ErrorFromNotifications(result.Notifications())
	default:
		return nil, errInternal()
	}
}

// missingFields returns the strict fields not present on the wire, in the
// declared order. An unknown field name counts as missing — the contract
// named a field the message cannot carry, which is a wiring bug surfaced
// loudly at the first strict call.
func missingFields(m proto.Message, fields []string) []string {
	var missing []string
	r := m.ProtoReflect()
	descs := r.Descriptor().Fields()
	for _, name := range fields {
		fd := descs.ByName(protoreflect.Name(name))
		if fd == nil || !r.Has(fd) {
			missing = append(missing, name)
		}
	}
	return missing
}

// missingFieldsError mirrors web.respondMissingFieldsAsSchema: one
// RequiredFieldNotification (Semantic=Schema) per missing field, translated
// through the pipeline, emitted as INVALID_ARGUMENT with BadRequest
// violations.
func missingFieldsError[T any](pipe *pipeline.Pipeline, appCtx *configuration.AppContext, missing []string) error {
	nctx := domain.NewNotificationContext("Schema")
	schema := domain.SemanticSchema
	for _, field := range missing {
		nctx.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    field,
			Notification: domain.RequiredFieldNotification{}.WithSemantic(schema),
		})
	}
	return failureError[T](pipe, appCtx, domain.NewDomainError([]*domain.NotificationContext{nctx}))
}

// conversionError classifies a toCommand/toQuery error: a
// NotificationCarrier keeps its own semantics; anything else is a
// wire-contract violation (SchemaViolationNotification), mirroring the REST
// body-parse rejection.
func conversionError[T any](pipe *pipeline.Pipeline, appCtx *configuration.AppContext, err error) error {
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		nctx := domain.NewNotificationContext("Schema")
		nctx.AddNotificationMessage(domain.NotificationMessage{
			Notification: domain.SchemaViolationNotification{},
			FieldValue:   fmt.Sprintf("%v", err),
		})
		err = domain.NewDomainError([]*domain.NotificationContext{nctx})
	}
	return failureError[T](pipe, appCtx, err)
}

// failureError funnels a notification-carrying error through pipeline.Run —
// the translation choke point every surface uses — and renders the failure.
func failureError[T any](pipe *pipeline.Pipeline, appCtx *configuration.AppContext, err error) error {
	result := pipeline.Run(pipe, appCtx, func() (T, error) {
		var zero T
		return zero, err
	})
	if result.IsFailure() {
		return ErrorFromNotifications(result.Notifications())
	}
	return errInternal()
}

// handleCommandWithBodyID is the update-style sibling (PUT/PATCH): body +
// id, mirroring web.CommandWithBodyID and graphql.MutationWithBodyID.
// The wrapper injects cmd.SetPathID(idFrom(msg)) AFTER toCommand — the
// pipeline.CommandWithBodyID seam every surface shares. idFrom is a typed
// extractor; pass the generated getter directly, e.g.
// (*usersv1.UpdateUserRequest).GetId.
func handleCommandWithBodyID[
	PB, RPB any,
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBodyID
	},
	TResult any,
](
	pipe *pipeline.Pipeline,
	idFrom func(*PB) string,
	toCommand func(*PB) (TCmdPtr, error),
	h pipeline.Handler[TCmdPtr, TResult],
	fromResult func(TResult) (*RPB, error),
	strictFields []string,
) func(context.Context, *connect.Request[PB]) (*connect.Response[RPB], error) {
	return func(ctx context.Context, req *connect.Request[PB]) (*connect.Response[RPB], error) {
		appCtx := AppContextFrom(ctx)
		if len(strictFields) > 0 {
			if pm, ok := any(req.Msg).(proto.Message); ok {
				if missing := missingFields(pm, strictFields); len(missing) > 0 {
					return nil, missingFieldsError[TResult](pipe, appCtx, missing)
				}
			}
		}
		cmd, err := toCommand(req.Msg)
		if err != nil {
			return nil, conversionError[TResult](pipe, appCtx, err)
		}
		cmd.SetPathID(idFrom(req.Msg))
		result := pipeline.Dispatch(pipe, appCtx, cmd, h)
		return responseFromResult(result, fromResult)
	}
}

// handleCommandByID is the archive/delete-style sibling: id only, NO mapper
// — the wrapper constructs the command and injects the id, mirroring
// web.CommandByID (`cmd := TCmd(new(T)); cmd.SetPathID(...)`) and
// graphql.MutationByID.
func handleCommandByID[
	PB, RPB any,
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandByID
	},
	TResult any,
](
	pipe *pipeline.Pipeline,
	idFrom func(*PB) string,
	h pipeline.Handler[TCmdPtr, TResult],
	fromResult func(TResult) (*RPB, error),
) func(context.Context, *connect.Request[PB]) (*connect.Response[RPB], error) {
	return func(ctx context.Context, req *connect.Request[PB]) (*connect.Response[RPB], error) {
		cmd := TCmdPtr(new(TCmd))
		cmd.SetPathID(idFrom(req.Msg))
		result := pipeline.Dispatch(pipe, AppContextFrom(ctx), cmd, h)
		return responseFromResult(result, fromResult)
	}
}

// handleQueryByID is the get-one sibling of web.QueryByID: the
// handler returns the typed Result (the application filled it from the view
// document and ran FromQueryResult) and fromResult projects it to the response
// message. The wrapper injects q.SetPathID(idFrom(msg)) after toQuery,
// symmetric with the command side; toQuery carries the rest of the message
// (e.g. include_archived).
func handleQueryByID[
	PB, RPB any,
	TQ queries.QueryByID[TResult],
	TResult any,
](
	pipe *pipeline.Pipeline,
	idFrom func(*PB) string,
	toQuery func(*PB) (TQ, error),
	h pipeline.Handler[TQ, TResult],
	fromResult func(TResult) (*RPB, error),
) func(context.Context, *connect.Request[PB]) (*connect.Response[RPB], error) {
	return func(ctx context.Context, req *connect.Request[PB]) (*connect.Response[RPB], error) {
		appCtx := AppContextFrom(ctx)
		q, err := toQuery(req.Msg)
		if err != nil {
			return nil, conversionError[TResult](pipe, appCtx, err)
		}
		q.SetPathID(idFrom(req.Msg))
		result := pipeline.Dispatch(pipe, appCtx, q, h)
		return responseFromResult(result, fromResult)
	}
}
