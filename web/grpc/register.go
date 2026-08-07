package grpc

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The attachment surface mirrors the REST Spec constructors exactly — the
// SAME ingredients (the Request DTO with its ToCommand/ToQuery, the
// Response DTO's FromResult or an AutoFromDoc projector), plus the
// generated procedure constant and the pb types as explicit type
// parameters (Go cannot infer them from a procedure string):
//
//	reg.Register(fwgrpc.CommandWithBody[usersv1.CreateUserRequest, usersv1.CreateUserResponse](
//	    usersv1connect.UsersServiceCreateUserProcedure,
//	    requests.InsertUserRequest{},
//	    requests.InsertUserResponse{}.FromResult,
//	    &handlers.SharedBaseInsertCommandHandler[…]{Repo: repo},
//	    fwgrpc.RequirePermission("users:write")))
//
// The framework crosses the pb ↔ DTO boundary mechanically (bind.go): the
// bridge plan compiles AT REGISTER TIME and a contract/DTO mismatch aborts
// boot — semantic transformation stays in the DTO seats, exactly like every
// other surface. This is the ONE attachment API: every constructor accepts
// any pipeline.Handler (auto or hand-written — the canonical/manual
// equivalence lives at the application-handler seam). A contract whose
// shape cannot mirror DTOs is a MountRaw procedure.
//
// NOTE: the pb TYPE PARAMETERS are the message VALUE types
// (usersv1.CreateUserRequest, not *…). The generated pointer type is what
// implements proto.Message; the constructors allocate as needed.

// Procedure is one packaged RPC — the gRPC sibling of graphql.Field.
// Produced only by the constructor family; consumed by Registry.Register.
type Procedure struct {
	procedure string
	build     func(r *Registry) http.Handler
}

// RequestDTO, HasToParamsQuery and HasToIDQuery are the DTO seats the
// constructors consume — the same contracts the REST wrappers demand, so
// one Request DTO serves both wires.
type RequestDTO[TCmd any] interface{ ToCommand() TCmd }

type HasToParamsQuery[TQ queries.QueryWithParams] interface {
	ToQuery(criteria queries.ReadCriteria) TQ
}

type HasToIDQuery[TQ queries.QueryByID] interface{ ToQuery() TQ }

// ProcedureOption configures one constructor call.
type ProcedureOption func(*procedureConfig)

type procedureConfig struct {
	strictFields []string
	permission   string
	aliases      map[string]string
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

// Alias declares one exceptional wire ↔ DTO pairing the normalized name
// match cannot see: wireField is the proto field name (snake_case),
// goField the DTO's Go field name (for filters: the DTO's wire or Go
// filter path). Everything else keeps matching by name.
func Alias(wireField, goField string) ProcedureOption {
	return func(cfg *procedureConfig) {
		if cfg.aliases == nil {
			cfg.aliases = map[string]string{}
		}
		cfg.aliases[wireField] = goField
	}
}

// Register materializes the procedure with this registry's pipeline and
// interceptor chain and serves it under its generated procedure constant.
// Bridge plans compile HERE — boot time — so a wire/DTO mismatch aborts
// before the listener accepts traffic. Mounting per-procedure is exactly
// what the generated New<Service>Handler does internally, so generated
// CLIENTS, the gRPC protocol and the reflection service all work
// unchanged. The fully-qualified service name is recorded once for
// reflection.
func (r *Registry) Register(p Procedure) *Registry {
	r.mountProcedure(p.procedure, p.build(r))
	return r
}

// pathIDField is the request field the ByID constructor family binds to
// SetPathID (via the typed idFrom getter) — exempt from DTO matching, the
// proto sibling of the REST `/:id` path segment.
const pathIDField = "id"

// CommandWithBody attaches a create-style command: the request message
// mirrors the SAME Request DTO the REST POST consumes (ToCommand seat),
// the response mirrors the FromResult DTO. Optional Strict enforces the
// FullBody contract.
func CommandWithBody[
	PB, RPB any,
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBody
	},
	TResult any,
	RD any,
](
	procedure string,
	sample TReq,
	fromResult func(TResult) RD,
	h pipeline.Handler[TCmdPtr, TResult],
	opts ...ProcedureOption,
) Procedure {
	_ = sample // anchors TReq for type inference; pbToDTO allocates per request
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		toCommand, project := commandBridges[PB, RPB, TReq, TCmdPtr, TResult, RD](
			"CommandWithBody "+procedure, fromResult, cfg, nil)
		fn := handleCommandWithBody(r.Pipeline(), toCommand, h, project, cfg.strictFields)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// CommandWithBodyID attaches an update-style command (body + id); idFrom is
// the generated getter, e.g. (*usersv1.UpdateUserRequest).GetId — the
// wrapper injects cmd.SetPathID(idFrom(msg)) after ToCommand, the
// pipeline.CommandWithBodyID seam every surface shares. The request's `id`
// field is the path id and is exempt from DTO matching.
func CommandWithBodyID[
	PB, RPB any,
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBodyID
	},
	TResult any,
	RD any,
](
	procedure string,
	idFrom func(*PB) string,
	sample TReq,
	fromResult func(TResult) RD,
	h pipeline.Handler[TCmdPtr, TResult],
	opts ...ProcedureOption,
) Procedure {
	_ = sample
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		toCommand, project := commandBridges[PB, RPB, TReq, TCmdPtr, TResult, RD](
			"CommandWithBodyID "+procedure, fromResult, cfg, map[string]bool{pathIDField: true})
		fn := handleCommandWithBodyID(r.Pipeline(), idFrom, toCommand, h, project, cfg.strictFields)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// CommandByID attaches an archive/delete-style command: id only, NO DTO —
// the wrapper constructs the command and injects the id, mirroring
// web.CommandByID. The request carries only the `id` field and the
// response is EMPTY (the 204 sibling) — both enforced at boot; a byID
// contract that answers with data is a CommandWithBody-family shape.
func CommandByID[
	PB, RPB any,
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandByID
	},
	TResult any,
](
	procedure string,
	idFrom func(*PB) string,
	h pipeline.Handler[TCmdPtr, TResult],
	opts ...ProcedureOption,
) Procedure {
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		ctxName := "CommandByID " + procedure
		reqMD := descriptorOf[PB](ctxName, "request")
		for i := 0; i < reqMD.Fields().Len(); i++ {
			if name := string(reqMD.Fields().Get(i).Name()); name != pathIDField {
				bootFail("%s: request field %q is unreachable — a byID command carries only %q; body-carrying commands are the CommandWithBody family",
					ctxName, name, pathIDField)
			}
		}
		if respMD := descriptorOf[RPB](ctxName, "response"); respMD.Fields().Len() > 0 {
			bootFail("%s: response carries fields — a byID command answers with an empty message (the 204 sibling); results with data are the CommandWithBody family",
				ctxName)
		}
		project := func(TResult) (*RPB, error) { return new(RPB), nil }
		fn := handleCommandByID[PB, RPB, TCmd, TCmdPtr](r.Pipeline(), idFrom, h, project)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// QueryWithParams attaches a list query. The request composes the shared
// omnicore.v1 components (discovered by type at boot); filters bind to the
// SAME Request DTO the REST list consumes and inherit its `filter:` tag
// operator allowlist; read_mask/sort resolve against the projector's
// Response DTO — then ToQuery(criteria) receives the INPUT criteria,
// exactly what the REST parser hands it. The response composes one
// repeated items message + omnicore.v1.PaginationInfo, filled from the same
// projector seat REST uses (fwresponses.AutoFromDoc or a hand-written
// FromDoc).
func QueryWithParams[
	PB, RPB any,
	TReq HasToParamsQuery[TQ],
	TQ queries.QueryWithParams,
	R any,
](
	procedure string,
	sample TReq,
	projector func(map[string]any) R,
	h pipeline.Handler[TQ, queries.Page],
	opts ...ProcedureOption,
) Procedure {
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		ctxName := "QueryWithParams " + procedure
		respDTO := reflect.TypeOf((*R)(nil)).Elem()
		env, err := compileListEnvelope(ctxName, descriptorOf[RPB](ctxName, "response"), respDTO, cfg.aliases)
		if err != nil {
			bootFail("%v", err)
		}
		plan, err := compileQueryPlan(ctxName, descriptorOf[PB](ctxName, "request"),
			reflect.TypeOf(sample), env.items.Message(), respDTO, cfg.aliases)
		if err != nil {
			bootFail("%v", err)
		}
		toQuery := func(msg *PB) (TQ, error) {
			crit, err := plan.buildCriteria(any(msg).(proto.Message).ProtoReflect())
			if err != nil {
				var zero TQ
				return zero, err
			}
			return sample.ToQuery(crit), nil
		}
		fromPage := func(p queries.Page) (*RPB, error) {
			return buildListResponse[RPB](env, projector, p)
		}
		fn := handleQueryWithParams(r.Pipeline(), toQuery, h, fromPage)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// QueryByID attaches a get-one query: the request mirrors the REST by-id
// DTO (id exempt — it rides SetPathID via idFrom), the response mirrors
// the projector's Response DTO directly (no envelope).
func QueryByID[
	PB, RPB any,
	TReq HasToIDQuery[TQ],
	TQ queries.QueryByID,
	R any,
](
	procedure string,
	idFrom func(*PB) string,
	sample TReq,
	projector func(map[string]any) R,
	h pipeline.Handler[TQ, map[string]any],
	opts ...ProcedureOption,
) Procedure {
	_ = sample
	cfg := resolveProcedureOptions(opts)
	return Procedure{procedure: procedure, build: func(r *Registry) http.Handler {
		ctxName := "QueryByID " + procedure
		reqPlan, err := compileBindPlan(ctxName, "request", descriptorOf[PB](ctxName, "request"),
			reflect.TypeOf(sample), map[string]bool{pathIDField: true}, cfg.aliases)
		if err != nil {
			bootFail("%v", err)
		}
		respPlan, err := compileBindPlan(ctxName, "response", descriptorOf[RPB](ctxName, "response"),
			reflect.TypeOf((*R)(nil)).Elem(), nil, cfg.aliases)
		if err != nil {
			bootFail("%v", err)
		}
		toQuery := func(msg *PB) (TQ, error) {
			req, err := pbToDTO[TReq](reqPlan, any(msg).(proto.Message))
			if err != nil {
				var zero TQ
				return zero, err
			}
			return req.ToQuery(), nil
		}
		fromDoc := func(doc map[string]any) (*RPB, error) {
			return dtoToPB[RPB](respPlan, projector(doc))
		}
		fn := handleQueryByID(r.Pipeline(), idFrom, toQuery, h, fromDoc)
		return connect.NewUnaryHandler(procedure, guarded(r, cfg.permission, fn), r.HandlerOptions()...)
	}}
}

// commandBridges compiles the two bridge plans a body-carrying command
// needs and returns the toCommand / project closures over them.
func commandBridges[
	PB, RPB any,
	TReq RequestDTO[TCmdPtr],
	TCmdPtr any,
	TResult any,
	RD any,
](
	ctxName string,
	fromResult func(TResult) RD,
	cfg procedureConfig,
	exempt map[string]bool,
) (func(*PB) (TCmdPtr, error), func(TResult) (*RPB, error)) {
	var sample TReq
	reqPlan, err := compileBindPlan(ctxName, "request", descriptorOf[PB](ctxName, "request"),
		reflect.TypeOf(sample), exempt, cfg.aliases)
	if err != nil {
		bootFail("%v", err)
	}
	respPlan, err := compileBindPlan(ctxName, "response", descriptorOf[RPB](ctxName, "response"),
		reflect.TypeOf((*RD)(nil)).Elem(), nil, cfg.aliases)
	if err != nil {
		bootFail("%v", err)
	}
	toCommand := func(msg *PB) (TCmdPtr, error) {
		req, err := pbToDTO[TReq](reqPlan, any(msg).(proto.Message))
		if err != nil {
			var zero TCmdPtr
			return zero, err
		}
		return req.ToCommand(), nil
	}
	project := func(res TResult) (*RPB, error) {
		return dtoToPB[RPB](respPlan, fromResult(res))
	}
	return toCommand, project
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
