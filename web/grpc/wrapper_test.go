package grpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
)

// --- fake application layer (the same shapes a real service wires) ---

type createGadgetCommand struct {
	pipeline.CommandBase
	Name   string
	Kind   string
	Rating int32
}

type gadgetResult struct{ ID, Name string }

type createGadgetHandler struct {
	fail      error
	sawIdent  *string
	sawCancel *bool
}

func (h *createGadgetHandler) Handle(ctx *configuration.AppContext, cmd *createGadgetCommand) (*gadgetResult, error) {
	if h.sawIdent != nil {
		if id := ctx.Identity(); id != nil {
			*h.sawIdent = id.Subject
		}
	}
	if h.sawCancel != nil {
		_, ok := ctx.Deadline()
		*h.sawCancel = ok
	}
	if h.fail != nil {
		return nil, h.fail
	}
	if cmd.Name == "" {
		nctx := domain.NewNotificationContext("Gadget")
		nctx.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    "name",
			Notification: domain.RequiredFieldNotification{},
		})
		return nil, domain.NewDomainError([]*domain.NotificationContext{nctx})
	}
	return &gadgetResult{ID: "g-1", Name: cmd.Name}, nil
}

type listGadgetsQuery struct {
	queries.QueryWithParamsBase
	NameContains string
}

func (q listGadgetsQuery) ToCriteria(*configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{}, nil
}

type listGadgetsHandler struct{}

func (listGadgetsHandler) Handle(_ *configuration.AppContext, q *listGadgetsQuery) (queries.Page, error) {
	return queries.Page{Items: []map[string]any{{"Name": q.NameContains}}, TotalCount: 1}, nil
}

func fromGadgetsPage(page queries.Page) (*testpb.ListGadgetsResponse, error) {
	names := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		names = append(names, item["Name"].(string))
	}
	return &testpb.ListGadgetsResponse{Total: page.TotalCount, Names: names}, nil
}

func toCreateCommand(pb *testpb.CreateGadgetRequest) (*createGadgetCommand, error) {
	return &createGadgetCommand{Name: pb.GetName(), Kind: pb.GetKind(), Rating: pb.GetRating()}, nil
}

func fromGadgetResult(r *gadgetResult) (*testpb.CreateGadgetResponse, error) {
	return &testpb.CreateGadgetResponse{Id: r.ID, Name: r.Name}, nil
}

func newCommandFn(h *createGadgetHandler, strict ...string) func(context.Context, *connect.Request[testpb.CreateGadgetRequest]) (*connect.Response[testpb.CreateGadgetResponse], error) {
	return handleCommandWithBody(pipeline.New(nil), toCreateCommand, h, fromGadgetResult, strict)
}

func fullRequest() *connect.Request[testpb.CreateGadgetRequest] {
	return connect.NewRequest(&testpb.CreateGadgetRequest{
		Name: proto.String("Widget"), Kind: proto.String("tool"), Rating: proto.Int32(3),
	})
}

func TestHandleCommandSuccess(t *testing.T) {
	fn := newCommandFn(&createGadgetHandler{})
	res, err := fn(context.Background(), fullRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Msg.GetId() != "g-1" || res.Msg.GetName() != "Widget" {
		t.Fatalf("projection mismatch: %v", res.Msg)
	}
}

func TestHandleCommandDomainFailure(t *testing.T) {
	fn := newCommandFn(&createGadgetHandler{})
	_, err := fn(context.Background(), connect.NewRequest(&testpb.CreateGadgetRequest{
		Name: proto.String(""), Kind: proto.String("tool"), Rating: proto.Int32(1),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	reasons, _, violations := decodeDetails(t, cerr)
	if len(reasons) == 0 || reasons[0] != "RequiredFieldNotification" {
		t.Fatalf("NotificationKey missing: %v", reasons)
	}
	if violations["name"] == "" {
		t.Fatalf("field violation missing: %v", violations)
	}
}

func TestHandleCommandStrictPresence(t *testing.T) {
	fn := newCommandFn(&createGadgetHandler{}, "name", "kind", "rating")

	if _, err := fn(context.Background(), fullRequest()); err != nil {
		t.Fatalf("all present must pass: %v", err)
	}

	_, err := fn(context.Background(), connect.NewRequest(&testpb.CreateGadgetRequest{
		Name: proto.String("OnlyName"),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	_, _, violations := decodeDetails(t, cerr)
	if _, ok := violations["kind"]; !ok {
		t.Fatalf("'kind' must be flagged: %v", violations)
	}
	if _, ok := violations["rating"]; !ok {
		t.Fatalf("'rating' must be flagged: %v", violations)
	}
	if _, ok := violations["name"]; ok {
		t.Fatalf("'name' was present: %v", violations)
	}
}

func TestHandleCommandStrictUnknownFieldIsMissing(t *testing.T) {
	fn := newCommandFn(&createGadgetHandler{}, "nonexistent")
	_, err := fn(context.Background(), fullRequest())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("unknown strict field must reject loudly, got %v", err)
	}
}

func TestHandleCommandConversionPlainError(t *testing.T) {
	fn := handleCommandWithBody(pipeline.New(nil),
		func(*testpb.CreateGadgetRequest) (*createGadgetCommand, error) {
			return nil, errors.New("cannot map")
		},
		&createGadgetHandler{}, fromGadgetResult, nil)
	_, err := fn(context.Background(), fullRequest())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	reasons, _, _ := decodeDetails(t, cerr)
	if len(reasons) == 0 || reasons[0] != "SchemaViolationNotification" {
		t.Fatalf("plain conversion error must map to SchemaViolation: %v", reasons)
	}
}

func TestHandleCommandConversionCarrierKeepsSemantic(t *testing.T) {
	fn := handleCommandWithBody(pipeline.New(nil),
		func(*testpb.CreateGadgetRequest) (*createGadgetCommand, error) {
			nctx := domain.NewNotificationContext("Gadget")
			nctx.AddNotificationMessage(domain.NotificationMessage{
				Notification: domain.EntityIsNotActiveNotification{},
			})
			return nil, domain.NewDomainError([]*domain.NotificationContext{nctx})
		},
		&createGadgetHandler{}, fromGadgetResult, nil)
	_, err := fn(context.Background(), fullRequest())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("carrier semantics must survive: %v", err)
	}
}

func TestHandleCommandExceptionIsOpaque(t *testing.T) {
	fn := newCommandFn(&createGadgetHandler{fail: errors.New("pg exploded: secret dsn")})
	_, err := fn(context.Background(), fullRequest())
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInternal {
		t.Fatalf("want Internal, got %v", err)
	}
	if cerr.Message() != "internal server error" {
		t.Fatalf("exception must not leak: %q", cerr.Message())
	}
}

func TestHandleCommandPanicIsOpaque(t *testing.T) {
	// pipeline.Dispatch's own recover converts the panic to an Exception
	// result — the wrapper then renders the opaque INTERNAL.
	h := &createGadgetHandler{}
	fn := handleCommandWithBody(pipeline.New(nil),
		func(*testpb.CreateGadgetRequest) (*createGadgetCommand, error) { panicNow(); return nil, nil },
		h, fromGadgetResult, nil)
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panic escaped the wrapper: %v", p)
		}
	}()
	// toCommand panics OUTSIDE Dispatch — the registry's recovery
	// interceptor owns that layer; direct invocation must propagate so the
	// interceptor can see it. Assert it panics.
	didPanic := false
	func() {
		defer func() { didPanic = recover() != nil }()
		_, _ = fn(context.Background(), fullRequest())
	}()
	if !didPanic {
		t.Fatalf("toCommand panic must reach the recovery interceptor layer")
	}
}

func panicNow() { panic("boom") }

func TestHandleQuerySuccessAndConversion(t *testing.T) {
	pipe := pipeline.New(nil)
	fn := handleQueryWithParams(pipe,
		func(pb *testpb.ListGadgetsRequest) (*listGadgetsQuery, error) {
			return &listGadgetsQuery{NameContains: pb.GetNameContains()}, nil
		},
		listGadgetsHandler{},
		fromGadgetsPage)
	res, err := fn(context.Background(), connect.NewRequest(&testpb.ListGadgetsRequest{
		NameContains: proto.String("Drill"),
	}))
	if err != nil || res.Msg.GetNames()[0] != "Drill" {
		t.Fatalf("query flow: res=%v err=%v", res, err)
	}

	failing := handleQueryWithParams(pipe,
		func(*testpb.ListGadgetsRequest) (*listGadgetsQuery, error) { return nil, errors.New("bad") },
		listGadgetsHandler{},
		func(queries.Page) (*testpb.ListGadgetsResponse, error) { return &testpb.ListGadgetsResponse{}, nil })
	_, err = failing(context.Background(), connect.NewRequest(&testpb.ListGadgetsRequest{}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("query conversion error: %v", err)
	}
}

// --- WithBodyID / ByID / QueryByID family ---

type updateGadgetCommand struct {
	pipeline.CommandByIDBase
	Name string
}

type updateGadgetHandler struct{ sawID *string }

func (h *updateGadgetHandler) Handle(_ *configuration.AppContext, cmd *updateGadgetCommand) (*gadgetResult, error) {
	if h.sawID != nil {
		*h.sawID = cmd.PathID()
	}
	if cmd.PathID() == "" {
		return nil, errors.New("no id reached the handler")
	}
	return &gadgetResult{ID: cmd.PathID(), Name: cmd.Name}, nil
}

func TestHandleCommandWithBodyIDInjectsID(t *testing.T) {
	var sawID string
	fn := handleCommandWithBodyID(pipeline.New(nil),
		(*testpb.UpdateGadgetRequest).GetId, // the generated getter, straight in
		func(pb *testpb.UpdateGadgetRequest) (*updateGadgetCommand, error) {
			return &updateGadgetCommand{Name: pb.GetName()}, nil
		},
		&updateGadgetHandler{sawID: &sawID},
		fromGadgetResult,
		nil,
	)
	res, err := fn(context.Background(), connect.NewRequest(&testpb.UpdateGadgetRequest{
		Id: proto.String("g-77"), Name: proto.String("Renamed"),
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if sawID != "g-77" || res.Msg.GetId() != "g-77" || res.Msg.GetName() != "Renamed" {
		t.Fatalf("SetPathID seam broken: sawID=%q res=%v", sawID, res.Msg)
	}
}

func TestHandleCommandWithBodyIDStrict(t *testing.T) {
	fn := handleCommandWithBodyID(pipeline.New(nil),
		(*testpb.UpdateGadgetRequest).GetId,
		func(pb *testpb.UpdateGadgetRequest) (*updateGadgetCommand, error) {
			return &updateGadgetCommand{Name: pb.GetName()}, nil
		},
		&updateGadgetHandler{},
		fromGadgetResult,
		[]string{"id", "name", "kind"},
	)
	_, err := fn(context.Background(), connect.NewRequest(&testpb.UpdateGadgetRequest{
		Id: proto.String("g-1"),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("strict must flag missing fields: %v", err)
	}
	_, _, violations := decodeDetails(t, cerr)
	if _, ok := violations["name"]; !ok {
		t.Fatalf("'name' must be flagged: %v", violations)
	}
}

type archiveGadgetCommand struct {
	pipeline.CommandByIDBase
}

type archiveGadgetHandler struct{}

func (archiveGadgetHandler) Handle(_ *configuration.AppContext, cmd *archiveGadgetCommand) (*gadgetResult, error) {
	if cmd.PathID() == "legacy" {
		nctx := domain.NewNotificationContext("Gadget")
		nctx.AddNotificationMessage(domain.NotificationMessage{
			Notification: domain.EntityIsNotActiveNotification{},
		})
		return nil, domain.NewDomainError([]*domain.NotificationContext{nctx})
	}
	return &gadgetResult{ID: cmd.PathID()}, nil
}

func TestHandleCommandByIDNoMapper(t *testing.T) {
	fn := handleCommandByID[testpb.ArchiveGadgetRequest, testpb.CreateGadgetResponse, archiveGadgetCommand](
		pipeline.New(nil),
		(*testpb.ArchiveGadgetRequest).GetId,
		archiveGadgetHandler{},
		fromGadgetResult,
	)
	res, err := fn(context.Background(), connect.NewRequest(&testpb.ArchiveGadgetRequest{
		Id: proto.String("g-9"),
	}))
	if err != nil || res.Msg.GetId() != "g-9" {
		t.Fatalf("byID flow: res=%v err=%v", res, err)
	}

	_, err = fn(context.Background(), connect.NewRequest(&testpb.ArchiveGadgetRequest{
		Id: proto.String("legacy"),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("byID failure mapping: %v", err)
	}
}

type getGadgetQuery struct {
	queries.QueryByIDBase
	IncludeArchived bool
}

func (q getGadgetQuery) ToCriteria(*configuration.AppContext) (queries.ReadCriteria, error) {
	return queries.ReadCriteria{IncludeArchived: q.IncludeArchived}, nil
}

func (getGadgetQuery) ContextName() string { return "Gadget" }

type getGadgetHandler struct{}

func (getGadgetHandler) Handle(ctx *configuration.AppContext, q *getGadgetQuery) (map[string]any, error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return nil, err
	}
	if q.PathID().String() == "missing" {
		nctx := domain.NewNotificationContext(q.ContextName())
		nctx.AddNotificationMessage(domain.NotificationMessage{
			Notification: domain.RecordNotFoundNotification{},
		})
		return nil, domain.NewDomainError([]*domain.NotificationContext{nctx})
	}
	return map[string]any{
		"ID":              q.PathID().String(),
		"Name":            "Found Gadget",
		"IncludeArchived": crit.IncludeArchived,
	}, nil
}

func TestHandleQueryByID(t *testing.T) {
	fn := handleQueryByID(pipeline.New(nil),
		(*testpb.GetGadgetRequest).GetId,
		func(pb *testpb.GetGadgetRequest) (*getGadgetQuery, error) {
			return &getGadgetQuery{IncludeArchived: pb.GetIncludeArchived()}, nil
		},
		getGadgetHandler{},
		func(doc map[string]any) (*testpb.GetGadgetResponse, error) {
			return &testpb.GetGadgetResponse{
				Id:   doc["ID"].(string),
				Name: doc["Name"].(string),
			}, nil
		},
	)

	res, err := fn(context.Background(), connect.NewRequest(&testpb.GetGadgetRequest{
		Id: proto.String("g-42"), IncludeArchived: true,
	}))
	if err != nil || res.Msg.GetId() != "g-42" || res.Msg.GetName() != "Found Gadget" {
		t.Fatalf("get flow: res=%v err=%v", res, err)
	}

	_, err = fn(context.Background(), connect.NewRequest(&testpb.GetGadgetRequest{
		Id: proto.String("missing"),
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("not-found mapping: %v", err)
	}

	failing := handleQueryByID(pipeline.New(nil),
		(*testpb.GetGadgetRequest).GetId,
		func(*testpb.GetGadgetRequest) (*getGadgetQuery, error) { return nil, errors.New("bad") },
		getGadgetHandler{},
		func(map[string]any) (*testpb.GetGadgetResponse, error) { return &testpb.GetGadgetResponse{}, nil },
	)
	_, err = failing(context.Background(), connect.NewRequest(&testpb.GetGadgetRequest{Id: proto.String("x")}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("query conversion error: %v", err)
	}
}

func TestFailureErrorNonCarrierIsOpaque(t *testing.T) {
	// defensive branch: failureError fed a plain error (pipeline.Run turns
	// it into an Exception) must render the opaque INTERNAL.
	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	appCtx.SetParent(context.Background())
	err := failureError[int](pipeline.New(nil), appCtx, errors.New("not a carrier"))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInternal {
		t.Fatalf("want opaque Internal, got %v", err)
	}
}

func TestHandleCommandWithBodyIDConversionError(t *testing.T) {
	fn := handleCommandWithBodyID(pipeline.New(nil),
		(*testpb.UpdateGadgetRequest).GetId,
		func(*testpb.UpdateGadgetRequest) (*updateGadgetCommand, error) {
			return nil, errors.New("cannot map")
		},
		&updateGadgetHandler{},
		fromGadgetResult,
		nil,
	)
	_, err := fn(context.Background(), connect.NewRequest(&testpb.UpdateGadgetRequest{Id: proto.String("g-1")}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("WithBodyID conversion error: %v", err)
	}
}
