package grpc

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
	omnicorepb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	fwresponses "github.com/ClaudioSchirmer/omnicore/web/responses"
)

// bareGadgetsDTO declares ONLY filter leaves — no reserved control keys.
// The shared proto still shows every control field; the canonical gateway
// must reject each one at runtime because this endpoint's DTO does not
// declare it. The DTO is the single source of truth for what an endpoint
// exposes — on gRPC the cut is a runtime INVALID_ARGUMENT (the proto is
// shared, so the contract cannot narrow per RPC).
type bareGadgetsDTO struct {
	Name      *string  `query:"name"      filter:"eq,icontains"`
	Rating    *int64   `query:"rating"    filter:"gte,lte"`
	Price     *float64 `query:"price"     filter:"gt,lt"`
	Active    *bool    `query:"active"    filter:"eq"`
	CreatedAt *string  `query:"createdAt" filter:"gte"`
}

func (bareGadgetsDTO) ToQuery(c queries.ReadCriteria) *searchGadgetsQuery {
	return &searchGadgetsQuery{Criteria: c}
}

// TestReservedGate_UndeclaredControlsReject proves the DTO opt-in gate on
// the gRPC wire for EVERY control the shared PaginationRequest / order_by /
// fields components carry: set on the wire without the matching `query:"…"`
// declaration → INVALID_ARGUMENT, never a silent honor.
func TestReservedGate_UndeclaredControlsReject(t *testing.T) {
	reg := New(pipeline.New(nil))
	var saw queries.ReadCriteria
	reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		"/omnicore.grpctest.v1.GadgetService/SearchGadgetsBare",
		bareGadgetsDTO{},
		fwresponses.AutoFromDoc[gadgetItemDTO],
		searchGadgetsHandler{sawCriteria: &saw},
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		srv.Client(), srv.URL+"/omnicore.grpctest.v1.GadgetService/SearchGadgetsBare")

	cases := []struct {
		name string
		req  *testpb.SearchGadgetsRequest
	}{
		{"first", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{First: proto.Int64(5)}}},
		{"last", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{Last: proto.Int64(5)}}},
		{"after", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{After: proto.String("c")}}},
		{"before", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{Before: proto.String("c")}}},
		{"only_total", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{OnlyTotal: proto.Bool(true)}}},
		{"include_archived", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{IncludeArchived: proto.Bool(true)}}},
		// The bools are proto3 optional too: an explicitly-set FALSE is a
		// presence exactly like REST's `?onlyTotal=false` — the opt-in gate
		// polices presence, activation is a separate axis.
		{"only_total_false", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{OnlyTotal: proto.Bool(false)}}},
		{"include_archived_false", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{IncludeArchived: proto.Bool(false)}}},
		{"search", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{Search: proto.String("x")}}},
		// An explicitly-set EMPTY search is still a presence on the optional
		// field — gated like REST's `?search=`, never read as absent.
		{"search_empty", &testpb.SearchGadgetsRequest{Pagination: &omnicorepb.PaginationRequest{Search: proto.String("")}}},
		{"order_by", &testpb.SearchGadgetsRequest{OrderBy: []*omnicorepb.OrderByField{{Field: "name"}}}},
		{"fields", &testpb.SearchGadgetsRequest{Fields: &fieldmaskpb.FieldMask{Paths: []string{"name"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.CallUnary(context.Background(), connect.NewRequest(tc.req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("%s without DTO declaration must reject as INVALID_ARGUMENT, got %v", tc.name, err)
			}
		})
	}

	// A filter-only request still passes — the gate polices controls, not filters.
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Filters: &testpb.SearchGadgetsFilters{
			Name: &omnicorepb.StringFilter{Conditions: []*omnicorepb.StringCondition{{
				Op: omnicorepb.StringOp_STRING_OP_EQ, Values: []string{"Drill"},
			}}},
		},
	})); err != nil {
		t.Fatalf("filter-only request must pass the gate: %v", err)
	}
}

// TestReservedGate_DirectionPair proves the Relay direction semantics on the
// declared DTO: `last` alone maps to a backward window (Limit + Backward),
// and a forward+backward mix rejects with the gateway's directional rule.
func TestReservedGate_DirectionPair(t *testing.T) {
	reg := New(pipeline.New(nil))
	var saw queries.ReadCriteria
	reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		"/omnicore.grpctest.v1.GadgetService/SearchGadgetsDir",
		searchGadgetsDTO{},
		fwresponses.AutoFromDoc[gadgetItemDTO],
		searchGadgetsHandler{sawCriteria: &saw},
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	client := connect.NewClient[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		srv.Client(), srv.URL+"/omnicore.grpctest.v1.GadgetService/SearchGadgetsDir")

	// last alone = the LAST N of the set.
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Pagination: &omnicorepb.PaginationRequest{Last: proto.Int64(3)},
	})); err != nil {
		t.Fatalf("last alone must pass: %v", err)
	}
	if saw.Limit != 3 || !saw.Backward {
		t.Fatalf("last=3 must map to Limit=3 Backward=true, got %+v", saw)
	}

	// forward + backward mix → directional violation.
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Pagination: &omnicorepb.PaginationRequest{First: proto.Int64(2), Last: proto.Int64(3)},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("first+last must reject as INVALID_ARGUMENT, got %v", err)
	}

	// non-positive size → gateway size violation.
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Pagination: &omnicorepb.PaginationRequest{First: proto.Int64(0)},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("first=0 must reject as INVALID_ARGUMENT, got %v", err)
	}
}
