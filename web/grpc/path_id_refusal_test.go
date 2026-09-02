package grpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
)

// The same refusal the other two surfaces answer, in this wire's idiom: the
// semantic maps to a Connect code, so a read is NotFound and a write is
// InvalidArgument. Before, both were CodeInternal — the driver's error with no
// notification to classify it.

func codeOf(t *testing.T, err error) connect.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *connect.Error, got %T", err)
	}
	return cerr.Code()
}

func TestHandleQueryByID_MalformedIDIsNotFound(t *testing.T) {
	fn := handleQueryByID(pipeline.New(nil),
		(*testpb.GetGadgetRequest).GetId,
		func(pb *testpb.GetGadgetRequest) (*getGadgetQuery, error) {
			return &getGadgetQuery{Criteria: queries.ReadCriteria{}}, nil
		},
		getGadgetHandler{},
		func(r gadgetDetailResult) (*testpb.GetGadgetResponse, error) {
			return &testpb.GetGadgetResponse{Id: r.ID, Name: r.Name}, nil
		},
	)

	_, err := fn(context.Background(), connect.NewRequest(&testpb.GetGadgetRequest{
		Id: proto.String("not-a-uuid"),
	}))
	if got := codeOf(t, err); got != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", got)
	}
}

func TestHandleCommandByID_MalformedIDIsInvalidArgument(t *testing.T) {
	fn := handleCommandByID[testpb.ArchiveGadgetRequest, testpb.CreateGadgetResponse, archiveGadgetCommand](
		pipeline.New(nil),
		(*testpb.ArchiveGadgetRequest).GetId,
		archiveGadgetHandler{},
		fromGadgetResult,
	)

	_, err := fn(context.Background(), connect.NewRequest(&testpb.ArchiveGadgetRequest{
		Id: proto.String("not-a-uuid"),
	}))
	if got := codeOf(t, err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
}
