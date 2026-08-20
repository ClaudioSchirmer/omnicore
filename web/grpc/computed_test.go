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
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
	omnicorepb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// COMPUTED-field parity on the gRPC wire. A computed field is declared on the
// RESPONSE DTO (`computed:"A,B"`, naming Result fields) and has no column
// behind it — the Query's FromQueryResult derives it after the read. The two
// consumers of the read vocabulary therefore diverge exactly as they do on
// REST: read_mask pushes the SOURCES down (the `?fields=` pushdown) while
// order_by resolves against the Sortable vocabulary, which a computed path is
// never part of — the `?orderBy=` 400, INVALID_ARGUMENT here.

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

// TestHiddenComputedSources — the builder names the sources that were read
// ONLY to feed a masked computed field: a source the mask also named is not
// hidden, and a source with no wire slot (absent from the vocabulary) needs
// no hiding. No mask, or no computed entry in it, answers nil.
func TestHiddenComputedSources(t *testing.T) {
	fields := map[string]string{"id": "ID", "name": "Name", "display": "Display"}
	computed := map[string][]string{"display": {"Name", "UserName"}}

	// Mask names only the computed field: Name (on the wire) hides,
	// UserName (no wire slot) does not.
	b := NewCriteria().Fields(fields).ComputedFields(computed).
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"display"}})
	if _, err := b.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := b.HiddenComputedSources(); !reflect.DeepEqual(got, []string{"Name"}) {
		t.Fatalf("hidden sources: %v", got)
	}

	// Mask names the source too: nothing to hide.
	b = NewCriteria().Fields(fields).ComputedFields(computed).
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"display", "name"}})
	if _, err := b.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := b.HiddenComputedSources(); got != nil {
		t.Fatalf("a masked source must not hide, got %v", got)
	}

	// No mask at all: nil.
	b = NewCriteria().Fields(fields).ComputedFields(computed)
	if got := b.HiddenComputedSources(); got != nil {
		t.Fatalf("no mask must answer nil, got %v", got)
	}

	// Mask carries no computed path: nil.
	b = NewCriteria().Fields(fields).ComputedFields(computed).
		FieldMask(&fieldmaskpb.FieldMask{Paths: []string{"id"}})
	if got := b.HiddenComputedSources(); got != nil {
		t.Fatalf("a mask without computed paths must answer nil, got %v", got)
	}
}

// TestComputedAutoPath_MaskBlanksUnrequestedSources — the read_mask shapes
// the WIRE, not just the store projection: a mask naming only the computed
// field answers the computed value alone, with the sources it fed — which
// ARE declared Response fields — blanked before projection, exactly as
// REST's `?fields=<computed>` elides them. A source the mask also names
// stays.
func TestComputedAutoPath_MaskBlanksUnrequestedSources(t *testing.T) {
	var saw queries.ReadCriteria
	client := newComputedGadgetClient(t, "/omnicore.grpctest.v1.GadgetService/SearchGadgetsComputedBlank", &saw)

	res, err := client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Fields: &fieldmaskpb.FieldMask{Paths: []string{"kind"}},
	}))
	if err != nil {
		t.Fatalf("masked read: %v", err)
	}
	items := res.Msg.GetItems()
	if len(items) != 1 {
		t.Fatalf("items: %+v", items)
	}
	if items[0].GetKind() != "tool" {
		t.Fatalf("the computed field must answer, got %q", items[0].GetKind())
	}
	if items[0].GetId() != "" || items[0].GetName() != "" {
		t.Fatalf("sources read only to feed the computed field must be blanked, got id=%q name=%q",
			items[0].GetId(), items[0].GetName())
	}

	// Mixed mask: a source the mask names outright is kept.
	res, err = client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{
		Fields: &fieldmaskpb.FieldMask{Paths: []string{"kind", "name"}},
	}))
	if err != nil {
		t.Fatalf("mixed mask: %v", err)
	}
	items = res.Msg.GetItems()
	if items[0].GetName() != "Drill" || items[0].GetKind() != "tool" {
		t.Fatalf("a masked source must stay, got %+v", items[0])
	}
	if items[0].GetId() != "" {
		t.Fatalf("the unmasked source must still blank, got id=%q", items[0].GetId())
	}

	// No mask: the full response, nothing blanked.
	res, err = client.CallUnary(context.Background(), connect.NewRequest(&testpb.SearchGadgetsRequest{}))
	if err != nil {
		t.Fatalf("unmasked read: %v", err)
	}
	items = res.Msg.GetItems()
	if items[0].GetId() != "g-1" || items[0].GetName() != "Drill" {
		t.Fatalf("an unmasked read must keep every field, got %+v", items[0])
	}
}

// TestSortOutsideTheVocabularyIsRefused — order_by resolves against the
// Sortable vocabulary, which is the Request DTO's declaration. A path that is
// projectable (it is in Fields) but was never declared orderable fails Build,
// and one offending entry refuses the whole request.
func TestSortOutsideTheVocabularyIsRefused(t *testing.T) {
	_, err := NewCriteria().
		Fields(map[string]string{"name": "Name", "display": "Display"}).
		Sortable(map[string]queryschema.SortSpec{"name": {GoPath: "Name", Asc: true, Desc: true}}).
		OrderBy(&omnicorepb.OrderByField{Field: "name"}, &omnicorepb.OrderByField{Field: "display", Desc: true}).
		Build()
	if err == nil {
		t.Fatal("ordering by an undeclared path must fail Build")
	}
	if got := violationFields(t, err); !reflect.DeepEqual(got, []string{"display"}) {
		t.Fatalf("the refusal must name the offending entry, got %v", got)
	}
}

// TestSortRepeatedPathIsRefused — the entries become the reader's sort
// document, where a duplicated key is malformed. The refusal names the SECOND
// occurrence, the entry the consumer has to remove — same rule, same choice of
// token, as REST's `orderBy[<token>]`.
func TestSortRepeatedPathIsRefused(t *testing.T) {
	vocab := map[string]queryschema.SortSpec{
		"name":  {GoPath: "Name", Asc: true, Desc: true},
		"email": {GoPath: "Email", Asc: true, Desc: true},
	}
	_, err := NewCriteria().Sortable(vocab).OrderBy(
		&omnicorepb.OrderByField{Field: "name"},
		&omnicorepb.OrderByField{Field: "email"},
		&omnicorepb.OrderByField{Field: "name", Desc: true},
	).Build()
	if got := violationFields(t, err); !reflect.DeepEqual(got, []string{"name"}) {
		t.Fatalf("a repeated ordering path must be refused naming the entry, got %v", got)
	}

	// Distinct paths in one ordering stay legal.
	if _, err := NewCriteria().Sortable(vocab).OrderBy(
		&omnicorepb.OrderByField{Field: "name"},
		&omnicorepb.OrderByField{Field: "email", Desc: true},
	).Build(); err != nil {
		t.Fatalf("a multi-key ordering over distinct paths must pass, got %v", err)
	}
}

// TestSortDirectionIsEnforced — a declaration that admits only one direction
// refuses the other, on this surface exactly as on REST.
func TestSortDirectionIsEnforced(t *testing.T) {
	vocab := map[string]queryschema.SortSpec{"name": {GoPath: "Name", Asc: true}}

	if _, err := NewCriteria().Sortable(vocab).
		OrderBy(&omnicorepb.OrderByField{Field: "name"}).Build(); err != nil {
		t.Fatalf("the admitted direction must pass, got %v", err)
	}
	if _, err := NewCriteria().Sortable(vocab).
		OrderBy(&omnicorepb.OrderByField{Field: "name", Desc: true}).Build(); err == nil {
		t.Fatal("a direction the declaration does not admit must fail Build")
	}
}

// TestSortWithoutAVocabularyRefusesEverything — not calling Sortable means
// nothing is orderable, the framework-wide default.
func TestSortWithoutAVocabularyRefusesEverything(t *testing.T) {
	if _, err := NewCriteria().Fields(map[string]string{"name": "Name"}).
		OrderBy(&omnicorepb.OrderByField{Field: "name"}).Build(); err == nil {
		t.Fatal("a raw builder with no declared vocabulary must not order")
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
// google.rpc details. A computed field backs no column, so it is simply not
// part of the ordering vocabulary: the refusal is the canonical schema
// violation, the same one any undeclared path gets.
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
		if info.GetReason() != "SchemaViolationNotification" {
			t.Fatalf("reason: %q", info.GetReason())
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
