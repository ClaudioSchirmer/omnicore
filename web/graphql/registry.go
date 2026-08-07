package graphql

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// HasToParamsQuery is the contract for read Request DTOs — the same shape the
// REST wrapper consumes. Declared here (not imported from web) so web/graphql
// stays independent of web. A zero Request maps the parsed criteria into the
// Query; AppContext-derived overlays still layer on inside Query.ToCriteria.
type HasToParamsQuery[TQ queries.QueryWithParams] interface {
	ToQuery(criteria queries.ReadCriteria) TQ
}

// resolver runs one root field: GraphQL args → a wire-shaped value tree (or
// errors). The executor trims the tree by the field's selection set. The
// selection set + fragments are also passed IN so a read resolver can derive a
// projection from the requested fields (field-access enforcement + Mongo
// pushdown); write/introspection resolvers ignore them.
type resolver func(ctx *configuration.AppContext, args map[string]any, sel ast.SelectionSet, frags ast.FragmentDefinitionList) (any, []GraphQLError)

// Field is a registered GraphQL root field (query or mutation). The generic
// constructors (QueryWithParams, and the Mutation* variants) capture the typed handler into
// the SDL emitter + the resolver factory; the Registry binds the pipeline.
type Field struct {
	name               string
	isMutation         bool
	requiredPermission string // Layer-1 gate; empty = no permission required
	sdlLine            func(b *sdlBuilder) string
	makeResolve        func(pipe *pipeline.Pipeline) resolver
}

// FieldOption customizes a Field at registration. It is the GraphQL twin of
// openapi.MountOption — the per-field knobs a consumer attaches when wiring a
// Query / Mutation into the registry.
type FieldOption func(*Field)

// RequirePermission declares the permission the request's Identity must carry
// for this field to resolve — the GraphQL twin of openapi.RequirePermission,
// the Layer-1 declarative gate. The permission string is "resource:action"
// (wildcards are the IdP's to grant, never the caller's to declare). The gate
// is enforced in the resolver, behind the same master switch the REST gate
// uses (EnableAuthorization, set at boot from auth.authorization.enabled): when
// the switch is off the annotation is inert (incremental-rollout parity with
// REST). On a denied request the resolver emits the canonical
// MissingPermissionNotification (semantic Forbidden) in errors[].extensions —
// the same notification the REST permission gate returns as 403.
func RequirePermission(permission string) FieldOption {
	if permission == "" || !strings.Contains(permission, ":") {
		panic("graphql.RequirePermission: permission must be \"resource:action\"; got " + strconv.Quote(permission))
	}
	if strings.Contains(permission, "*") {
		panic("graphql.RequirePermission: wildcards on the caller side are not allowed; got " +
			strconv.Quote(permission) + ". The IdP grants wildcards; the field declares the exact action")
	}
	return func(f *Field) {
		if f.requiredPermission != "" {
			panic("graphql.RequirePermission: duplicate option on the same field (previous=" +
				strconv.Quote(f.requiredPermission) + ", new=" + strconv.Quote(permission) +
				"). Combine via a compound permission at the IdP, not via multiple RequirePermission options")
		}
		f.requiredPermission = permission
	}
}

// applyOptions folds the variadic FieldOptions into a constructed Field.
func applyOptions(f Field, opts []FieldOption) Field {
	for _, o := range opts {
		o(&f)
	}
	return f
}

// Registry is the "list of queries/mutations" a consumer attaches handlers to.
// New(pipe) → Register(QueryWithParams(...)) → mount the Handler. The schema is built
// lazily (and cached) on the first execution.
type Registry struct {
	pipe          *pipeline.Pipeline
	fields        []Field
	introspection bool
	authorization bool
	schema        *ast.Schema
	sdl           string
	resolvers     map[string]resolver
	buildErr      error
}

// New constructs a Registry bound to the dispatch pipeline.
func New(pipe *pipeline.Pipeline) *Registry {
	return &Registry{pipe: pipe, resolvers: map[string]resolver{}}
}

// Register attaches a field (from Query / Mutation) and invalidates any built
// schema so the next execution rebuilds. Returns the Registry for chaining.
func (r *Registry) Register(f Field) *Registry {
	r.fields = append(r.fields, f)
	r.schema = nil
	r.buildErr = nil
	return r
}

// EnableIntrospection toggles whether `__schema` / `__type` are answered.
// bootstrap sets it from Config.GraphQL.Introspection; off by default so an
// operator opts in (the playground's autocomplete needs it). Invalidates any
// built schema so the next execution re-binds the introspection resolvers.
func (r *Registry) EnableIntrospection(on bool) *Registry {
	r.introspection = on
	r.schema = nil
	r.buildErr = nil
	return r
}

// EnableAuthorization toggles the Layer-1 permission gate master switch the
// resolvers consult per request. bootstrap sets it from
// auth.authorization.enabled (mirroring fwweb.SetAuthorizationEnabled for the
// REST gate); off by default so RequirePermission annotations sit inert until
// the operator opts in — the same incremental-rollout posture as REST.
// Invalidates any built schema so the next execution re-binds the guarded
// resolvers.
func (r *Registry) EnableAuthorization(on bool) *Registry {
	r.authorization = on
	r.schema = nil
	r.buildErr = nil
	return r
}

// QueryWithParams registers a read handler as a root Query field returning a
// Relay connection. TReq is the read Request DTO, R the Response DTO (the connection
// node), TQ its Query. The argument set (where / first / after / last / before /
// orderBy / search / includeArchived) and the node/where/connection types are
// reflected from TReq + R — both are reflection-only (they appear in no
// parameter) and must be named; TQ is inferred from TReq's ToQuery + the
// handler, so a call passes just `QueryWithParams[TReq, R](...)`. TQ trails the type-param
// list precisely so it can be elided as the inferable suffix.
func QueryWithParams[TReq HasToParamsQuery[TQ], R any, TQ queries.QueryWithParams](
	name, entity string,
	h pipeline.Handler[TQ, queries.Page],
	opts ...FieldOption,
) Field {
	reqType := reflect.TypeOf((*TReq)(nil)).Elem()
	respType := reflect.TypeOf((*R)(nil)).Elem()
	plan := newCriteriaPlan(reqType, respType)
	return applyOptions(Field{
		name:    name,
		sdlLine: func(b *sdlBuilder) string { return b.queryFieldSDL(name, entity, reqType, respType) },
		makeResolve: func(pipe *pipeline.Pipeline) resolver {
			return func(ctx *configuration.AppContext, args map[string]any, sel ast.SelectionSet, frags ast.FragmentDefinitionList) (any, []GraphQLError) {
				crit, controls, gerr := plan.buildCriteria(args)
				if gerr != nil {
					return nil, []GraphQLError{*gerr}
				}
				// Only-total: a selection of just totalCount (no edges/pageInfo)
				// is the GraphQL expression of REST's ?onlyTotal=true — no
				// argument, the selection shape is the switch. The MODE activates
				// only when the endpoint's DTO opted in via `query:"onlyTotal"`
				// (the reader short-circuits to its count primitive); without the
				// opt-in the same selection stays valid and returns the same
				// totalCount through the un-optimized paged read — the total is
				// intrinsic to every list envelope on every surface.
				if onlyTotalSelected(sel, frags) && plan.reqSchema.Reserved[queryschema.KeyOnlyTotal] {
					active := true
					controls.OnlyTotal = &active
					crit.OnlyTotal = true
				}
				// The canonical control gateway — the same three checks REST and
				// gRPC run (DTO opt-in gate, directional rule, only-total conflict
				// matrix), rendered in this surface's idiom via schemaViolation.
				// The SDL already cut undeclared args (gqlparser rejects unknown
				// arguments before the resolver), so the gate arm is defense in
				// depth; direction + conflicts are the live checks.
				if violations := queryschema.ValidateControls(plan.reqSchema.Reserved, controls, graphqlNaturalControls); len(violations) > 0 {
					return nil, schemaViolation(pipe, ctx, violations[0].Field())
				}
				// Selection set → projection: an explicitly selected restricted
				// field trips ReadCriteria.Restrict's active-reference 403 (parity
				// with the REST ?fields= path), and Mongo projects only the
				// requested fields (pushdown). Empty node selection → nil → whole-doc.
				if proj := plan.projectionFromSelection(sel, frags); len(proj) > 0 {
					crit.Projection = proj
				} else if !crit.OnlyTotal && pageInfoOnlySelected(sel, frags) {
					// Pagination probe: pageInfo with no edges — the consumer wants
					// the window's boundaries, not its rows. Narrow the read to the
					// keyset essentials (ordering values + _id); the reader still
					// walks the window (edges cannot exist without it) but skips
					// materializing full documents.
					crit.Projection = keysOnlyProjection(crit.OrderBy)
				}
				var req TReq
				res := pipeline.Dispatch(pipe, ctx, req.ToQuery(crit), h)
				switch {
				case res.IsSuccess():
					return pageToConnection(res.Value(), respType), nil
				case res.IsFailure():
					return nil, fromNotifications(res.Notifications())
				default:
					return nil, internalError()
				}
			}
		},
	}, opts)
}

// guardPermission wraps a resolver with the Layer-1 permission check. Behind
// the EnableAuthorization master switch (off → pass-through, REST-parity
// incremental rollout): when on, the request's Identity must carry the declared
// permission, else the resolver short-circuits with the canonical
// MissingPermissionNotification (semantic Forbidden) instead of running the
// handler.
func (r *Registry) guardPermission(permission string, inner resolver) resolver {
	return func(ctx *configuration.AppContext, args map[string]any, sel ast.SelectionSet, frags ast.FragmentDefinitionList) (any, []GraphQLError) {
		if r.authorization {
			id := ctx.Identity()
			if id == nil || !id.HasPermission(permission) {
				return nil, r.missingPermission(ctx, permission)
			}
		}
		return inner(ctx, args, sel, frags)
	}
}

// missingPermission renders the 403-equivalent rejection through the same
// translation + DTO path a handler failure takes, so the GraphQL error carries
// the identical notification triple the REST gate emits (semantic Forbidden,
// notificationKey MissingPermissionNotification, field "permission"; the
// declared permission is the field value). Routed through pipeline.Run so the
// message is translated against the request language.
func (r *Registry) missingPermission(ctx *configuration.AppContext, permission string) []GraphQLError {
	res := pipeline.Run(r.pipe, ctx, func() (any, error) {
		nc := domain.NewNotificationContext("Authorization")
		nc.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    "permission",
			FieldValue:   permission,
			Notification: notifications.MissingPermissionNotification{},
		})
		return nil, domain.NewDomainError([]*domain.NotificationContext{nc})
	})
	return fromNotifications(res.Notifications())
}

// schemaViolation renders a SchemaViolationNotification (semantic Schema) the
// same way a handler failure surfaces — through pipeline.Run so the message is
// translated against the request language and carries the typed triple
// (notificationKey SchemaViolationNotification, field = the offending argument).
// Used for the only-total-vs-pagination conflict, REST parity with the
// onlyTotalConflicts 400. Package-level (the read resolver holds pipe, not the
// Registry).
func schemaViolation(pipe *pipeline.Pipeline, ctx *configuration.AppContext, field string) []GraphQLError {
	res := pipeline.Run(pipe, ctx, func() (any, error) {
		nc := domain.NewNotificationContext("Schema")
		nc.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    field,
			Notification: domain.SchemaViolationNotification{},
		})
		return nil, domain.NewDomainError([]*domain.NotificationContext{nc})
	})
	return fromNotifications(res.Notifications())
}

// SDL returns the generated schema document (building it if needed). Useful for
// snapshot tests and for serving the schema to tooling.
func (r *Registry) SDL() (string, error) {
	if err := r.build(); err != nil {
		return "", err
	}
	return r.sdl, nil
}

// build assembles the SDL from every registered field, loads + validates it via
// gqlparser, and binds the resolvers. Cached; Register invalidates it.
func (r *Registry) build() error {
	if r.schema != nil || r.buildErr != nil {
		return r.buildErr
	}
	b := newSDLBuilder()
	var queryLines, mutationLines []string
	resolvers := map[string]resolver{}
	for _, f := range r.fields {
		line := f.sdlLine(b)
		if f.isMutation {
			mutationLines = append(mutationLines, line)
		} else {
			queryLines = append(queryLines, line)
		}
		res := f.makeResolve(r.pipe)
		if f.requiredPermission != "" {
			res = r.guardPermission(f.requiredPermission, res)
		}
		resolvers[f.name] = res
	}
	// GraphQL requires a Query root type; supply a stub when only mutations are
	// registered so a mutation-only service still produces a valid schema.
	if len(queryLines) == 0 {
		queryLines = append(queryLines, "  _empty: Boolean")
	}
	roots := []string{"type Query {\n" + strings.Join(queryLines, "\n") + "\n}"}
	if len(mutationLines) > 0 {
		roots = append(roots, "type Mutation {\n"+strings.Join(mutationLines, "\n")+"\n}")
	}
	doc := b.document(roots...)
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: "omnicore.graphql", Input: doc})
	if err != nil {
		r.buildErr = err
		return err
	}
	r.schema = schema
	r.sdl = doc
	if r.introspection {
		for name, res := range r.introspectionResolvers() {
			resolvers[name] = res
		}
	}
	r.resolvers = resolvers
	return nil
}
