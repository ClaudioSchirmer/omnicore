package grpc

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
	omnicorepb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
)

// COMPUTED-field parity on the gRPC wire. A computed field is declared on the
// RESPONSE DTO (`computed:"A,B"`, naming Result fields) and has no column
// behind it — the Query's FromQueryResult derives it after the read. The two
// consumers of the read vocabulary therefore diverge exactly as they do on
// REST: read_mask pushes the SOURCES down (the `?fields=` pushdown) and
// order_by is refused with ComputedFieldNotSortableNotification (the
// `?orderBy=` 400, INVALID_ARGUMENT here).

// computedGadgetItemDTO is the gadget list Response DTO with `kind` declared
// COMPUTED over the Result's Name+ID — the wire shape stays identical to
// gadgetItemDTO (the item proto is unchanged), only the derivation contract
// differs.
type computedGadgetItemDTO struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Kind *string `json:"kind,omitempty" computed:"Name,ID"`
}

func (computedGadgetItemDTO) FromResult(r gadgetSearchResult) computedGadgetItemDTO {
	return computedGadgetItemDTO{ID: r.ID, Name: r.Name, Kind: r.Kind}
}

// TestComputedFieldMaskPushesSources — a read_mask entry naming a computed
// field contributes its declared SOURCES to the projection, never its own Go
// path (which resolves to no column). A source that is absent from the
// vocabulary is still pushed: sources are Result fields, optional on the
// Response.
func TestComputedFieldMaskPushesSources(t *testing.T) {
	crit, err := NewCriteria().
		Fields(map[string]string{"id": "ID", "name": "Name", "display": "Display"}).
		ComputedFields(map[string][]string{"display": {"Name", "UserName"}}).
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"display"}}).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !reflect.DeepEqual(crit.Projection, map[string]int{"Name": 1, "UserName": 1}) {
		t.Fatalf("read_mask over a computed field must push its sources, got %+v", crit.Projection)
	}

	// Mixed mask: the stored path lands verbatim, the computed one expands.
	crit, err = NewCriteria().
		Fields(map[string]string{"id": "ID", "name": "Name", "display": "Display"}).
		ComputedFields(map[string][]string{"display": {"Name", "UserName"}}).
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"id", "display"}}).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !reflect.DeepEqual(crit.Projection, map[string]int{"ID": 1, "Name": 1, "UserName": 1}) {
		t.Fatalf("mixed mask: %+v", crit.Projection)
	}
}

// TestComputedSortIsRefused — order_by over a computed field is a wire-
// contract violation carrying the typed ComputedFieldNotSortableNotification
// (semantic Schema), reported under the sort entry as the wire spelled it.
// One offending entry refuses the whole request, exactly like the REST
// `?orderBy=name,display` 400.
func TestComputedSortIsRefused(t *testing.T) {
	_, err := NewCriteria().
		Fields(map[string]string{"name": "Name", "display": "Display"}).
		ComputedFields(map[string][]string{"display": {"Name", "UserName"}}).
		OrderBy(&omnicorepb.OrderByField{Field: "name"}, &omnicorepb.OrderByField{Field: "display", Desc: true}).
		Build()
	if err == nil {
		t.Fatal("ordering by a computed field must fail Build")
	}
	if got := violationFields(t, err); len(got) != 1 || got[0] != "display" {
		t.Fatalf("the refusal must name the sort entry, got %v", got)
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected a NotificationCarrier, got %T", err)
	}
	msgs := carrier.NotificationContexts()[0].Messages()
	if _, typed := msgs[0].Notification.(domain.ComputedFieldNotSortableNotification); !typed {
		t.Fatalf("the refusal must be typed, got %T", msgs[0].Notification)
	}
	if msgs[0].Notification.Semantic() != domain.SemanticSchema {
		t.Fatalf("semantic: %v", msgs[0].Notification.Semantic())
	}
}

// TestComputedPlanCarriesTheComputedCut — compileQueryPlan records the
// computed subset of the vocabulary on the plan itself, keyed by the PROTO
// wire path, so neither consumer re-derives it from the Response at request
// time.
func TestComputedPlanCarriesTheComputedCut(t *testing.T) {
	plan, err := compileQueryPlan("t",
		(&testpb.SearchGadgetsRequest{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(searchGadgetsDTO{}),
		(&testpb.GadgetItem{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(computedGadgetItemDTO{}), nil)
	if err != nil {
		t.Fatalf("compileQueryPlan: %v", err)
	}
	if plan.fields["kind"] != "Kind" {
		t.Fatalf("a computed field stays in the vocabulary (it is selectable): %+v", plan.fields)
	}
	if !reflect.DeepEqual(plan.computed, map[string][]string{"kind": {"Name", "ID"}}) {
		t.Fatalf("plan.computed: %+v", plan.computed)
	}
}

// newComputedGadgetClient wires the Auto list procedure over the computed
// Response DTO and returns a live Connect client for it.
func newComputedGadgetClient(t *testing.T, procedure string, saw *queries.ReadCriteria) *connect.Client[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse] {
	t.Helper()
	reg := New(pipeline.New(nil))
	reg.Register(QueryWithParams[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](
		procedure,
		searchGadgetsDTO{},
		computedGadgetItemDTO{}.FromResult,
		searchGadgetsHandler{sawCriteria: saw},
	))
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	return connect.NewClient[testpb.SearchGadgetsRequest, testpb.SearchGadgetsResponse](srv.Client(), srv.URL+procedure)
}

// TestComputedAutoPath_MaskPushesSources — the compiled Auto path carries the
// computed cut end to end: a fields mask naming the computed proto field
// reaches the handler as a projection over the SOURCES.
func TestComputedAutoPath_MaskPushesSources(t *testing.T) {
	var saw queries.ReadCriteria
	client := newComputedGadgetClient(t, "/omnicore.grpctest.v1.GadgetService/SearchGadgetsComputedMask", &saw)
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Fields: &fieldmaskpb.FieldMask{Paths: []string{"kind"}},
	})); err != nil {
		t.Fatalf("selecting a computed field must pass: %v", err)
	}
	if !reflect.DeepEqual(saw.Projection, map[string]int{"Name": 1, "ID": 1}) {
		t.Fatalf("read_mask over a computed field must push its sources, got %+v", saw.Projection)
	}
}

// TestComputedAutoPath_SortRejectsWithNotification — the same plan refuses a
// sort over that field as INVALID_ARGUMENT, with the canonical envelope in
// google.rpc details: reason=ComputedFieldNotSortableNotification,
// semantic=Schema, field=the sort entry.
func TestComputedAutoPath_SortRejectsWithNotification(t *testing.T) {
	var saw queries.ReadCriteria
	client := newComputedGadgetClient(t, "/omnicore.grpctest.v1.GadgetService/SearchGadgetsComputedSort", &saw)
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		OrderBy: []*omnicorepb.OrderByField{{Field: "kind"}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("sorting by a computed field must reject as INVALID_ARGUMENT, got %v", err)
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a *connect.Error, got %T", err)
	}
	var sawReason bool
	for _, d := range cerr.Details() {
		v, derr := d.Value()
		if derr != nil {
			t.Fatalf("detail decode: %v", derr)
		}
		info, ok := v.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}
		if info.GetReason() != "ComputedFieldNotSortableNotification" {
			t.Fatalf("reason: %q", info.GetReason())
		}
		if got := info.GetMetadata()["field"]; got != "kind" {
			t.Fatalf("the detail must name the sort entry, got %q", got)
		}
		if got := info.GetMetadata()["semantic"]; got != "Schema" {
			t.Fatalf("semantic: %q", got)
		}
		sawReason = true
	}
	if !sawReason {
		t.Fatal("the rejection must carry a google.rpc ErrorInfo detail")
	}
	// The handler is never reached: the refusal happens at criteria build.
	if saw.OrderBy != nil {
		t.Fatalf("the handler must not run: %+v", saw)
	}

	// A stored path in the same vocabulary still sorts.
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		OrderBy: []*omnicorepb.OrderByField{{Field: "name"}},
	})); err != nil {
		t.Fatalf("sorting by a stored path must pass: %v", err)
	}
	if len(saw.OrderBy) != 1 || saw.OrderBy[0].Field != "Name" {
		t.Fatalf("stored sort: %+v", saw.OrderBy)
	}
}
