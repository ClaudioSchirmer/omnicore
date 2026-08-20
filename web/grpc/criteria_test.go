package grpc

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	pb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

func TestCriteriaPageSortReadMask(t *testing.T) {
	after := "cursor-a"
	limit := int64(25)
	search := "drill"
	fields := map[string]string{"id": "ID", "name": "Name", "created_at": "CreatedAt"}
	sortable := map[string]queryschema.SortSpec{
		"name":       {GoPath: "Name", Asc: true, Desc: true},
		"created_at": {GoPath: "CreatedAt", Asc: true, Desc: true},
	}
	crit, err := NewCriteria().
		Fields(fields).
		Sortable(sortable).
		Page(&pb.PaginationRequest{After: &after, First: &limit, IncludeArchived: proto.Bool(true), Search: &search}).
		OrderBy(&pb.OrderByField{Field: "name"}, &pb.OrderByField{Field: "created_at", Desc: true}, nil, &pb.OrderByField{}).
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"id", "name", ""}}).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if crit.After != "cursor-a" || crit.Limit != 25 || !crit.IncludeArchived || crit.Search != "drill" {
		t.Fatalf("page: %+v", crit)
	}
	// wire names resolve to GO FIELD PATHS — the spelling ToCriteria
	// overlays (Restrict) operate on, closing the physical-column bypass
	wantSort := []queries.OrderByField{{Field: "Name"}, {Field: "CreatedAt", Desc: true}}
	if !reflect.DeepEqual(crit.OrderBy, wantSort) {
		t.Fatalf("sort: %+v", crit.OrderBy)
	}
	if !reflect.DeepEqual(crit.Projection, map[string]int{"ID": 1, "Name": 1}) {
		t.Fatalf("projection: %+v", crit.Projection)
	}
}

// violationFields unwraps a gateway rejection (the framework's typed
// notification error) into the per-message field names, in order.
func violationFields(t *testing.T, err error) []string {
	t.Helper()
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected a NotificationCarrier, got %T: %v", err, err)
	}
	var fields []string
	for _, nctx := range carrier.NotificationContexts() {
		for _, msg := range nctx.Messages() {
			fields = append(fields, msg.FieldName)
		}
	}
	return fields
}

// TestCriteriaOnlyTotalConflicts proves the REST conflict matrix on the
// gRPC wire: only_total=true combined with any page-shaping control is a
// wire-contract violation, never a silent ignore — the same 400 the REST
// wrapper emits for ?onlyTotal=true&first=….
func TestCriteriaOnlyTotalConflicts(t *testing.T) {
	after := "c"
	limit := int64(5)
	fields := map[string]string{"name": "Name"}

	cases := []struct {
		name string
		req  *pb.PaginationRequest
		add  func(*CriteriaBuilder) *CriteriaBuilder
		want string
	}{
		{"after", &pb.PaginationRequest{OnlyTotal: proto.Bool(true), After: &after}, nil, "onlyTotal[after]"},
		{"before", &pb.PaginationRequest{OnlyTotal: proto.Bool(true), Before: &after}, nil, "onlyTotal[before]"},
		{"first", &pb.PaginationRequest{OnlyTotal: proto.Bool(true), First: &limit}, nil, "onlyTotal[first]"},
		{"sort", &pb.PaginationRequest{OnlyTotal: proto.Bool(true)},
			func(b *CriteriaBuilder) *CriteriaBuilder { return b.OrderBy(&pb.OrderByField{Field: "name"}) },
			"onlyTotal[orderBy]"},
		{"read_mask", &pb.PaginationRequest{OnlyTotal: proto.Bool(true)},
			func(b *CriteriaBuilder) *CriteriaBuilder {
				return b.FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"name"}})
			},
			"onlyTotal[fields]"},
	}
	for _, tc := range cases {
		b := NewCriteria().Fields(fields).Page(tc.req)
		if tc.add != nil {
			b = tc.add(b)
		}
		_, err := b.Build()
		if err == nil {
			t.Fatalf("%s: want %q violation, got nil", tc.name, tc.want)
		}
		// Gateway violations travel as the framework's typed notification
		// error — assert the per-field triple, not the error prose.
		if got := violationFields(t, err); len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: want field %q, got %v", tc.name, tc.want, got)
		}
	}

	// only_total alone (with filters upstream) stays the canonical count path.
	crit, err := NewCriteria().Fields(fields).Page(&pb.PaginationRequest{OnlyTotal: proto.Bool(true), IncludeArchived: proto.Bool(true)}).Build()
	if err != nil || !crit.OnlyTotal || !crit.IncludeArchived {
		t.Fatalf("only-total must pass clean: crit=%+v err=%v", crit, err)
	}
}

// TestCriteriaBoolPresenceWithoutActivation proves the presence/activation
// split on the optional bools — the REST `?onlyTotal=false` semantics on the
// gRPC wire: an explicitly-set false registers PRESENCE for the opt-in gate
// but activates nothing (no count short-circuit, no conflict with paging,
// no archived rows surfaced).
func TestCriteriaBoolPresenceWithoutActivation(t *testing.T) {
	limit := int64(5)
	b := NewCriteria().Page(&pb.PaginationRequest{
		OnlyTotal:       proto.Bool(false),
		IncludeArchived: proto.Bool(false),
		First:           &limit,
	})
	controls := b.Controls()
	if controls.OnlyTotal == nil || *controls.OnlyTotal {
		t.Fatalf("only_total=false must record inactive presence, got %+v", controls.OnlyTotal)
	}
	if !controls.IncludeArchived {
		t.Fatalf("include_archived=false must record presence")
	}
	// Inactive only_total shapes nothing — first alongside is NOT a conflict.
	crit, err := b.Build()
	if err != nil {
		t.Fatalf("inactive only_total must not conflict with first: %v", err)
	}
	if crit.OnlyTotal || crit.IncludeArchived || crit.Limit != 5 {
		t.Fatalf("false values must not activate: %+v", crit)
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
	cl := crit.Filter["Name"].(queries.Clause)
	if cl.Op != queries.FilterIn {
		t.Fatalf("want in-Clause, got %#v", cl)
	}
	in := cl.Values
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
	if !reflect.DeepEqual(mc.Clauses[0], queries.Clause{Op: queries.FilterGte, Values: []any{int64(2)}}) {
		t.Fatalf("gte clause: %#v", mc.Clauses[0])
	}
	if !reflect.DeepEqual(mc.Clauses[1], queries.Clause{Op: queries.FilterLte, Values: []any{int64(5)}}) {
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
	if !reflect.DeepEqual(crit.Filter["Score"], queries.Clause{Op: queries.FilterGt, Values: []any{4.5}}) {
		t.Fatalf("double: %#v", crit.Filter["Score"])
	}
	if crit.Filter["Active"] != true {
		t.Fatalf("bool eq must land as scalar: %#v", crit.Filter["Active"])
	}
	want := queries.Clause{Op: queries.FilterGte, Values: []any{when.Format(time.RFC3339Nano)}}
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
	rml, ok := crit.Filter["Kind"].(queries.TextMatchList)
	if !ok || !rml.CaseInsensitive || rml.Negate {
		t.Fatalf("iin: %#v", crit.Filter["Kind"])
	}
	if len(rml.Values) != 2 || rml.Values[0] != "Tool" {
		t.Fatalf("raw values: %#v", rml.Values)
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
		Page(nil).FieldMask(nil).
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
	// The Fields vocabulary IS the allowlist: an undeclared fields-mask or
	// sort path fails Build (→ SchemaViolation at the wrapper), so a raw
	// physical-column spelling can never reach the reader and bypass
	// ToCriteria overlays such as Restrict.
	_, err := NewCriteria().
		Fields(map[string]string{"id": "ID"}).
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"phone"}}).
		Build()
	if err == nil || !strings.Contains(err.Error(), "fields") {
		t.Fatalf("undeclared mask path must fail: %v", err)
	}
	_, err = NewCriteria().
		Fields(map[string]string{"id": "ID"}).
		OrderBy(&pb.OrderByField{Field: "phone"}).
		Build()
	if err == nil || !strings.Contains(err.Error(), "orderBy") {
		t.Fatalf("undeclared sort field must fail: %v", err)
	}
	// no Fields declared at all → mask/sort unsupported for the view
	_, err = NewCriteria().
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"id"}}).
		Build()
	if err == nil {
		t.Fatalf("mask without a declared vocabulary must fail")
	}
}
