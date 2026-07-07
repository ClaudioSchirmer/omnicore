package graphql

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// ── insert-style command quad (body, no id) ─────────────────────────────────

type mutCmd struct {
	pipeline.CommandBase
	Name string
}

type mutRequest struct {
	Name string `json:"name"`
}

func (r mutRequest) ToCommand() *mutCmd { return &mutCmd{Name: r.Name} }

type mutResult struct {
	ID   string
	Name string
}

type mutResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (mutResponse) FromResult(r mutResult) mutResponse {
	return mutResponse{ID: r.ID, Name: r.Name}
}

type fakeCmdHandler struct {
	captured *mutCmd
	result   mutResult
}

func (h *fakeCmdHandler) Handle(_ *configuration.AppContext, c *mutCmd) (mutResult, error) {
	h.captured = c
	return h.result, nil
}

func TestMutationWithBody_InsertStyleEndToEnd(t *testing.T) {
	h := &fakeCmdHandler{result: mutResult{ID: "x1", Name: "Bob"}}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationWithBody[mutRequest, mutCmd, *mutCmd, mutResult, mutResponse](
			"createThing", mutResponse{}.FromResult, h),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `mutation { createThing(input: { name: "Bob" }) { id name } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	if h.captured == nil || h.captured.Name != "Bob" {
		t.Fatalf("command did not receive the input name, got %+v", h.captured)
	}
	out := resp.Data["createThing"].(map[string]any)
	if out["id"] != "x1" || out["name"] != "Bob" {
		t.Errorf("createThing output = %v, want {id:x1 name:Bob}", out)
	}
}

func TestMutationWithBody_MissingRequiredInputFieldRejected(t *testing.T) {
	h := &fakeCmdHandler{}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationWithBody[mutRequest, mutCmd, *mutCmd, mutResult, mutResponse](
			"createThing", mutResponse{}.FromResult, h),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	// name is non-pointer without omitempty → NonNull input field → validation
	// rejects an empty input object.
	resp := reg.Execute(ctx, `mutation { createThing(input: {}) { id } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("expected validation to reject a missing required input field")
	}
}

// ── bodyless command quad (id only → MutationResult) ────────────────────────

type delCmd struct {
	pipeline.CommandByIDBase
}

type fakeDelHandler struct {
	capturedID string
}

func (h *fakeDelHandler) Handle(_ *configuration.AppContext, c *delCmd) (results.None, error) {
	h.capturedID = c.PathID()
	return results.None{}, nil
}

func TestMutationByID_BodylessReturnsMutationResult(t *testing.T) {
	h := &fakeDelHandler{}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationByID[delCmd, *delCmd, results.None]("deleteThing", h),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `mutation { deleteThing(id: "u9") { success id } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	if h.capturedID != "u9" {
		t.Errorf("path id not injected, got %q", h.capturedID)
	}
	out := resp.Data["deleteThing"].(map[string]any)
	if out["success"] != true || out["id"] != "u9" {
		t.Errorf("deleteThing = %v, want {success:true id:u9}", out)
	}
}

// ── update-style command quad (body + path id) ──────────────────────────────

type mutUpdCmd struct {
	pipeline.CommandByIDBase
	Name string
}

type mutUpdRequest struct {
	Name string `json:"name"`
}

func (r mutUpdRequest) ToCommand() *mutUpdCmd { return &mutUpdCmd{Name: r.Name} }

type mutUpdResult struct {
	ID   string
	Name string
}

type mutUpdResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (mutUpdResponse) FromResult(r mutUpdResult) mutUpdResponse {
	return mutUpdResponse{ID: r.ID, Name: r.Name}
}

type fakeUpdHandler struct {
	captured *mutUpdCmd
}

func (h *fakeUpdHandler) Handle(_ *configuration.AppContext, c *mutUpdCmd) (mutUpdResult, error) {
	h.captured = c
	return mutUpdResult{ID: c.PathID(), Name: c.Name}, nil
}

func TestMutationWithBodyID_InjectsPathIDAndInput(t *testing.T) {
	h := &fakeUpdHandler{}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationWithBodyID[mutUpdRequest, mutUpdCmd, *mutUpdCmd, mutUpdResult, mutUpdResponse](
			"updateThing", mutUpdResponse{}.FromResult, h),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `mutation { updateThing(id: "u7", input: { name: "Renamed" }) { id name } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	if h.captured == nil || h.captured.PathID() != "u7" {
		t.Fatalf("path id not injected via SetPathID, got %+v", h.captured)
	}
	if h.captured.Name != "Renamed" {
		t.Errorf("input not decoded into the command, got name=%q", h.captured.Name)
	}
	out := resp.Data["updateThing"].(map[string]any)
	if out["id"] != "u7" || out["name"] != "Renamed" {
		t.Errorf("updateThing output = %v, want {id:u7 name:Renamed}", out)
	}
}

func TestMutationWithBodyID_SchemaCarriesIDArg(t *testing.T) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		MutationWithBodyID[mutUpdRequest, mutUpdCmd, *mutUpdCmd, mutUpdResult, mutUpdResponse](
			"updateThing", mutUpdResponse{}.FromResult, &fakeUpdHandler{}),
	)
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("SDL: %v", err)
	}
	if !strings.Contains(sdl, "updateThing(id: ID!, input:") {
		t.Errorf("MutationWithBodyID must expose an id: ID! arg + input, SDL:\n%s", sdl)
	}
}

// ── FullBody marker → strict input (every field NonNull) ────────────────────

type mutStrictCmd struct {
	pipeline.CommandBase
	Note *string
}

type mutStrictRequest struct {
	Note *string `json:"note,omitempty"`
}

func (r mutStrictRequest) ToCommand() *mutStrictCmd { return &mutStrictCmd{Note: r.Note} }

type mutStrictResult struct{ ID string }

type mutStrictResponse struct {
	ID string `json:"id"`
}

func (mutStrictResponse) FromResult(r mutStrictResult) mutStrictResponse {
	return mutStrictResponse{ID: r.ID}
}

type fakeStrictHandler struct{ pipeline.FullBody }

func (h *fakeStrictHandler) Handle(_ *configuration.AppContext, _ *mutStrictCmd) (mutStrictResult, error) {
	return mutStrictResult{ID: "x"}, nil
}

func TestMutationWithBody_FullBodyMakesInputStrict(t *testing.T) {
	reg := New(pipeline.New(translation.Default())).Register(
		MutationWithBody[mutStrictRequest, mutStrictCmd, *mutStrictCmd, mutStrictResult, mutStrictResponse](
			"createStrict", mutStrictResponse{}.FromResult, &fakeStrictHandler{}),
	)
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("SDL: %v", err)
	}
	if !strings.Contains(sdl, "note: String!") {
		t.Errorf("FullBody must make even an optional field NonNull in the input, SDL:\n%s", sdl)
	}
}
