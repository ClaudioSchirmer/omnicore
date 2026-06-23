package graphql

import (
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// HasToParamsQuery is the contract for read Request DTOs — the same shape the
// REST wrapper consumes. Declared here (not imported from web) so web/graphql
// stays independent of web. A zero Request maps the parsed criteria into the
// Query; AppContext-derived overlays still layer on inside Query.ToCriteria.
type HasToParamsQuery[TQ queries.FindByParamsQuery] interface {
	ToQuery(criteria queries.ReadCriteria) TQ
}

// resolver runs one root field: GraphQL args → a wire-shaped value tree (or
// errors). The executor trims the tree by the field's selection set.
type resolver func(ctx *configuration.AppContext, args map[string]any) (any, []GraphQLError)

// Field is a registered GraphQL root field (query or mutation). The generic
// constructors (Query, and the future Mutation) capture the typed handler into
// the SDL emitter + the resolver factory; the Registry binds the pipeline.
type Field struct {
	name        string
	isMutation  bool
	sdlLine     func(b *sdlBuilder) string
	makeResolve func(pipe *pipeline.Pipeline) resolver
}

// Registry is the "list of queries/mutations" a consumer attaches handlers to.
// New(pipe) → Register(Query(...)) → mount the Handler. The schema is built
// lazily (and cached) on the first execution.
type Registry struct {
	pipe      *pipeline.Pipeline
	fields    []Field
	schema    *ast.Schema
	sdl       string
	resolvers map[string]resolver
	buildErr  error
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

// Query registers a read handler as a root Query field returning a Relay
// connection. TReq is the read Request DTO, TQ its Query, R the Response DTO
// (the connection node). The argument set (where / first / after / last /
// before / orderBy / search / includeArchived) and the node/where/connection
// types are reflected from TReq + R.
func Query[TReq HasToParamsQuery[TQ], TQ queries.FindByParamsQuery, R any](
	name, entity string,
	h pipeline.Handler[TQ, queries.Page],
) Field {
	reqType := reflect.TypeOf((*TReq)(nil)).Elem()
	respType := reflect.TypeOf((*R)(nil)).Elem()
	plan := newCriteriaPlan(reqType, respType)
	return Field{
		name:    name,
		sdlLine: func(b *sdlBuilder) string { return b.queryFieldSDL(name, entity, reqType, respType) },
		makeResolve: func(pipe *pipeline.Pipeline) resolver {
			return func(ctx *configuration.AppContext, args map[string]any) (any, []GraphQLError) {
				crit, gerr := plan.buildCriteria(args)
				if gerr != nil {
					return nil, []GraphQLError{*gerr}
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
	}
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
		resolvers[f.name] = f.makeResolve(r.pipe)
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
	r.resolvers = resolvers
	return nil
}
