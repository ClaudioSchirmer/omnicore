package grpc

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
	omnicorepb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
)

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"user_name": "username",
		"userName":  "username",
		"UserName":  "username",
		"ID":        "id",
		"id":        "id",
		"":          "",
	} {
		if got := normalizeName(in); got != want {
			t.Fatalf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDTOFieldsOfTagRules(t *testing.T) {
	type embedded struct {
		Inner string `json:"inner"`
	}
	type dto struct {
		embedded
		Named   string `json:"wire_named"`
		Skipped string `json:"-"`
		Query   string `query:"queryKey"`
		Plain   string
		hidden  string //nolint:unused // exercised: unexported must be skipped
	}
	fields := dtoFieldsOf(reflect.TypeOf(dto{}))
	if f, ok := fields["inner"]; !ok || f.name != "Inner" {
		t.Fatalf("anonymous embed not promoted: %v", fields)
	}
	if f, ok := fields["wirenamed"]; !ok || f.jsonKey != "wire_named" {
		t.Fatalf("json tag key missing: %v", fields)
	}
	if _, ok := fields[normalizeName("Skipped")]; ok {
		t.Fatalf(`json:"-" must be excluded`)
	}
	if f, ok := fields["querykey"]; !ok || f.name != "Query" {
		t.Fatalf("query tag spelling missing: %v", fields)
	}
	if _, ok := fields["plain"]; !ok {
		t.Fatalf("plain field name spelling missing")
	}
	if _, ok := fields["hidden"]; ok {
		t.Fatalf("unexported field must be skipped")
	}
}

func TestCompileBindPlanErrors(t *testing.T) {
	createMD := (&testpb.CreateGadgetRequest{}).ProtoReflect().Descriptor()

	// non-struct DTO
	if _, err := compileBindPlan("t", "request", createMD, reflect.TypeOf("x"), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "is not a struct") {
		t.Fatalf("non-struct DTO: %v", err)
	}

	// missing counterpart
	type missing struct {
		Name string `json:"name"`
	}
	if _, err := compileBindPlan("t", "request", createMD, reflect.TypeOf(missing{}), nil, nil); err == nil ||
		!strings.Contains(err.Error(), `field "kind" has no counterpart`) {
		t.Fatalf("missing counterpart: %v", err)
	}

	// exemption silences the same field
	type onlyKindRating struct {
		Kind   string `json:"kind"`
		Rating int32  `json:"rating"`
	}
	if _, err := compileBindPlan("t", "request", createMD, reflect.TypeOf(onlyKindRating{}),
		map[string]bool{"name": true}, nil); err != nil {
		t.Fatalf("exempt field must not require a counterpart: %v", err)
	}

	// alias resolves an unmatchable pairing; alias to a ghost field fails
	type aliased struct {
		Name     string `json:"name"`
		Category string `json:"category"`
		Rating   int32  `json:"rating"`
	}
	if _, err := compileBindPlan("t", "request", createMD, reflect.TypeOf(aliased{}), nil,
		map[string]string{"kind": "Category"}); err != nil {
		t.Fatalf("alias must resolve: %v", err)
	}
	if _, err := compileBindPlan("t", "request", createMD, reflect.TypeOf(aliased{}), nil,
		map[string]string{"kind": "Ghost"}); err == nil || !strings.Contains(err.Error(), "names no field") {
		t.Fatalf("alias to ghost field: %v", err)
	}

	// proto map fields are out of the bridge's vocabulary
	mapMD := (&testpb.MapCarrier{}).ProtoReflect().Descriptor()
	type withLabels struct {
		Labels map[string]string `json:"labels"`
	}
	if _, err := compileBindPlan("t", "request", mapMD, reflect.TypeOf(withLabels{}), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "proto map") {
		t.Fatalf("map field: %v", err)
	}

	// repeated proto field demands a slice
	listMD := (&testpb.ListGadgetsResponse{}).ProtoReflect().Descriptor()
	type notSlice struct {
		Total int64  `json:"total"`
		Names string `json:"names"`
	}
	if _, err := compileBindPlan("t", "response", listMD, reflect.TypeOf(notSlice{}), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "not a slice") {
		t.Fatalf("repeated vs non-slice: %v", err)
	}

	// google.protobuf.Timestamp pairs with time.Time only
	timeMD := (&testpb.TimeCarrier{}).ProtoReflect().Descriptor()
	type goodTime struct {
		When time.Time `json:"when"`
	}
	if _, err := compileBindPlan("t", "request", timeMD, reflect.TypeOf(goodTime{}), nil, nil); err != nil {
		t.Fatalf("Timestamp ↔ time.Time must pass: %v", err)
	}
	type badTime struct {
		When string `json:"when"`
	}
	if _, err := compileBindPlan("t", "request", timeMD, reflect.TypeOf(badTime{}), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "not time.Time") {
		t.Fatalf("Timestamp vs string: %v", err)
	}

	// other well-known types are unsupported
	maskCarrierMD := (&testpb.MaskCarrier{}).ProtoReflect().Descriptor()
	type maskDTO struct {
		Mask struct{} `json:"mask"`
	}
	if _, err := compileBindPlan("t", "request", maskCarrierMD, reflect.TypeOf(maskDTO{}), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "well-known type") {
		t.Fatalf("well-known rejection: %v", err)
	}

	// message field vs non-struct DTO field
	parentMD := (&testpb.ParentThing{}).ProtoReflect().Descriptor()
	type badParent struct {
		Kids     []string `json:"kids"`
		Favorite string   `json:"favorite"`
	}
	if _, err := compileBindPlan("t", "request", parentMD, reflect.TypeOf(badParent{}), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "not a struct") {
		t.Fatalf("message vs non-struct: %v", err)
	}
}

type childDTO struct {
	FullName string // no tag: json key "FullName" ≠ wire "fullName" → rename
}

type parentDTO struct {
	Kids     []childDTO `json:"kids"`
	Favorite *childDTO  `json:"favorite,omitempty"`
}

func TestBridgeRenamesBothDirectionsWithNesting(t *testing.T) {
	plan, err := compileBindPlan("t", "request",
		(&testpb.ParentThing{}).ProtoReflect().Descriptor(), reflect.TypeOf(parentDTO{}), nil, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !plan.hasNodes() {
		t.Fatalf("nested rename expected")
	}

	// pb → DTO: repeated + nested message, keys renamed per level
	msg := &testpb.ParentThing{
		Kids:     []*testpb.ChildThing{{FullName: "a"}, {FullName: "b"}},
		Favorite: &testpb.ChildThing{FullName: "fav"},
	}
	dto, err := pbToDTO[parentDTO](plan, msg)
	if err != nil {
		t.Fatalf("pbToDTO: %v", err)
	}
	if len(dto.Kids) != 2 || dto.Kids[1].FullName != "b" || dto.Favorite == nil || dto.Favorite.FullName != "fav" {
		t.Fatalf("nested bridge mismatch: %+v", dto)
	}

	// DTO → pb: the inverse renames
	back, err := dtoToPB[testpb.ParentThing](plan, dto)
	if err != nil {
		t.Fatalf("dtoToPB: %v", err)
	}
	if len(back.GetKids()) != 2 || back.GetKids()[0].GetFullName() != "a" || back.GetFavorite().GetFullName() != "fav" {
		t.Fatalf("inverse bridge mismatch: %v", back)
	}
}

func TestBridgePresenceAndSubset(t *testing.T) {
	plan, err := compileBindPlan("t", "request",
		(&testpb.CreateGadgetRequest{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(struct {
			Name   *string `json:"name"`
			Kind   *string `json:"kind"`
			Rating *int32  `json:"rating"`
		}{}), nil, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	dto, err := pbToDTO[struct {
		Name   *string `json:"name"`
		Kind   *string `json:"kind"`
		Rating *int32  `json:"rating"`
	}](plan, &testpb.CreateGadgetRequest{Name: proto.String("Widget")})
	if err != nil {
		t.Fatalf("pbToDTO: %v", err)
	}
	if dto.Name == nil || *dto.Name != "Widget" {
		t.Fatalf("present optional must land: %+v", dto)
	}
	if dto.Kind != nil || dto.Rating != nil {
		t.Fatalf("absent optionals must stay nil: %+v", dto)
	}

	// DTO-only fields are dropped on the way out (proto subset rule)
	respPlan, err := compileBindPlan("t", "response",
		(&testpb.CreateGadgetResponse{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Extra string `json:"extra"`
		}{}), nil, nil)
	if err != nil {
		t.Fatalf("compile response: %v", err)
	}
	out, err := dtoToPB[testpb.CreateGadgetResponse](respPlan, struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Extra string `json:"extra"`
	}{ID: "g-1", Name: "Widget", Extra: "dropped"})
	if err != nil {
		t.Fatalf("dtoToPB: %v", err)
	}
	if out.GetId() != "g-1" || out.GetName() != "Widget" {
		t.Fatalf("subset bridge mismatch: %v", out)
	}
}

func expectBootFail(t *testing.T, fragment string, fn func()) {
	t.Helper()
	defer func() {
		p := recover()
		if p == nil {
			t.Fatalf("boot failure expected (%s)", fragment)
		}
		if msg, ok := p.(string); !ok || !strings.Contains(msg, fragment) {
			t.Fatalf("boot failure message %v must carry %q", p, fragment)
		}
	}()
	fn()
}

func TestBootFailures(t *testing.T) {
	pipe := pipeline.New(nil)

	// pb type param that is not a generated message
	expectBootFail(t, "not a generated proto message", func() {
		reg := New(pipe)
		reg.Register(CommandWithBody[int, testpb.CreateGadgetResponse]("/x.v1.S/M",
			createGadgetDTO{}, gadgetResponseDTO{}.FromResult, &createGadgetHandler{}))
	})

	// request field with no DTO counterpart
	expectBootFail(t, "has no counterpart", func() {
		reg := New(pipe)
		reg.Register(CommandWithBody[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse]("/x.v1.S/M",
			lonelyDTO{}, gadgetResponseDTO{}.FromResult, &createGadgetHandler{}))
	})

	// CommandByID: extra request field is unreachable
	expectBootFail(t, "unreachable", func() {
		reg := New(pipe)
		reg.Register(CommandByID[testpb.UpdateGadgetRequest, testpb.ArchiveGadgetResponse, archiveGadgetCommand](
			"/x.v1.S/M", (*testpb.UpdateGadgetRequest).GetId, archiveGadgetHandler{}))
	})

	// CommandByID: response with fields is not a byID shape
	expectBootFail(t, "empty message", func() {
		reg := New(pipe)
		reg.Register(CommandByID[testpb.ArchiveGadgetRequest, testpb.CreateGadgetResponse, archiveGadgetCommand](
			"/x.v1.S/M", (*testpb.ArchiveGadgetRequest).GetId, archiveGadgetHandler{}))
	})

	// QueryWithParams: bespoke scalar in the request
	expectBootFail(t, "shared read contract", func() {
		reg := New(pipe)
		reg.Register(QueryWithParams[testpb.ListGadgetsRequest, testpb.SearchGadgetsResponse]("/x.v1.S/M",
			searchGadgetsDTO{}, gadgetItemDTO{}.FromResult, searchGadgetsHandler{}))
	})

	// QueryWithParams: filter without a `filter:`-tagged DTO leaf
	expectBootFail(t, "filter:", func() {
		reg := New(pipe)
		reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse]("/x.v1.S/M",
			narrowSearchDTO{}, gadgetItemDTO{}.FromResult, searchGadgetsHandler{}))
	})

	// QueryWithParams: two filter groups
	expectBootFail(t, "two filter groups", func() {
		reg := New(pipe)
		reg.Register(QueryWithParams[testpb.TwoGroupsRequest, testpb.SearchGadgetsResponse]("/x.v1.S/M",
			searchGadgetsDTO{}, gadgetItemDTO{}.FromResult, searchGadgetsHandler{}))
	})

	// list envelope shapes
	expectBootFail(t, "two repeated message fields", func() {
		reg := New(pipe)
		reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.TwoRepeatsResponse]("/x.v1.S/M",
			searchGadgetsDTO{}, gadgetItemDTO{}.FromResult, searchGadgetsHandler{}))
	})
	expectBootFail(t, "no repeated items message", func() {
		reg := New(pipe)
		reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.OnlyPaginationInfoResponse]("/x.v1.S/M",
			searchGadgetsDTO{}, gadgetItemDTO{}.FromResult, searchGadgetsHandler{}))
	})
	expectBootFail(t, "two PaginationInfo fields", func() {
		reg := New(pipe)
		reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.TwoPaginationInfosResponse]("/x.v1.S/M",
			searchGadgetsDTO{}, gadgetItemDTO{}.FromResult, searchGadgetsHandler{}))
	})
	expectBootFail(t, "not part of the list envelope", func() {
		reg := New(pipe)
		reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.ListGadgetsResponse]("/x.v1.S/M",
			searchGadgetsDTO{}, gadgetItemDTO{}.FromResult, searchGadgetsHandler{}))
	})

	// QueryByID: request field with no DTO counterpart (id is exempt, the
	// bespoke one is not)
	expectBootFail(t, "has no counterpart", func() {
		reg := New(pipe)
		reg.Register(QueryByID[testpb.GetGadgetRequest, testpb.GetGadgetResponse]("/x.v1.S/M",
			(*testpb.GetGadgetRequest).GetId,
			bareGetDTO{},
			getGadgetResponseDTO{}.FromResult,
			getGadgetHandler{}))
	})
}

// mismatchedCmdDTO declares name as an int — the scalar kinds only meet at
// runtime, where the bridge failure surfaces as the REST body-parse
// rejection (SchemaViolation → INVALID_ARGUMENT).
type mismatchedCmdDTO struct {
	Name   int    `json:"name"`
	Kind   string `json:"kind"`
	Rating int32  `json:"rating"`
}

func (mismatchedCmdDTO) ToCommand() *createGadgetCommand { return &createGadgetCommand{} }

// leakyResultDTO carries a channel — json.Marshal fails, proving the
// response-side bridge failure stays an opaque INTERNAL.
type leakyResultDTO struct {
	ID string   `json:"id"`
	Ch chan int `json:"ch"`
}

// mismatchedItemDTO renders kind as a JSON object where the proto wants a
// string — the per-item bridge failure path of the list envelope.
type mismatchedItemDTO struct {
	ID   string           `json:"id"`
	Name string           `json:"name"`
	Kind struct{ X bool } `json:"kind"`
}

func TestBridgeRuntimeErrors(t *testing.T) {
	// request side: wire string → DTO int rejects as INVALID_ARGUMENT
	reg := New(pipeline.New(nil))
	reg.Register(CommandWithBody[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](testProcedure,
		mismatchedCmdDTO{}, gadgetResponseDTO{}.FromResult, &createGadgetHandler{}))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.CreateGadgetRequest{
		Name: proto.String("Widget"), Kind: proto.String("tool"), Rating: proto.Int32(1),
	}))
	expectInvalidArgument(t, err, "schema")

	// response side: an unmarshalable DTO is an opaque INTERNAL
	respPlan, err := compileBindPlan("t", "response",
		(&testpb.CreateGadgetResponse{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(leakyResultDTO{}), map[string]bool{"name": true}, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := dtoToPB[testpb.CreateGadgetResponse](respPlan, leakyResultDTO{ID: "x", Ch: make(chan int)}); err == nil {
		t.Fatalf("channel DTO must fail json.Marshal")
	}

	// response side: DTO value the proto kind rejects (string field fed a
	// number) — protojson refuses
	numPlan, err := compileBindPlan("t", "response",
		(&testpb.GadgetItem{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(mismatchedItemDTO{}), nil, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := dtoToPB[testpb.GadgetItem](numPlan, mismatchedItemDTO{ID: "x", Name: "n", Kind: struct{ X bool }{true}}); err == nil {
		t.Fatalf("object into proto string must fail protojson")
	}

	// list envelope: the per-item bridge failure propagates out of
	// buildListResponse
	env, err := compileListEnvelope("t",
		(&testpb.SearchGadgetsResponse{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(mismatchedItemDTO{}), nil)
	if err != nil {
		t.Fatalf("compile envelope: %v", err)
	}
	_, err = buildListResponse[testpb.SearchGadgetsResponse](env,
		func(map[string]any) mismatchedItemDTO {
			return mismatchedItemDTO{ID: "g", Name: "n", Kind: struct{ X bool }{true}}
		},
		queries.PageOf[map[string]any]{Items: []map[string]any{{"ID": "g"}}, TotalCount: 1})
	if err == nil {
		t.Fatalf("item bridge failure must propagate")
	}
}

// brokenResultDTO makes FromResult unmarshalable — the response-side
// bridge failure must surface as the opaque INTERNAL end to end.
type brokenResultDTO struct {
	ID string   `json:"id"`
	Ch chan int `json:"name"`
}

func TestResponseBridgeFailureIsOpaqueInternal(t *testing.T) {
	reg := New(pipeline.New(nil))
	reg.Register(CommandWithBody[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](testProcedure,
		createGadgetDTO{},
		func(*gadgetResult) brokenResultDTO { return brokenResultDTO{ID: "x", Ch: make(chan int)} },
		&createGadgetHandler{}))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.CreateGadgetRequest{
		Name: proto.String("Widget"),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInternal {
		t.Fatalf("bridge failure must be opaque INTERNAL: %v", err)
	}
}

func TestFilterAliasBindsTheLeaf(t *testing.T) {
	var crit queries.ReadCriteria
	reg := New(pipeline.New(nil))
	reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		"/omnicore.grpctest.v1.GadgetService/SearchGadgets",
		searchGadgetsDTO{},
		gadgetItemDTO{}.FromResult,
		searchGadgetsHandler{sawCriteria: &crit},
		Alias("name", "Name"), // redundant pairing, exercises the filter alias seam
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		srv.Client(), srv.URL+"/omnicore.grpctest.v1.GadgetService/SearchGadgets")
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Filters: &testpb.SearchGadgetsFilters{
			Name: &omnicorepb.StringFilter{Conditions: []*omnicorepb.StringCondition{{
				Op: omnicorepb.StringOp_STRING_OP_EQ, Values: []string{"Drill"},
			}}},
		},
	})); err != nil {
		t.Fatalf("aliased filter must pass: %v", err)
	}
	if _, ok := crit.Filter["Name"]; !ok {
		t.Fatalf("aliased filter must land on the Go path: %+v", crit.Filter)
	}
}

type NamedCount int

type embeddedNonStructDTO struct {
	NamedCount `json:"count"`
}

func TestCollectDTOFieldsEdges(t *testing.T) {
	// pointer DTO dereferences; a non-struct anonymous embed is a plain
	// field; a non-struct type yields nothing
	fields := dtoFieldsOf(reflect.TypeOf(&embeddedNonStructDTO{}))
	if f, ok := fields["count"]; !ok || f.jsonKey != "count" {
		t.Fatalf("non-struct anonymous embed must be a plain field: %v", fields)
	}
	if got := dtoFieldsOf(reflect.TypeOf(42)); len(got) != 0 {
		t.Fatalf("non-struct type yields no fields: %v", got)
	}
}

func TestBootFailuresOnResponseSide(t *testing.T) {
	pipe := pipeline.New(nil)

	// RPB type param that is not a generated message
	expectBootFail(t, "not a generated proto message", func() {
		reg := New(pipe)
		reg.Register(CommandWithBody[testpb.CreateGadgetRequest, int](testProcedure,
			createGadgetDTO{}, gadgetResponseDTO{}.FromResult, &createGadgetHandler{}))
	})

	// command response field with no DTO counterpart
	expectBootFail(t, "has no counterpart", func() {
		reg := New(pipe)
		reg.Register(CommandWithBody[testpb.CreateGadgetRequest, testpb.GetGadgetResponse](testProcedure,
			createGadgetDTO{}, func(*gadgetResult) lonelyDTO { return lonelyDTO{} }, &createGadgetHandler{}))
	})

	// QueryByID response field with no DTO counterpart
	expectBootFail(t, "has no counterpart", func() {
		reg := New(pipe)
		reg.Register(QueryByID[testpb.GetGadgetRequest, testpb.GetGadgetResponse]("/x.v1.S/M",
			(*testpb.GetGadgetRequest).GetId,
			getGadgetDTO{},
			func(gadgetDetailResult) lonelyDTO { return lonelyDTO{} },
			getGadgetHandler{}))
	})
}

// lonelyDTO carries fewer fields than CreateGadgetRequest declares — the
// wire contract would silently ignore kind/rating.
type lonelyDTO struct {
	Name string `json:"name"`
}

func (d lonelyDTO) ToCommand() *createGadgetCommand { return &createGadgetCommand{Name: d.Name} }

// narrowSearchDTO declares only the name leaf — the proto's rating/price/
// active/created_at filters have no allowlist to inherit.
type narrowSearchDTO struct {
	Name *string `query:"name" filter:"eq"`
}

func (narrowSearchDTO) ToQuery(c queries.ReadCriteria) *searchGadgetsQuery {
	return &searchGadgetsQuery{Criteria: c}
}

// bareGetDTO has no IncludeArchived counterpart for GetGadgetRequest's
// include_archived field.
type bareGetDTO struct{}

func (bareGetDTO) ToQuery(criteria queries.ReadCriteria) *getGadgetQuery {
	return &getGadgetQuery{Criteria: criteria}
}

// searchServer registers the Search RPC and returns a typed caller.
func searchServer(t *testing.T, sawCriteria *queries.ReadCriteria, opts ...ProcedureOption) func(*testpb.SearchGadgetsRequest) (*testpb.SearchGadgetsResponse, error) {
	t.Helper()
	reg := New(pipeline.New(nil))
	reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		"/omnicore.grpctest.v1.GadgetService/SearchGadgets",
		searchGadgetsDTO{},
		gadgetItemDTO{}.FromResult,
		searchGadgetsHandler{sawCriteria: sawCriteria},
		opts...,
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		srv.Client(), srv.URL+"/omnicore.grpctest.v1.GadgetService/SearchGadgets")
	return func(req *testpb.SearchGadgetsRequest) (*testpb.SearchGadgetsResponse, error) {
		res, err := client.CallUnary(context.Background(), connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
}

func expectInvalidArgument(t *testing.T, err error, fragment string) {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument carrying %q, got %v", fragment, err)
	}
}

func TestFilterTagAllowlistEnforced(t *testing.T) {
	var crit queries.ReadCriteria
	call := searchServer(t, &crit)

	// every declared kind passes with an allowed operator
	created := timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if _, err := call(&testpb.SearchGadgetsRequest{Filters: &testpb.SearchGadgetsFilters{
		Name:      &omnicorepb.StringFilter{Conditions: []*omnicorepb.StringCondition{{Op: omnicorepb.StringOp_STRING_OP_EQ, Values: []string{"Drill"}}}},
		Rating:    &omnicorepb.Int64Filter{Conditions: []*omnicorepb.Int64Condition{{Op: omnicorepb.NumberOp_NUMBER_OP_GTE, Values: []int64{3}}}},
		Price:     &omnicorepb.DoubleFilter{Conditions: []*omnicorepb.DoubleCondition{{Op: omnicorepb.NumberOp_NUMBER_OP_GT, Values: []float64{9.5}}}},
		Active:    &omnicorepb.BoolFilter{Conditions: []*omnicorepb.BoolCondition{{Op: omnicorepb.BoolOp_BOOL_OP_EQ, Value: true}}},
		CreatedAt: &omnicorepb.TimestampFilter{Conditions: []*omnicorepb.TimestampCondition{{Op: omnicorepb.NumberOp_NUMBER_OP_GTE, Values: []*timestamppb.Timestamp{created}}}},
	}}); err != nil {
		t.Fatalf("allowed operators must pass: %v", err)
	}
	for _, key := range []string{"Name", "Rating", "Price", "Active", "CreatedAt"} {
		if _, ok := crit.Filter[key]; !ok {
			t.Fatalf("filter %q missing from criteria: %+v", key, crit.Filter)
		}
	}

	// one rejection per wrapper kind: the operator exists in the wire enum
	// but not in the leaf's `filter:` tag
	cases := []struct {
		name    string
		filters *testpb.SearchGadgetsFilters
	}{
		{"string", &testpb.SearchGadgetsFilters{Name: &omnicorepb.StringFilter{Conditions: []*omnicorepb.StringCondition{{Op: omnicorepb.StringOp_STRING_OP_STARTSWITH, Values: []string{"D"}}}}}},
		{"int64", &testpb.SearchGadgetsFilters{Rating: &omnicorepb.Int64Filter{Conditions: []*omnicorepb.Int64Condition{{Op: omnicorepb.NumberOp_NUMBER_OP_NE, Values: []int64{1}}}}}},
		{"double", &testpb.SearchGadgetsFilters{Price: &omnicorepb.DoubleFilter{Conditions: []*omnicorepb.DoubleCondition{{Op: omnicorepb.NumberOp_NUMBER_OP_EQ, Values: []float64{1}}}}}},
		{"bool", &testpb.SearchGadgetsFilters{Active: &omnicorepb.BoolFilter{Conditions: []*omnicorepb.BoolCondition{{Op: omnicorepb.BoolOp_BOOL_OP_NE, Value: true}}}}},
		{"timestamp", &testpb.SearchGadgetsFilters{CreatedAt: &omnicorepb.TimestampFilter{Conditions: []*omnicorepb.TimestampCondition{{Op: omnicorepb.NumberOp_NUMBER_OP_LTE, Values: []*timestamppb.Timestamp{created}}}}}},
	}
	for _, tc := range cases {
		_, err := call(&testpb.SearchGadgetsRequest{Filters: tc.filters})
		if err == nil {
			t.Fatalf("%s: operator outside the tag must reject", tc.name)
		}
		expectInvalidArgument(t, err, "allowlist")
	}
}

func TestReadMaskAndSortSpeakItemWireNames(t *testing.T) {
	var crit queries.ReadCriteria
	call := searchServer(t, &crit)

	if _, err := call(&testpb.SearchGadgetsRequest{
		OrderBy: []*omnicorepb.OrderByField{{Field: "name", Desc: true}},
		Fields:  &fieldmaskpb.FieldMask{Paths: []string{"id", "kind"}},
	}); err != nil {
		t.Fatalf("declared mask/sort paths must pass: %v", err)
	}
	if len(crit.OrderBy) != 1 || crit.OrderBy[0].Field != "Name" || !crit.OrderBy[0].Desc {
		t.Fatalf("sort must resolve to the Go doc path: %+v", crit.OrderBy)
	}
	if crit.Projection["ID"] != 1 || crit.Projection["Kind"] != 1 {
		t.Fatalf("read_mask must resolve to Go doc paths: %+v", crit.Projection)
	}

	_, err := call(&testpb.SearchGadgetsRequest{
		OrderBy: []*omnicorepb.OrderByField{{Field: "phone"}},
	})
	expectInvalidArgument(t, err, "not a declared field")
}

type aliasCmdDTO struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Rating   int32  `json:"rating"`
}

func (d aliasCmdDTO) ToCommand() *createGadgetCommand {
	return &createGadgetCommand{Name: d.Name, Kind: d.Category, Rating: d.Rating}
}

func TestAliasEndToEnd(t *testing.T) {
	reg := New(pipeline.New(nil))
	reg.Register(CommandWithBody[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](testProcedure,
		aliasCmdDTO{},
		gadgetResponseDTO{}.FromResult,
		&createGadgetHandler{},
		Alias("kind", "Category"),
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	res, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.CreateGadgetRequest{
		Name: proto.String("Widget"), Kind: proto.String("tool"), Rating: proto.Int32(1),
	}))
	if err != nil || res.Msg.GetName() != "Widget" {
		t.Fatalf("alias e2e: res=%v err=%v", res, err)
	}
}
