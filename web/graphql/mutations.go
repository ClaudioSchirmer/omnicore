package graphql

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/vektah/gqlparser/v2/ast"
)

// CommandRequest is the write Request DTO contract — the body-only mapper to a
// Command. Declared here (not imported from web) so web/graphql stays
// independent of web; identical to web.RequestDTO.
type CommandRequest[TCmd any] interface {
	ToCommand() TCmd
}

// MutationWithBody registers an insert-style command handler (body, no path id)
// as a root Mutation field `<name>(input: <Name>Input!): <Name>Payload!` —
// `createUser` yields `CreateUserInput`/`CreateUserPayload`, the Relay/GitHub
// mutation naming convention: the schema type names derive from the REGISTERED
// FIELD NAME, never from the Go DTO names (which carry REST vocabulary like
// Insert/Request/Response that must not leak into the SDL). Nested input
// objects reached while walking the Request still derive from their Go type
// names via inputName. TReq is the write Request DTO, TCmdPtr its Command
// pointer, TResult the handler result, TResp the Response DTO (the mutation
// output — its fields become the payload's). The input object is reflected
// from TReq (json tags; required when the handler embeds pipeline.FullBody,
// else when the field is non-pointer without ,omitempty — mirroring the REST
// rule).
func MutationWithBody[
	TReq CommandRequest[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBody
	},
	TResult any,
	TResp any,
](name string, project func(TResult) TResp, h pipeline.Handler[TCmdPtr, TResult], opts ...FieldOption) Field {
	mustValidName("MutationWithBody", "name", name)
	reqType := reflect.TypeOf((*TReq)(nil)).Elem()
	respType := reflect.TypeOf((*TResp)(nil)).Elem()
	strict := isFullBody(h)
	return applyOptions(Field{
		name:       name,
		isMutation: true,
		sdlLine: func(b *sdlBuilder) string {
			in := b.inputObject(rootTypeName(name, "Input"), reqType, strict)
			out := b.objectTypeAs(rootTypeName(name, "Payload"), respType)
			return "  " + name + "(input: " + in + "!): " + out + "!"
		},
		makeResolve: func(pipe *pipeline.Pipeline) resolver {
			return func(ctx *configuration.AppContext, args map[string]any, _ ast.SelectionSet, _ ast.FragmentDefinitionList) (any, []GraphQLError) {
				req, gerr := decodeInput[TReq](args)
				if gerr != nil {
					return nil, []GraphQLError{*gerr}
				}
				res := pipeline.Dispatch(pipe, ctx, req.ToCommand(), h)
				return mutationOutput(res, project)
			}
		},
	}, opts)
}

// MutationWithBodyID registers an update/patch-style command handler (body + path
// id) as `<name>(id: ID!, input: <Name>Input!): <Name>Payload!` — type names
// derived from the registered field name exactly like MutationWithBody. The id
// arg is injected via SetPathID after ToCommand, mirroring CommandWithBodyID.
func MutationWithBodyID[
	TReq CommandRequest[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBodyID
	},
	TResult any,
	TResp any,
](name string, project func(TResult) TResp, h pipeline.Handler[TCmdPtr, TResult], opts ...FieldOption) Field {
	mustValidName("MutationWithBodyID", "name", name)
	reqType := reflect.TypeOf((*TReq)(nil)).Elem()
	respType := reflect.TypeOf((*TResp)(nil)).Elem()
	strict := isFullBody(h)
	return applyOptions(Field{
		name:       name,
		isMutation: true,
		sdlLine: func(b *sdlBuilder) string {
			in := b.inputObject(rootTypeName(name, "Input"), reqType, strict)
			out := b.objectTypeAs(rootTypeName(name, "Payload"), respType)
			return "  " + name + "(id: ID!, input: " + in + "!): " + out + "!"
		},
		makeResolve: func(pipe *pipeline.Pipeline) resolver {
			return func(ctx *configuration.AppContext, args map[string]any, _ ast.SelectionSet, _ ast.FragmentDefinitionList) (any, []GraphQLError) {
				req, gerr := decodeInput[TReq](args)
				if gerr != nil {
					return nil, []GraphQLError{*gerr}
				}
				rawID := asString(args["id"])
				if queryschema.IsMalformedPathID(rawID) {
					return nil, renderViolation(pipe, ctx, queryschema.MalformedPathID(queryschema.KeyPathID, rawID))
				}
				cmd := req.ToCommand()
				cmd.SetPathID(rawID)
				res := pipeline.Dispatch(pipe, ctx, cmd, h)
				return mutationOutput(res, project)
			}
		},
	}, opts)
}

// MutationByID registers a bodyless command handler (archive / unarchive /
// delete) as `<name>(id: ID!): <Name>Payload!` — the payload type name derives
// from the registered field name exactly like the body forms (`archiveUser` →
// `ArchiveUserPayload`), the Relay/GitHub per-mutation-payload convention; the
// payload body is the fixed bodyless shape `{ success: Boolean!, id: ID }`.
// There is no input; the command is allocated, given the path id, and
// dispatched — mirroring CommandByID. On success the field returns
// { success: true, id }.
func MutationByID[
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandByID
	},
	TResult any,
](name string, h pipeline.Handler[TCmdPtr, TResult], opts ...FieldOption) Field {
	mustValidName("MutationByID", "name", name)
	return applyOptions(Field{
		name:       name,
		isMutation: true,
		sdlLine: func(b *sdlBuilder) string {
			out := b.bodylessPayload(rootTypeName(name, "Payload"))
			return "  " + name + "(id: ID!): " + out + "!"
		},
		makeResolve: func(pipe *pipeline.Pipeline) resolver {
			return func(ctx *configuration.AppContext, args map[string]any, _ ast.SelectionSet, _ ast.FragmentDefinitionList) (any, []GraphQLError) {
				id := asString(args["id"])
				if queryschema.IsMalformedPathID(id) {
					return nil, renderViolation(pipe, ctx, queryschema.MalformedPathID(queryschema.KeyPathID, id))
				}
				cmd := TCmdPtr(new(TCmd))
				cmd.SetPathID(id)
				res := pipeline.Dispatch(pipe, ctx, cmd, h)
				switch {
				case res.IsSuccess():
					return map[string]any{"success": true, "id": id}, nil
				case res.IsFailure():
					return nil, fromNotifications(res.Notifications())
				default:
					return nil, internalError()
				}
			}
		},
	}, opts)
}

// decodeInput materializes a write Request DTO from the GraphQL `input`
// argument by round-tripping through JSON, so the Request's json tags govern
// the mapping exactly as the REST body bind does.
func decodeInput[TReq any](args map[string]any) (TReq, *GraphQLError) {
	var req TReq
	input, ok := args["input"].(map[string]any)
	if !ok {
		return req, nil // no input provided; zero Request
	}
	data, err := json.Marshal(input)
	if err != nil {
		return req, errf("input: %v", err)
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, errf("input: %v", err)
	}
	return req, nil
}

// mutationOutput maps a command Result to the mutation field's value: the
// projected Response (reshaped to wire) on success, notification errors on
// failure, an opaque internal error on exception.
func mutationOutput[TResult any, TResp any](res pipeline.Result[TResult], project func(TResult) TResp) (any, []GraphQLError) {
	switch {
	case res.IsSuccess():
		return structToWire(project(res.Value())), nil
	case res.IsFailure():
		return nil, fromNotifications(res.Notifications())
	default:
		return nil, internalError()
	}
}

// structToWire reshapes a typed Response value into a wire-keyed map by
// round-tripping through JSON (the Response's json tags define the wire
// shape), so the executor's selection trim is a pure key pick.
func structToWire(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func isFullBody(h any) bool {
	_, ok := h.(pipeline.FullBodyEnforcer)
	return ok
}

// rootTypeName derives a mutation's schema type name from its registered
// field name: `createUser` + "Input" → `CreateUserInput`, + "Payload" →
// `CreateUserPayload`. The field name — not the Go DTO name — is the naming
// authority for the root input/payload pair, so REST/Go vocabulary (Insert,
// Request, Response) never leaks into the SDL.
func rootTypeName(field, suffix string) string {
	r := []rune(field)
	if len(r) == 0 {
		return suffix
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r) + suffix
}

// inputName derives a NESTED input object's name from its Go type:
// `InsertUserAddressRequest` → `InsertUserAddressInput`, otherwise
// `<Name>Input`. Root input objects are named from the mutation field via
// rootTypeName; only the inner structs reached while walking a Request use
// this Go-derived fallback (they have no field name to derive from).
func inputName(t reflect.Type) string {
	n := graphqlName(t)
	n = strings.TrimSuffix(n, "Request")
	return n + "Input"
}

// ── SDL: input objects + the bodyless payload ───────────────────────────────

// inputObject registers (once) an SDL input object for a write Request struct
// and returns its name. Fields are named by their wire (json) name; nested
// structs become nested input objects; the required marker follows the
// strict / lenient rule.
func (b *sdlBuilder) inputObject(name string, t reflect.Type, strict bool) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if _, ok := b.defs[name]; ok {
		return name
	}
	// Reserve the name before recursing so a self-referential request type
	// resolves to this input instead of looping.
	b.put(name, "input "+name+" { _placeholder: Boolean }")
	var sb strings.Builder
	sb.WriteString("input " + name + " {\n")
	for _, f := range bodyFields(t) {
		sb.WriteString("  " + f.wire + ": " + b.inputTypeRef(f.field.Type, strict) + requiredMark(f.field, strict) + "\n")
	}
	sb.WriteString("}")
	b.defs[name] = sb.String() // overwrite the placeholder with the real body
	return name
}

// inputTypeRef returns the SDL type reference for an input field: scalar,
// nested input object, or list thereof. Unknown shapes degrade to String.
func (b *sdlBuilder) inputTypeRef(t reflect.Type, strict bool) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if s := b.scalarName(t); s != "" {
		return s
	}
	switch t.Kind() {
	case reflect.Struct:
		return b.inputObject(inputName(t), t, strict)
	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Uint8 {
			return "String"
		}
		return "[" + b.inputTypeRef(elem, strict) + "]"
	default:
		return "String"
	}
}

// bodylessPayload registers (once) a bodyless mutation's payload type under
// its field-derived name and returns it. Every bodyless payload carries the
// same fixed shape — `{ success: Boolean!, id: ID }` — but each mutation owns
// its OWN named type (`ArchiveUserPayload`, `DeleteUserPayload`, …), the same
// per-mutation-payload convention the body forms follow; a shared generic
// result type would read as RPC, not GraphQL.
func (b *sdlBuilder) bodylessPayload(name string) string {
	b.put(name, "type "+name+" {\n  success: Boolean!\n  id: ID\n}")
	return name
}

// requiredMark returns "!" when an input field is required: strict (FullBody)
// → always; lenient → non-pointer AND no ,omitempty (mirrors the REST
// required-set rule and the OpenAPI generator).
func requiredMark(f reflect.StructField, strict bool) string {
	if strict {
		return "!"
	}
	if f.Type.Kind() == reflect.Pointer {
		return ""
	}
	if tag := f.Tag.Get("json"); strings.Contains(tag, ",omitempty") {
		return ""
	}
	return "!"
}

// bodyFields returns a struct's body-relevant fields in declaration order:
// exported, not json:"-", not path/query-bound, anonymous structs promoted.
// Used for input objects (the write-side counterpart of exportedJSONFields).
func bodyFields(t reflect.Type) []jsonField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []jsonField
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, bodyFields(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("path") != "" || f.Tag.Get("query") != "" {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		wire := f.Name
		if tag != "" {
			if name, _, _ := strings.Cut(tag, ","); name != "" {
				wire = name
			}
		}
		out = append(out, jsonField{field: f, wire: wire})
	}
	return out
}
