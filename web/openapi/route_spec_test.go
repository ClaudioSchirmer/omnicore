package openapi

import (
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/web/responses"
)

// Body+Response — the common case on POST/PUT/PATCH manual routes.
func TestRouteSpecOf_PopulatesRequestAndResponseTypes(t *testing.T) {
	type insertReq struct {
		Name string `json:"name"`
	}
	type insertResp struct {
		ID string `json:"id"`
	}

	got := RouteSpecOf[insertReq, insertResp](201)

	if got.RequestType == nil || got.RequestType != reflect.TypeOf(insertReq{}) {
		t.Fatalf("RequestType: got %+v, want %v", got.RequestType, reflect.TypeOf(insertReq{}))
	}
	if got.ResponseType == nil || got.ResponseType != reflect.TypeOf(insertResp{}) {
		t.Fatalf("ResponseType: got %+v, want %v", got.ResponseType, reflect.TypeOf(insertResp{}))
	}
	if got.SuccessStatus != 201 {
		t.Fatalf("SuccessStatus: got %d, want 201", got.SuccessStatus)
	}
	if got.Strict {
		t.Fatal("Strict should be false — manual path does not type-assert FullBody")
	}
	if got.HasPathID {
		t.Fatal("HasPathID should be false — manual path does not auto-bind :id")
	}
}

// fwresponses.None as TResp — the DELETE/Archive/Unarchive 204-style case.
// ResponseType is populated (the canonical None struct), and the spec
// assembler's isResponseNone() picks it up so the rendered envelope omits
// `data` — equivalent to leaving ResponseType nil.
func TestRouteSpecOf_NoneResponseRecognizedByAssembler(t *testing.T) {
	type keyReq struct {
		Email string `path:"email"`
	}

	got := RouteSpecOf[keyReq, responses.None](204)

	if got.ResponseType == nil {
		t.Fatal("ResponseType should carry responses.None, not be nil")
	}
	if !isResponseNone(got.ResponseType) {
		t.Fatalf("isResponseNone rejected %v — assembler would still try to render data", got.ResponseType)
	}
	if got.SuccessStatus != 204 {
		t.Fatalf("SuccessStatus: got %d, want 204", got.SuccessStatus)
	}
}

// Equivalence ruler — the helper produces the same RouteSpec a consumer
// would have hand-rolled with reflect.TypeOf literals (sans Strict /
// HasPathID, which are manual-path opt-ins).
func TestRouteSpecOf_EquivalentToHandRolledRouteSpec(t *testing.T) {
	type updateReq struct {
		Name string `json:"name"`
	}
	type updateResp struct {
		ID string `json:"id"`
	}

	got := RouteSpecOf[updateReq, updateResp](200)
	want := RouteSpec{
		RequestType:   reflect.TypeOf(updateReq{}),
		ResponseType:  reflect.TypeOf(updateResp{}),
		SuccessStatus: 200,
	}

	if got != want {
		t.Fatalf("Spec helper diverged from hand-rolled literal:\n got=%+v\nwant=%+v", got, want)
	}
}

// ─── ResponseOf — companion helper for MountRaw routes ────────────────────

// Common case — the helper carries Description + Type and leaves
// ContentType / Examples zero so the consumer can attach them after the
// fact via plain struct field assignment.
func TestResponseOf_PopulatesTypeAndDescription(t *testing.T) {
	type whoamiResp struct {
		Subject string `json:"subject"`
	}

	got := ResponseOf[whoamiResp]("Authenticated identity")

	if got.Type == nil || got.Type != reflect.TypeOf(whoamiResp{}) {
		t.Fatalf("Type: got %+v, want %v", got.Type, reflect.TypeOf(whoamiResp{}))
	}
	if got.Description != "Authenticated identity" {
		t.Fatalf("Description: got %q, want %q", got.Description, "Authenticated identity")
	}
	if got.ContentType != "" {
		t.Fatalf("ContentType should be zero so the consumer can override; got %q", got.ContentType)
	}
	if got.Examples != nil {
		t.Fatalf("Examples should be nil so the consumer can attach them; got %v", got.Examples)
	}
}

// Equivalence ruler — the helper produces the same ResponseSpec a
// consumer would have hand-rolled with a reflect.TypeOf literal.
func TestResponseOf_EquivalentToHandRolledResponseSpec(t *testing.T) {
	type listResp struct {
		Items []string `json:"items"`
	}

	got := ResponseOf[listResp]("List of items")
	want := ResponseSpec{
		Description: "List of items",
		Type:        reflect.TypeOf(listResp{}),
	}

	if got.Description != want.Description {
		t.Fatalf("Description divergence: got %q, want %q", got.Description, want.Description)
	}
	if got.Type != want.Type {
		t.Fatalf("Type divergence: got %v, want %v", got.Type, want.Type)
	}
}

// Composability — start from the helper, attach Examples afterward.
// Matches the documented usage in MountRaw routes that want named
// example payloads on a typed response.
func TestResponseOf_AcceptsPostHocExamples(t *testing.T) {
	type echoResp struct {
		Body string `json:"body"`
	}

	spec := ResponseOf[echoResp]("Echoed body")
	spec.Examples = map[string]Example{
		"sample": {Summary: "Plain echo", Value: echoResp{Body: "hello"}},
	}

	if spec.Type != reflect.TypeOf(echoResp{}) {
		t.Fatalf("Type lost after post-hoc Examples assignment; got %v", spec.Type)
	}
	if len(spec.Examples) != 1 || spec.Examples["sample"].Summary != "Plain echo" {
		t.Fatalf("Examples not preserved after composing on the helper output; got %+v", spec.Examples)
	}
}
