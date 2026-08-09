package graphql

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ── fixtures: a by-id read quad (Request / Query / Response / handler) ───────

type byIDQuery struct {
	queries.QueryByIDBase
	IncludeArchived bool
}

func (q *byIDQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{IncludeArchived: q.IncludeArchived}, nil
}

func (q *byIDQuery) ContextName() string { return "User" }

type byIDRequest struct {
	IncludeArchived *bool `query:"includeArchived"`
}

func (r byIDRequest) ToQuery() *byIDQuery {
	arch := false
	if r.IncludeArchived != nil {
		arch = *r.IncludeArchived
	}
	return &byIDQuery{IncludeArchived: arch}
}

// byIDResponse is a DISTINCT Go type from execResponse with the same wire
// fields — the by-id/list Response DTO pair of one entity, proving both map
// onto the single "User" node type.
type byIDResponse struct {
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
	Age  *int64  `json:"age,omitempty"`
}

type fakeByIDHandler struct {
	capturedID   string
	capturedArch bool
	doc          map[string]any
	notFound     bool
}

func (h *fakeByIDHandler) Handle(_ *configuration.AppContext, q *byIDQuery) (map[string]any, error) {
	h.capturedID = q.PathID().String()
	h.capturedArch = q.IncludeArchived
	if h.notFound {
		return nil, domain.NotFoundError("User", "id", q.PathID().String())
	}
	return h.doc, nil
}

// newByIDRegistry registers the singular by-id field BESIDE the plural list
// field under the same entity name — the canonical pairing.
func newByIDRegistry(list *fakeReadHandler, byID *fakeByIDHandler) (*Registry, *configuration.AppContext) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		QueryWithParams[execRequest, execResponse]("users", "User", list),
	).Register(
		QueryByID[byIDRequest, byIDResponse]("user", "User", byID),
	)
	return reg, configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func TestQueryByID_SDLFieldAndSharedNode(t *testing.T) {
	reg, _ := newByIDRegistry(&fakeReadHandler{}, &fakeByIDHandler{})
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("SDL build failed: %v", err)
	}
	if !strings.Contains(sdl, "user(id: ID!, includeArchived: Boolean): User") {
		t.Errorf("by-id field line missing or misshapen:\n%s", sdl)
	}
	// Nullable node: a missing document resolves to null, so no `!`.
	if strings.Contains(sdl, "includeArchived: Boolean): User!") {
		t.Errorf("by-id node must be nullable (no !):\n%s", sdl)
	}
	// One entity name, one node type: the second Response DTO maps onto the
	// existing "User" object and leaks no orphan type into the SDL.
	if strings.Contains(sdl, "byIDResponse") {
		t.Errorf("by-id Response Go type name leaked into the SDL:\n%s", sdl)
	}
	if strings.Count(sdl, "type User {") != 1 {
		t.Errorf("expected exactly one User node type:\n%s", sdl)
	}
}

func TestQueryByID_IncludeArchivedObeysDTO(t *testing.T) {
	// A Request DTO WITHOUT the reserved key (bareRequest) must not emit the
	// argument…
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		QueryByID[bareRequest, byIDResponse]("user", "User", &fakeByIDHandler{doc: map[string]any{"ID": "u1"}}),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("SDL build failed: %v", err)
	}
	if !strings.Contains(sdl, "user(id: ID!): User") || strings.Contains(sdl, "includeArchived: Boolean): User") {
		t.Errorf("no DTO opt-in → no includeArchived argument:\n%s", sdl)
	}
	// …and gqlparser rejects it as an unknown argument before any resolver runs.
	resp := reg.Execute(ctx, `{ user(id: "u1", includeArchived: true) { id } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("undeclared includeArchived must be rejected by validation")
	}
}

// bareRequest is the opt-in-free by-id Request DTO for the test above.
type bareRequest struct{}

func (bareRequest) ToQuery() *byIDQuery { return &byIDQuery{} }

func TestQueryByID_ExecuteEndToEnd(t *testing.T) {
	h := &fakeByIDHandler{doc: map[string]any{"ID": "u1", "Name": "alice", "Age": int64(30)}}
	reg, ctx := newByIDRegistry(&fakeReadHandler{}, h)

	resp := reg.Execute(ctx, `{ user(id: "u1", includeArchived: true) { id name } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if h.capturedID != "u1" {
		t.Errorf("path id = %q, want u1 (SetPathID injection)", h.capturedID)
	}
	if !h.capturedArch {
		t.Error("includeArchived: true did not reach the Query")
	}
	user, ok := resp.Data["user"].(map[string]any)
	if !ok {
		t.Fatalf("data.user = %#v, want object", resp.Data["user"])
	}
	if user["id"] != "u1" || user["name"] != "alice" {
		t.Errorf("node = %v, want {id:u1 name:alice}", user)
	}
	if _, present := user["age"]; present {
		t.Error("unselected field age must be trimmed from the node")
	}
}

func TestQueryByID_IncludeArchivedDefaultsFalse(t *testing.T) {
	h := &fakeByIDHandler{doc: map[string]any{"ID": "u1"}}
	reg, ctx := newByIDRegistry(&fakeReadHandler{}, h)

	resp := reg.Execute(ctx, `{ user(id: "u1") { id } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if h.capturedArch {
		t.Error("absent includeArchived must leave the Query default (false)")
	}
}

func TestQueryByID_NotFoundIsNullPlusCanonicalError(t *testing.T) {
	h := &fakeByIDHandler{notFound: true}
	reg, ctx := newByIDRegistry(&fakeReadHandler{}, h)

	resp := reg.Execute(ctx, `{ user(id: "missing") { id } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a missing document must surface the canonical not-found error")
	}
	if got := resp.Errors[0].Extensions["notificationKey"]; got != "RecordNotFoundNotification" {
		t.Errorf("notificationKey = %v, want RecordNotFoundNotification", got)
	}
	if resp.Data["user"] != nil {
		t.Errorf("data.user = %#v, want null (nullable node)", resp.Data["user"])
	}
}
