package grpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
)

// registerServer registers the create RPC through the PUBLIC attachment API
// (reg.Register + CommandWithBody) and returns a live caller — the
// consumer-facing path end to end.
func registerServer(t *testing.T, reg *Registry, opts ...ProcedureOption) func(req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
	t.Helper()
	reg.Register(CommandWithBody(testProcedure,
		toCreateCommand,
		fromGadgetResult,
		&createGadgetHandler{},
		opts...,
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.CreateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), srv.URL+testProcedure)
	return func(req *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
		return client.CallUnary(context.Background(), req)
	}
}

func TestRegisterCommandWithBodyEndToEnd(t *testing.T) {
	reg := New(pipeline.New(nil))
	call := registerServer(t, reg)
	res, err := call(connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("Widget")}))
	if err != nil || res.Msg.GetId() != "g-1" {
		t.Fatalf("registered RPC: res=%v err=%v", res, err)
	}
	names := reg.ServiceNames()
	if len(names) != 1 || names[0] != "omnicore.grpctest.v1.GadgetService" {
		t.Fatalf("service name not recorded: %v", names)
	}
}

func TestRegisterStrictOption(t *testing.T) {
	reg := New(pipeline.New(nil))
	call := registerServer(t, reg, Strict("name", "kind"))
	_, err := call(connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("Strict via Register: %v", err)
	}
}

func TestRequirePermissionInertWhenAuthorizationOff(t *testing.T) {
	reg := New(pipeline.New(nil)) // EnableAuthorization never called
	call := registerServer(t, reg, RequirePermission("gadgets:write"))
	if _, err := call(connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")})); err != nil {
		t.Fatalf("annotation must be inert with the switch off: %v", err)
	}
}

func TestRequirePermissionDeniesWithoutIdentity(t *testing.T) {
	reg := New(pipeline.New(nil))
	reg.EnableAuthorization(true)
	call := registerServer(t, reg, RequirePermission("gadgets:write"))
	_, err := call(connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	reasons, _, violations := decodeDetails(t, cerr)
	if len(reasons) == 0 || reasons[0] != "MissingPermissionNotification" {
		t.Fatalf("canonical notification missing: %v", reasons)
	}
	if violations["permission"] == "" {
		t.Fatalf("permission field violation missing: %v", violations)
	}
}

func TestRequirePermissionGrantsWithClaim(t *testing.T) {
	authedReg, sign := authedRegistry(t, AuthPolicy{})
	authedReg.EnableAuthorization(true)
	call := registerServer(t, authedReg, RequirePermission("gadgets:write"))

	granted := baseClaims()
	granted["permissions"] = []string{"gadgets:write"}
	req := connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")})
	req.Header().Set("Authorization", "Bearer "+sign(granted))
	if _, err := call(req); err != nil {
		t.Fatalf("granted permission must pass: %v", err)
	}

	// same identity, wrong permission → denied
	denied := baseClaims()
	denied["permissions"] = []string{"gadgets:read"}
	denied["exp"] = time.Now().Add(time.Hour).Unix()
	req = connect.NewRequest(&testpb.CreateGadgetRequest{Name: proto.String("x")})
	req.Header().Set("Authorization", "Bearer "+signWith(t, sign, denied))
	_, err := call(req)
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied {
		t.Fatalf("wrong permission must deny: %v", err)
	}
}

func signWith(t *testing.T, sign func(jwt.MapClaims) string, claims jwt.MapClaims) string {
	t.Helper()
	return sign(claims)
}

// TestRegisterFullFamily exercises the remaining constructors through the
// public attachment API against one live server.
func TestRegisterFullFamily(t *testing.T) {
	reg := New(pipeline.New(nil))
	var sawID string

	reg.Register(CommandWithBodyID("/omnicore.grpctest.v1.GadgetService/UpdateGadget",
		(*testpb.UpdateGadgetRequest).GetId,
		func(pb *testpb.UpdateGadgetRequest) (*updateGadgetCommand, error) {
			return &updateGadgetCommand{Name: pb.GetName()}, nil
		},
		fromGadgetResult,
		&updateGadgetHandler{sawID: &sawID},
	))
	reg.Register(CommandByID[testpb.ArchiveGadgetRequest, testpb.CreateGadgetResponse, archiveGadgetCommand](
		"/omnicore.grpctest.v1.GadgetService/ArchiveGadget",
		(*testpb.ArchiveGadgetRequest).GetId,
		fromGadgetResult,
		archiveGadgetHandler{},
	))
	reg.Register(QueryWithParams("/omnicore.grpctest.v1.GadgetService/ListGadgets",
		func(pb *testpb.ListGadgetsRequest) (*listGadgetsQuery, error) {
			return &listGadgetsQuery{NameContains: pb.GetNameContains()}, nil
		},
		func(r *gadgetResult) *testpb.ListGadgetsResponse {
			return &testpb.ListGadgetsResponse{Total: 1, Names: []string{r.Name}}
		},
		listGadgetsHandler{},
	))
	reg.Register(QueryByID("/omnicore.grpctest.v1.GadgetService/GetGadget",
		(*testpb.GetGadgetRequest).GetId,
		func(pb *testpb.GetGadgetRequest) (*getGadgetQuery, error) {
			return &getGadgetQuery{IncludeArchived: pb.GetIncludeArchived()}, nil
		},
		func(doc map[string]any) *testpb.GetGadgetResponse {
			return &testpb.GetGadgetResponse{Id: doc["ID"].(string), Name: doc["Name"].(string)}
		},
		getGadgetHandler{},
	))

	if names := reg.ServiceNames(); len(names) != 1 {
		t.Fatalf("one service expected across four RPCs: %v", names)
	}

	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	base := srv.URL + "/omnicore.grpctest.v1.GadgetService/"

	update := connect.NewClient[testpb.UpdateGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), base+"UpdateGadget")
	res, err := update.CallUnary(context.Background(), connect.NewRequest(&testpb.UpdateGadgetRequest{
		Id: proto.String("g-7"), Name: proto.String("Renamed"),
	}))
	if err != nil || sawID != "g-7" || res.Msg.GetName() != "Renamed" {
		t.Fatalf("WithBodyID via Register: res=%v err=%v sawID=%q", res, err, sawID)
	}

	archive := connect.NewClient[testpb.ArchiveGadgetRequest, testpb.CreateGadgetResponse](srv.Client(), base+"ArchiveGadget")
	if res, err := archive.CallUnary(context.Background(), connect.NewRequest(&testpb.ArchiveGadgetRequest{Id: proto.String("g-9")})); err != nil || res.Msg.GetId() != "g-9" {
		t.Fatalf("ByID via Register: res=%v err=%v", res, err)
	}

	list := connect.NewClient[testpb.ListGadgetsRequest, testpb.ListGadgetsResponse](srv.Client(), base+"ListGadgets")
	if res, err := list.CallUnary(context.Background(), connect.NewRequest(&testpb.ListGadgetsRequest{NameContains: proto.String("Drill")})); err != nil || res.Msg.GetNames()[0] != "Drill" {
		t.Fatalf("QueryWithParams via Register: res=%v err=%v", res, err)
	}

	get := connect.NewClient[testpb.GetGadgetRequest, testpb.GetGadgetResponse](srv.Client(), base+"GetGadget")
	if res, err := get.CallUnary(context.Background(), connect.NewRequest(&testpb.GetGadgetRequest{Id: proto.String("g-42")})); err != nil || res.Msg.GetId() != "g-42" {
		t.Fatalf("QueryByID via Register: res=%v err=%v", res, err)
	}
}

func TestServiceOfAndMountProcedureEdgeCases(t *testing.T) {
	if got := serviceOf("no-leading-slash-and-no-second"); got != "" {
		t.Fatalf("procedure without service segment: %q", got)
	}
	reg := New(pipeline.New(nil))
	reg.mountProcedure("/lonely", http.NotFoundHandler()) // no second slash → no service recorded
	if names := reg.ServiceNames(); len(names) != 0 {
		t.Fatalf("malformed procedure must not record a service: %v", names)
	}
}
