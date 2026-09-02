package graphql

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ── input object reflection: bodyFields / inputTypeRef / requiredMark ────────

type covChildInput struct {
	City string `json:"city"`
}

type covRichRequest struct {
	covEmbedBase                       // anonymous struct embed → promoted
	*covPtrEmbedBase                   // anonymous pointer embed → deref + promoted
	hidden           string            //nolint:unused // unexported → skipped
	Skipped          string            `json:"-"`
	PathID           string            `path:"id"`
	Q                string            `query:"q"`
	Name             string            `json:"name"`
	Opt              string            `json:"opt,omitempty"`
	Note             *string           `json:"note"`
	Child            covChildInput     `json:"child"`
	Children         []covChildInput   `json:"children"`
	Tags             []*string         `json:"tags"`
	Blob             []byte            `json:"blob"`
	Pair             [2]int            `json:"pair"`
	Meta             map[string]string `json:"meta"`
}

func TestInputObject_RichRequestCoversEveryShape(t *testing.T) {
	b := newSDLBuilder()
	name := b.inputObject("CovRichInput", reflect.TypeOf(&covRichRequest{}), false) // pointer deref
	def := b.defs[name]

	for _, want := range []string{
		"base: String!",                   // struct embed promoted, required
		"ptrBase: String!",                // pointer embed promoted
		"name: String!",                   // plain non-pointer → required
		"opt: String\n",                   // ,omitempty → optional
		"note: String\n",                  // pointer → optional
		"child: covChildInputInput!",      // nested struct → nested input object
		"children: [covChildInputInput]!", // slice of struct
		"tags: [String]!",                 // slice of *scalar (elem pointer deref)
		"blob: String!",                   // []byte → String
		"pair: [Int]!",                    // array kind
		"meta: String!",                   // map → scalar fallback
	} {
		if !strings.Contains(def, want) {
			t.Errorf("input object missing %q, def:\n%s", want, def)
		}
	}
	for _, absent := range []string{"Skipped", "PathID", "hidden", "q:", "id:"} {
		if strings.Contains(def, absent) {
			t.Errorf("input object must not carry %q, def:\n%s", absent, def)
		}
	}
	// The nested input was registered as its own definition.
	if _, ok := b.defs["covChildInputInput"]; !ok {
		t.Error("nested struct input object not registered")
	}
	// A second registration of an existing name returns without re-emitting.
	if again := b.inputObject("CovRichInput", reflect.TypeOf(covRichRequest{}), false); again != name {
		t.Errorf("duplicate inputObject = %q, want %q", again, name)
	}
}

func TestBodyFields_NonStructIsEmpty(t *testing.T) {
	if got := bodyFields(reflect.TypeOf(42)); len(got) != 0 {
		t.Errorf("non-struct must yield no body fields, got %v", got)
	}
	// Pointer deref happens before the struct check.
	if got := bodyFields(reflect.TypeOf(new(int))); len(got) != 0 {
		t.Errorf("pointer to non-struct must yield no body fields, got %v", got)
	}
}

func TestRequiredMark_LenientBranches(t *testing.T) {
	typ := reflect.TypeOf(covRichRequest{})
	note, _ := typ.FieldByName("Note")
	if got := requiredMark(note, false); got != "" {
		t.Errorf("pointer field must be optional, got %q", got)
	}
	opt, _ := typ.FieldByName("Opt")
	if got := requiredMark(opt, false); got != "" {
		t.Errorf(",omitempty field must be optional, got %q", got)
	}
	if got := requiredMark(note, true); got != "!" {
		t.Errorf("strict mode must force required, got %q", got)
	}
}

// ── decodeInput: missing arg / marshal fail / unmarshal fail ─────────────────

type covDecodeReq struct {
	Name string `json:"name"`
}

func TestDecodeInput_MissingInputYieldsZeroRequest(t *testing.T) {
	req, gerr := decodeInput[covDecodeReq](map[string]any{})
	if gerr != nil {
		t.Fatalf("missing input must not error, got %v", gerr)
	}
	if req.Name != "" {
		t.Errorf("expected the zero request, got %+v", req)
	}
}

func TestDecodeInput_UnmarshalableValueErrors(t *testing.T) {
	_, gerr := decodeInput[covDecodeReq](map[string]any{
		"input": map[string]any{"bad": func() {}},
	})
	if gerr == nil {
		t.Fatal("a func value must fail json.Marshal and surface as an error")
	}
}

func TestDecodeInput_TypeMismatchErrors(t *testing.T) {
	_, gerr := decodeInput[covDecodeReq](map[string]any{
		"input": map[string]any{"name": 123},
	})
	if gerr == nil {
		t.Fatal("a number for a string field must fail json.Unmarshal")
	}
}

// ── structToWire: marshal fail / non-object fallback ─────────────────────────

type covChanResponse struct {
	Ch chan int `json:"ch"`
}

func TestStructToWire_MarshalFailureYieldsEmptyMap(t *testing.T) {
	out, ok := structToWire(covChanResponse{Ch: make(chan int)}).(map[string]any)
	if !ok || len(out) != 0 {
		t.Errorf("unmarshalable response must degrade to an empty map, got %v", out)
	}
}

func TestStructToWire_NonObjectValueYieldsEmptyMap(t *testing.T) {
	out, ok := structToWire(42).(map[string]any)
	if !ok || len(out) != 0 {
		t.Errorf("a non-object value must degrade to an empty map, got %v", out)
	}
}

// ── asString branches ────────────────────────────────────────────────────────

func TestAsString_Branches(t *testing.T) {
	if got := asString("s"); got != "s" {
		t.Errorf("string passthrough = %q", got)
	}
	if got := asString(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
	if got := asString(7); got != "7" {
		t.Errorf("int = %q, want 7", got)
	}
}

// ── mutation failure / exception paths end-to-end ────────────────────────────

type covFailCmdHandler struct{ err error }

func (h *covFailCmdHandler) Handle(_ *configuration.AppContext, _ *mutCmd) (mutResult, error) {
	return mutResult{}, h.err
}

func covBodyRegistry(err error) (*Registry, *configuration.AppContext) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationWithBody[mutRequest, mutCmd, *mutCmd, mutResult, mutResponse](
			"createThing", mutResponse{}.FromResult, &covFailCmdHandler{err: err}),
	)
	return reg, configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func TestMutationWithBody_DomainFailureSurfacesNotification(t *testing.T) {
	reg, ctx := covBodyRegistry(
		domain.SingleNotificationError("Thing", "name", domain.RecordNotFoundNotification{}))
	resp := reg.Execute(ctx, `mutation { createThing(input: { name: "x" }) { id } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a domain failure must surface in errors[]")
	}
	if got := resp.Errors[0].Extensions["notificationKey"]; got != "RecordNotFoundNotification" {
		t.Errorf("notificationKey = %v, want RecordNotFoundNotification", got)
	}
	if resp.Data["createThing"] != nil {
		t.Errorf("failed mutation must not resolve data, got %v", resp.Data["createThing"])
	}
}

func TestMutationWithBody_RawErrorIsOpaqueInternal(t *testing.T) {
	reg, ctx := covBodyRegistry(errors.New("boom"))
	resp := reg.Execute(ctx, `mutation { createThing(input: { name: "x" }) { id } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("an exception must surface in errors[]")
	}
	e := resp.Errors[0]
	if e.Message != "internal server error" || e.Extensions["semantic"] != "Internal" {
		t.Errorf("exception must be opaque Internal, got %+v", e)
	}
	if strings.Contains(e.Message, "boom") {
		t.Error("the raw error text must stay server-side")
	}
}

type covFailDelHandler struct{ err error }

func (h *covFailDelHandler) Handle(_ *configuration.AppContext, _ *delCmd) (results.None, error) {
	return results.None{}, h.err
}

func covDelRegistry(err error) (*Registry, *configuration.AppContext) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationByID[delCmd, *delCmd, results.None]("deleteThing", &covFailDelHandler{err: err}),
	)
	return reg, configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func TestMutationByID_DomainFailureSurfacesNotification(t *testing.T) {
	reg, ctx := covDelRegistry(
		domain.SingleNotificationError("Thing", "id", domain.RecordNotFoundNotification{}))
	resp := reg.Execute(ctx, `mutation { deleteThing(id: "22222222-2222-4222-8222-222222222222") { success } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a domain failure must surface in errors[]")
	}
	if got := resp.Errors[0].Extensions["notificationKey"]; got != "RecordNotFoundNotification" {
		t.Errorf("notificationKey = %v, want RecordNotFoundNotification", got)
	}
}

func TestMutationByID_RawErrorIsOpaqueInternal(t *testing.T) {
	reg, ctx := covDelRegistry(errors.New("boom"))
	resp := reg.Execute(ctx, `mutation { deleteThing(id: "22222222-2222-4222-8222-222222222222") { success } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("an exception must surface in errors[]")
	}
	if got := resp.Errors[0].Extensions["semantic"]; got != "Internal" {
		t.Errorf("semantic = %v, want Internal", got)
	}
}

// ── resolver-level decode failures (crafted args bypass query validation) ────

func TestMutationWithBody_ResolverRejectsBadInput(t *testing.T) {
	f := MutationWithBody[mutRequest, mutCmd, *mutCmd, mutResult, mutResponse](
		"createThing", mutResponse{}.FromResult, &fakeCmdHandler{})
	res := f.makeResolve(pipeline.New(translation.Default()))
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	out, gerrs := res(ctx, map[string]any{"input": map[string]any{"name": 123}}, nil, nil)
	if len(gerrs) == 0 {
		t.Fatal("a type-mismatched input must surface a decode error")
	}
	if out != nil {
		t.Errorf("decode failure must not produce data, got %v", out)
	}
}

func TestMutationWithBodyID_ResolverRejectsBadInput(t *testing.T) {
	f := MutationWithBodyID[mutUpdRequest, mutUpdCmd, *mutUpdCmd, mutUpdResult, mutUpdResponse](
		"updateThing", mutUpdResponse{}.FromResult, &fakeUpdHandler{})
	res := f.makeResolve(pipeline.New(translation.Default()))
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	out, gerrs := res(ctx, map[string]any{
		"id":    "22222222-2222-4222-8222-222222222222",
		"input": map[string]any{"name": 123},
	}, nil, nil)
	if len(gerrs) == 0 {
		t.Fatal("a type-mismatched input must surface a decode error")
	}
	if out != nil {
		t.Errorf("decode failure must not produce data, got %v", out)
	}
}
