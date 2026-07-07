package grpc

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	pb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

func TestCriteriaPageSortReadMask(t *testing.T) {
	after := "cursor-a"
	limit := int64(25)
	fields := map[string]string{"id": "ID", "name": "Name", "created_at": "CreatedAt"}
	crit, err := NewCriteria().
		Fields(fields).
		Page(&pb.PageRequest{After: &after, Limit: &limit, OnlyTotal: true, IncludeArchived: true}).
		Sort(&pb.SortField{Field: "name"}, &pb.SortField{Field: "created_at", Desc: true}, nil, &pb.SortField{}).
		ReadMask(&fieldmaskpb.FieldMask{Paths: []string{"id", "name", ""}}).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if crit.After != "cursor-a" || crit.Limit != 25 || !crit.OnlyTotal || !crit.IncludeArchived {
		t.Fatalf("page: %+v", crit)
	}
	// wire names resolve to GO FIELD PATHS — the spelling ToCriteria
	// overlays (Restrict) operate on, closing the physical-column bypass
	wantSort := []queries.SortField{{Field: "Name"}, {Field: "CreatedAt", Desc: true}}
	if !reflect.DeepEqual(crit.Sort, wantSort) {
		t.Fatalf("sort: %+v", crit.Sort)
	}
	if !reflect.DeepEqual(crit.Projection, map[string]int{"ID": 1, "Name": 1}) {
		t.Fatalf("projection: %+v", crit.Projection)
	}
}

func TestCriteriaStringParityWithRESTEmitter(t *testing.T) {
	// The builder must produce byte-identical clauses to the query-string
	// path — same emitter, same output.
	spec := queryschema.FilterSpec{DocPath: "Name", GoKind: reflect.String}

	viaREST := map[string]any{}
	queryschema.ApplyFilterParam(viaREST, spec, queryschema.OpContains, "Dri.ll")

	crit, err := NewCriteria().String("Name", &pb.StringFilter{Conditions: []*pb.StringCondition{
		{Op: pb.StringOp_STRING_OP_CONTAINS, Values: []string{"Dri.ll"}},
	}}).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !reflect.DeepEqual(crit.Filter, viaREST) {
		t.Fatalf("parity broken:\n grpc=%#v\n rest=%#v", crit.Filter, viaREST)
	}
}

func TestCriteriaStringInKeepsCommaValues(t *testing.T) {
	// The proto plane has no comma-in-value limitation: repeated values ride
	// verbatim into $in.
	crit, err := NewCriteria().String("Name", &pb.StringFilter{Conditions: []*pb.StringCondition{
		{Op: pb.StringOp_STRING_OP_IN, Values: []string{"Smith, John", "Doe, Jane"}},
	}}).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	in := crit.Filter["Name"].(map[string]any)["$in"].([]any)
	if len(in) != 2 || in[0] != "Smith, John" || in[1] != "Doe, Jane" {
		t.Fatalf("comma values mangled: %#v", in)
	}
}

func TestCriteriaMultiConditionBecomesMultiClause(t *testing.T) {
	crit, err := NewCriteria().Int64("Rating", &pb.Int64Filter{Conditions: []*pb.Int64Condition{
		{Op: pb.NumberOp_NUMBER_OP_GTE, Values: []int64{2}},
		{Op: pb.NumberOp_NUMBER_OP_LTE, Values: []int64{5}},
	}}).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	mc, ok := crit.Filter["Rating"].(queries.MultiClause)
	if !ok || len(mc.Clauses) != 2 {
		t.Fatalf("want MultiClause with 2 clauses: %#v", crit.Filter["Rating"])
	}
	if !reflect.DeepEqual(mc.Clauses[0], map[string]any{"$gte": int64(2)}) {
		t.Fatalf("gte clause: %#v", mc.Clauses[0])
	}
	if !reflect.DeepEqual(mc.Clauses[1], map[string]any{"$lte": int64(5)}) {
		t.Fatalf("lte clause: %#v", mc.Clauses[1])
	}
}

func TestCriteriaDoubleBoolTimestamp(t *testing.T) {
	when := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	crit, err := NewCriteria().
		Double("Score", &pb.DoubleFilter{Conditions: []*pb.DoubleCondition{
			{Op: pb.NumberOp_NUMBER_OP_GT, Values: []float64{4.5}},
		}}).
		Bool("Active", &pb.BoolFilter{Conditions: []*pb.BoolCondition{
			{Op: pb.BoolOp_BOOL_OP_EQ, Value: true},
		}}).
		Timestamp("CreatedAt", &pb.TimestampFilter{Conditions: []*pb.TimestampCondition{
			{Op: pb.NumberOp_NUMBER_OP_GTE, Values: []*timestamppb.Timestamp{timestamppb.New(when), nil}},
		}}).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !reflect.DeepEqual(crit.Filter["Score"], map[string]any{"$gt": 4.5}) {
		t.Fatalf("double: %#v", crit.Filter["Score"])
	}
	if crit.Filter["Active"] != true {
		t.Fatalf("bool eq must land as scalar: %#v", crit.Filter["Active"])
	}
	want := map[string]any{"$gte": when.Format(time.RFC3339Nano)}
	if !reflect.DeepEqual(crit.Filter["CreatedAt"], want) {
		t.Fatalf("timestamp: %#v", crit.Filter["CreatedAt"])
	}
}

func TestCriteriaCaseInsensitiveListOps(t *testing.T) {
	crit, err := NewCriteria().String("Kind", &pb.StringFilter{Conditions: []*pb.StringCondition{
		{Op: pb.StringOp_STRING_OP_IIN, Values: []string{"Tool", "Toy"}},
	}}).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rml, ok := crit.Filter["Kind"].(queries.RegexMatchList)
	if !ok || !rml.CaseInsensitive || rml.Negate {
		t.Fatalf("iin: %#v", crit.Filter["Kind"])
	}
	if len(rml.Patterns) != 2 || rml.Patterns[0] != "^Tool$" {
		t.Fatalf("anchoring: %#v", rml.Patterns)
	}
}

func TestCriteriaInvalidOpsFailBuild(t *testing.T) {
	_, err := NewCriteria().
		String("Name", &pb.StringFilter{Conditions: []*pb.StringCondition{{Op: pb.StringOp_STRING_OP_UNSPECIFIED, Values: []string{"x"}}}}).
		Int64("Rating", &pb.Int64Filter{Conditions: []*pb.Int64Condition{{Op: pb.NumberOp_NUMBER_OP_UNSPECIFIED, Values: []int64{1}}}}).
		Bool("Active", &pb.BoolFilter{Conditions: []*pb.BoolCondition{{Op: pb.BoolOp_BOOL_OP_UNSPECIFIED}}}).
		Timestamp("CreatedAt", &pb.TimestampFilter{Conditions: []*pb.TimestampCondition{{Op: pb.NumberOp_NUMBER_OP_UNSPECIFIED}}}).
		Double("Score", &pb.DoubleFilter{Conditions: []*pb.DoubleCondition{{Op: pb.NumberOp_NUMBER_OP_UNSPECIFIED}}}).
		Build()
	if err == nil || !strings.Contains(err.Error(), "grpc criteria") {
		t.Fatalf("unspecified ops must fail Build: %v", err)
	}
}

func TestCriteriaNilFiltersAreNoOps(t *testing.T) {
	crit, err := NewCriteria().
		Page(nil).ReadMask(nil).
		String("A", nil).Int64("B", nil).Double("C", nil).Bool("D", nil).Timestamp("E", nil).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(crit.Filter) != 0 || crit.Projection != nil {
		t.Fatalf("no-ops leaked: %+v", crit)
	}
}


func TestCriteriaUndeclaredMaskAndSortFailBuild(t *testing.T) {
	// The Fields vocabulary IS the allowlist: an undeclared read_mask or
	// sort path fails Build (→ SchemaViolation at the wrapper), so a raw
	// physical-column spelling can never reach the reader and bypass
	// ToCriteria overlays such as Restrict.
	_, err := NewCriteria().
		Fields(map[string]string{"id": "ID"}).
		ReadMask(&fieldmaskpb.FieldMask{Paths: []string{"phone"}}).
		Build()
	if err == nil || !strings.Contains(err.Error(), "read_mask") {
		t.Fatalf("undeclared mask path must fail: %v", err)
	}
	_, err = NewCriteria().
		Fields(map[string]string{"id": "ID"}).
		Sort(&pb.SortField{Field: "phone"}).
		Build()
	if err == nil || !strings.Contains(err.Error(), "sort") {
		t.Fatalf("undeclared sort field must fail: %v", err)
	}
	// no Fields declared at all → mask/sort unsupported for the view
	_, err = NewCriteria().
		ReadMask(&fieldmaskpb.FieldMask{Paths: []string{"id"}}).
		Build()
	if err == nil {
		t.Fatalf("mask without a declared vocabulary must fail")
	}
}
