package grpc

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The attachment surface mirrors the GraphQL idiom exactly — a constructor
// family producing a Procedure value, registered on the Registry:
//
//	reg.Register(fwgrpc.CommandWithBody(usersv1connect.UsersServiceCreateUserProcedure,
//	    requests.CreateUserPB{}.ToCommand,
//	    requests.CreateUserPB{}.FromResult,
//	    &handlers.SharedBaseInsertCommandHandler[…]{Repo: repo},
//	    fwgrpc.RequirePermission("users:write")))
//
// This is the ONE attachment API: every constructor accepts any
// pipeline.Handler (auto or hand-written — the canonical/manual equivalence
// lives at the application-handler seam, exactly like REST and GraphQL).
// Constructors are generic top-level functions because Go methods cannot be
// generic; the Procedure defers materialization so Register can inject the
// registry's pipeline and interceptor chain.

// Procedure is one packaged RPC — the gRPC sibling of graphql.Field.
// Produced only by the constructor family; consumed by Registry.Register.
type Procedure struct {
	procedure string
	build     func(r *Registry) http.Handler
}

// ProcedureOption configures one constructor call.
type ProcedureOption func(*procedureConfig)

type procedureConfig struct {
	strictFields []string
	permission   string
}

func resolveProcedureOptions(opts []ProcedureOption) procedureConfig {
	var cfg procedureConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// Strict is the FullBody sibling for the protobuf wire: every named field
// must be PRESENT on the request message (proto3 `optional` presence,
// checked via protoreflect) — a missing one rejects with the same
// RequiredFieldNotification/SemanticSchema envelope the REST strict-body
// check emits. Declare the contract's command fields `optional` in the
// .proto or the check cannot distinguish absent from zero-value.
func Strict(fields ...string) ProcedureOption {
	return func(cfg *procedureConfig) { cfg.strictFields = fields }
}

// RequirePermission declares the Layer-1 permission gate for the procedure —
// the gRPC twin of fwopenapi.RequirePermission and
// fwgraphql.RequirePermission: "resource:action", enforced against
// Identity.HasPermission when the registry's authorization switch is on
// (auth.authorization.enabled), inert otherwise so services can annotate
// ahead of the operator flip. Rejection is PERMISSION_DENIED carrying the
// canonical MissingPermissionNotification envelope (field "permission").
func RequirePermission(permission string) ProcedureOption {
	return func(cfg *procedureConfig) { cfg.permission = permission }
}

// Register materializes the procedure with this registry's pipeline and
// interceptor chain and serves it under its generated procedure constant.
// Mounting per-procedure is exactly what the generated New<Service>Handler
// does internally, so generated CLIENTS, the gRPC protocol and the
// reflection service all work unchanged. The fully-qualified service name
// is recorded once for reflection.
func (r *Registry) Register(p Procedure) *Registry {
	r.mountProcedure(p.procedure, p.build(r))
	return r
}

// CommandWithBody attaches a create-style command (full input on the
// request message; optional Strict enforces the FullBody contract).
func CommandWithBody[
	PB, RPB any,
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.Command
	},
	TResult any,
](
	procedure string,
	toCommand func(*PB) (TCmdPtr, error),
	fromResult func(TResult) *RPB,
	h pipeline.Handler[TCmdPtr, TResult],
	opts ...ProcedureOption,
) Procedure {
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		fn := handleCommandWithBody(r.Pipeline(), toCommand, h, fromResult, cfg.strictFields)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// CommandWithBodyID attaches an update-style command (body + id); idFrom is
// the generated getter, e.g. (*usersv1.UpdateUserRequest).GetId — the
// wrapper injects cmd.SetPathID(idFrom(msg)) after toCommand, the
// pipeline.CommandWithID seam every surface shares.
func CommandWithBodyID[
	PB, RPB any,
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithID
	},
	TResult any,
](
	procedure string,
	idFrom func(*PB) string,
	toCommand func(*PB) (TCmdPtr, error),
	fromResult func(TResult) *RPB,
	h pipeline.Handler[TCmdPtr, TResult],
	opts ...ProcedureOption,
) Procedure {
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		fn := handleCommandWithBodyID(r.Pipeline(), idFrom, toCommand, h, fromResult, cfg.strictFields)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// CommandByID attaches an archive/delete-style command: id only, NO mapper
// — the wrapper constructs the command and injects the id, mirroring
// web.HandleCommandByID and graphql.MutationByID.
func CommandByID[
	PB, RPB any,
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithID
	},
	TResult any,
](
	procedure string,
	idFrom func(*PB) string,
	fromResult func(TResult) *RPB,
	h pipeline.Handler[TCmdPtr, TResult],
	opts ...ProcedureOption,
) Procedure {
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		fn := handleCommandByID[PB, RPB, TCmd, TCmdPtr](r.Pipeline(), idFrom, h, fromResult)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// QueryWithParams attaches a list query (criteria via the shared
// omnicore.v1 components, converted in the binding's ToQuery).
func QueryWithParams[
	PB, RPB any,
	TQ any,
	TQPtr interface {
		*TQ
		pipeline.Query
	},
	TResult any,
](
	procedure string,
	toQuery func(*PB) (TQPtr, error),
	fromResult func(TResult) *RPB,
	h pipeline.Handler[TQPtr, TResult],
	opts ...ProcedureOption,
) Procedure {
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		fn := handleQueryWithParams(r.Pipeline(), toQuery, h, fromResult)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// QueryByID attaches a get-one query: the handler returns the view
// document; fromDoc projects it.
func QueryByID[
	PB, RPB any,
	TQ any,
	TQPtr interface {
		*TQ
		queries.FindByIDQuery
	},
](
	procedure string,
	idFrom func(*PB) string,
	toQuery func(*PB) (TQPtr, error),
	fromDoc func(map[string]any) *RPB,
	h pipeline.Handler[TQPtr, map[string]any],
	opts ...ProcedureOption,
) Procedure {
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		fn := handleQueryByID(r.Pipeline(), idFrom, toQuery, h, fromDoc)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// guarded wraps the typed RPC func with the Layer-1 permission gate — the
// gRPC twin of web.PermissionGate: master switch off or no permission
// declared → passthrough; identity missing the permission →
// PERMISSION_DENIED with the canonical MissingPermissionNotification
// envelope, translated per the request's language.
func guarded[PB, RPB any](
	r *Registry,
	permission string,
	fn func(context.Context, *connect.Request[PB]) (*connect.Response[RPB], error),
) func(context.Context, *connect.Request[PB]) (*connect.Response[RPB], error) {
	if permission == "" {
		return fn
	}
	return func(ctx context.Context, req *connect.Request[PB]) (*connect.Response[RPB], error) {
		if err := r.checkPermission(ctx, permission); err != nil {
			return nil, err
		}
		return fn(ctx, req)
	}
}

// checkPermission enforces one procedure's declared permission against the
// request Identity. Non-generic so the envelope construction lives once.
func (r *Registry) checkPermission(ctx context.Context, permission string) *connect.Error {
	if !r.authzEnabled {
		return nil
	}
	appCtx := AppContextFrom(ctx)
	id := appCtx.Identity()
	if r.posture != PostureInherit {
		// Anonymous call on the internal/mtls plane: the gates PASS — the
		// posture IS the trust declaration, and what an internal chain may
		// do is the flow designer's responsibility (maintainer decision,
		// tasks/grpc.md Phase 4). The mTLS certificate identity is
		// ATTRIBUTION (it names the calling service for the audit trail),
		// not an authorization subject — it passes the same way. Only a
		// forwarded USER (valid or stale-authentic bearer) is evaluated.
		if id == nil {
			return nil
		}
		if v, ok := id.Claims[attributionStaleClaim]; ok && v == attributionMTLSValue {
			return nil
		}
	}
	if id != nil && id.HasPermission(permission) {
		return nil
	}
	return r.missingPermission(appCtx, permission)
}

func (r *Registry) missingPermission(appCtx *configuration.AppContext, permission string) *connect.Error {
	nctx := domain.NewNotificationContext("Authorization")
	nctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    "permission",
		FieldValue:   permission,
		Notification: notifications.MissingPermissionNotification{},
	})
	dtos := notifications.ToContextDTOs(r.pipe.Translator(), appCtx.Language(),
		[]*domain.NotificationContext{nctx})
	return ErrorFromNotifications(dtos)
}

// serviceOf extracts "users.v1.UsersService" from
// "/users.v1.UsersService/CreateUser".
func serviceOf(procedure string) string {
	trimmed := strings.TrimPrefix(procedure, "/")
	if i := strings.IndexByte(trimmed, '/'); i > 0 {
		return trimmed[:i]
	}
	return ""
}
