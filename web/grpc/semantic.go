// Package grpc is the framework's gRPC transport surface, served with
// Connect (connectrpc.com): one net/http endpoint speaking the gRPC,
// gRPC-Web and Connect protocols. It is the fourth consumer of the
// application-layer handlers — the same pipeline.Handler REST, GraphQL and
// the tabular export dispatch to — re-targeted to protobuf messages via the
// Command/Query wrappers. Registration follows the GraphQL precedent: a feature
// opts into the surface by implementing bootstrap.GRPCFeature (MountGRPC), the
// framework builds the shared Registry (Deps.GRPCRegistry) and serves it on a
// dedicated listener.
package grpc

import (
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// errorInfoDomain is the google.rpc.ErrorInfo.domain stamped on every
// notification detail emitted by this surface.
const errorInfoDomain = "omnicore"

// semanticToCode maps the transport-agnostic NotificationSemantic to its
// canonical gRPC code — the sibling of web.semanticToStatus (HTTP) built
// from the same enum, per the design in tasks/grpc.md.
var semanticToCode = map[domain.NotificationSemantic]connect.Code{
	domain.SemanticValidation:           connect.CodeInvalidArgument,    // 422 sibling
	domain.SemanticSchema:               connect.CodeInvalidArgument,    // 400 sibling (ErrorInfo.reason disambiguates)
	domain.SemanticNotFound:             connect.CodeNotFound,           // 404
	domain.SemanticConflict:             connect.CodeAlreadyExists,      // 409, duplicate flavor
	domain.SemanticStateConflict:        connect.CodeFailedPrecondition, // 409, wrong-state flavor
	domain.SemanticForbidden:            connect.CodePermissionDenied,   // 403
	domain.SemanticUnauthorized:         connect.CodeUnauthenticated,    // 401
	domain.SemanticUnavailable:          connect.CodeUnavailable,        // 503
	domain.SemanticInternal:             connect.CodeInternal,           // 500
	domain.SemanticMethodNotAllowed:     connect.CodeUnimplemented,      // 405
	domain.SemanticPayloadTooLarge:      connect.CodeResourceExhausted,  // 413
	domain.SemanticGatewayTimeout:       connect.CodeDeadlineExceeded,   // 504
	domain.SemanticRequestTimeout:       connect.CodeDeadlineExceeded,   // 408 (HTTP-only in practice; mapped for enum completeness)
	domain.SemanticGone:                 connect.CodeNotFound,           // 410, permanently-removed flavor of absence
	domain.SemanticPreconditionFailed:   connect.CodeFailedPrecondition, // 412 (the conditional-header flavor; ErrorInfo.reason disambiguates from 409)
	domain.SemanticUnsupportedMediaType: connect.CodeInvalidArgument,    // 415 sibling (wire contract; ErrorInfo.reason disambiguates from 400)
	domain.SemanticTooManyRequests:      connect.CodeResourceExhausted,  // 429
	domain.SemanticNotImplemented:       connect.CodeUnimplemented,      // 501 (ErrorInfo.reason disambiguates from the 405 flavor)
	domain.SemanticBadGateway:           connect.CodeUnavailable,        // 502 (ErrorInfo.reason disambiguates from the 503 flavor)
}

// codeFromNotifications mirrors web.statusFromNotifications: the first
// message whose Semantic is not Validation wins (a mixed-bag failure
// surfaces the more specific transport semantic); all-validation falls back
// to INVALID_ARGUMENT (the 422 sibling).
func codeFromNotifications(dtos []notifications.ContextDTO) connect.Code {
	for _, ctx := range dtos {
		for _, m := range ctx.Messages {
			if m.Semantic != domain.SemanticValidation {
				if code, ok := semanticToCode[m.Semantic]; ok {
					return code
				}
			}
		}
	}
	return connect.CodeInvalidArgument
}

// ErrorFromNotifications converts the pipeline's translated notification
// DTOs into a *connect.Error carrying the full envelope in google.rpc
// details: one ErrorInfo per message (reason = NotificationKey, metadata
// carries EVERY slot the REST ErrorMessage carries — context, semantic,
// message, field, fieldLabel, value, funcName; empty slots elided like
// REST's omitempty) plus a single BadRequest aggregating the field-scoped
// messages — so a gRPC/Connect consumer receives exactly what the REST
// envelope carries, in the protobuf-native shape.
func ErrorFromNotifications(dtos []notifications.ContextDTO) *connect.Error {
	msg := "request rejected"
	if len(dtos) > 0 && len(dtos[0].Messages) > 0 {
		msg = dtos[0].Messages[0].Message
	}
	cerr := connect.NewError(codeFromNotifications(dtos), errors.New(msg))
	var violations []*errdetails.BadRequest_FieldViolation
	for _, ctx := range dtos {
		for _, m := range ctx.Messages {
			if m.NotificationKey != "" {
				metadata := map[string]string{"context": ctx.Context, "semantic": m.Semantic.String()}
				if m.Message != "" {
					metadata["message"] = m.Message
				}
				if m.FieldName != "" {
					metadata["field"] = m.FieldName
				}
				if m.FieldLabel != "" {
					metadata["fieldLabel"] = m.FieldLabel
				}
				if m.FieldValue != "" {
					metadata["value"] = m.FieldValue
				}
				if m.FuncName != "" {
					metadata["funcName"] = m.FuncName
				}
				if d, err := connect.NewErrorDetail(&errdetails.ErrorInfo{
					Reason:   m.NotificationKey,
					Domain:   errorInfoDomain,
					Metadata: metadata,
				}); err == nil {
					cerr.AddDetail(d)
				}
			}
			if m.FieldName != "" {
				violations = append(violations, &errdetails.BadRequest_FieldViolation{
					Field:       m.FieldName,
					Description: m.Message,
				})
			}
		}
	}
	if len(violations) > 0 {
		if d, err := connect.NewErrorDetail(&errdetails.BadRequest{FieldViolations: violations}); err == nil {
			cerr.AddDetail(d)
		}
	}
	return cerr
}

// errInternal is the exception-state response: like the REST surface's
// RespondWithInternalServerError, it never leaks the underlying error.
func errInternal() *connect.Error {
	return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
}
