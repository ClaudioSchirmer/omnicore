package grpc

// A *queryschema.RequestSchema is memoized per reflect.Type and handed to every
// surface that reads the same Request DTO — the REST wrapper, the OpenAPI
// generator, the GraphQL plan and this binder — and to every request each of
// them serves. It is therefore read-only shared state, and the only safe number
// of writers is zero.
//
// The Auto path takes the DTO's control set and ordering vocabulary from it.
// This file pins that it takes them WITHOUT writing back: a builder that
// recorded its filters into the borrowed schema raced every other reader of the
// cache (a fatal concurrent map write, unrecoverable) and leaked the Go-path
// spelling of each filtered field into the wire vocabulary the REST parser
// validates against — an undocumented alias that appeared only after someone
// called the gRPC procedure.

import (
	"reflect"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
	omnicorepb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// isolationPlan compiles the Auto-path plan for searchGadgetsDTO.
func isolationPlan(t *testing.T) *queryPlan {
	t.Helper()
	plan, err := compileQueryPlan("isolation",
		(&testpb.SearchGadgetsRequest{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(searchGadgetsDTO{}),
		(&testpb.GadgetItem{}).ProtoReflect().Descriptor(),
		reflect.TypeOf(gadgetItemDTO{}), nil)
	if err != nil {
		t.Fatalf("compileQueryPlan: %v", err)
	}
	return plan
}

// isolationRequest is a filtered, ordered, masked request — one that exercises
// every builder call that could write to the borrowed schema.
func isolationRequest() proto.Message {
	return &testpb.SearchGadgetsRequest{
		OrderBy: []*omnicorepb.OrderByField{{Field: "created_at", Desc: true}},
		Filters: &testpb.SearchGadgetsFilters{
			Name: &omnicorepb.StringFilter{Conditions: []*omnicorepb.StringCondition{{
				Op: omnicorepb.StringOp_STRING_OP_EQ, Values: []string{"Drill"},
			}}},
			Rating: &omnicorepb.Int64Filter{Conditions: []*omnicorepb.Int64Condition{{
				Op: omnicorepb.NumberOp_NUMBER_OP_GTE, Values: []int64{3},
			}}},
		},
	}
}

// snapshotVocabulary renders a schema's filter vocabulary as a comparable
// value: every accepted key with the operator set behind it.
func snapshotVocabulary(s *queryschema.RequestSchema) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(s.Filters))
	for key, spec := range s.Filters {
		ops := make(map[string]bool, len(spec.Ops))
		for op := range spec.Ops {
			ops[op] = true
		}
		out[key] = ops
	}
	return out
}

// TestSharedSchema_AutoPathLeavesTheCacheUntouched — one request must not change
// what the endpoint accepts. The vocabulary is the DTO's answer; a request is
// something that answer JUDGES, never something that extends it.
func TestSharedSchema_AutoPathLeavesTheCacheUntouched(t *testing.T) {
	shared := queryschema.ExtractRequestSchema(reflect.TypeOf(searchGadgetsDTO{}))
	before := snapshotVocabulary(shared)

	plan := isolationPlan(t)
	if _, _, err := plan.buildCriteria(isolationRequest().ProtoReflect()); err != nil {
		t.Fatalf("buildCriteria: %v", err)
	}

	after := snapshotVocabulary(shared)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the cached RequestSchema changed under a request:\n before %v\n after  %v", before, after)
	}
	// The specific leak that shipped: the criteria key is the GO path, and
	// recording it against the DTO's wire-keyed map taught the REST parser an
	// alias (`?Name=`) that no surface documents.
	for _, goPath := range []string{"Name", "Rating"} {
		if _, leaked := shared.Filters[goPath]; leaked {
			t.Errorf("the Go-path spelling %q leaked into the DTO's wire vocabulary — REST would accept ?%s=", goPath, goPath)
		}
	}
}

// TestSharedSchema_ConcurrentRequestsShareNothing — the surfaces read the cache
// concurrently by construction (one process serves REST and gRPC at once), so a
// single writer is a fatal concurrent map access, not a race the detector merely
// reports. Reading alongside the requests is the half that used to crash.
func TestSharedSchema_ConcurrentRequestsShareNothing(t *testing.T) {
	shared := queryschema.ExtractRequestSchema(reflect.TypeOf(searchGadgetsDTO{}))
	plan := isolationPlan(t)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// What the REST parser does on every request.
					_, _ = queryschema.ParseKeyAgainstSchema("name.icontains", shared)
				}
			}
		}()
	}

	var callers sync.WaitGroup
	for i := 0; i < 16; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if _, _, err := plan.buildCriteria(isolationRequest().ProtoReflect()); err != nil {
				t.Errorf("buildCriteria: %v", err)
			}
		}()
	}
	callers.Wait()
	close(stop)
	readers.Wait()
}

// TestSharedSchema_RawPathStillDeclaresItsOwnVocabulary — the fix must not cost
// MountRaw its contract. A raw mount carries no Request DTO, so the filters it
// CALLS FOR are what it accepts; that self-declaration is the whole point of
// the path and stays exactly as it was.
func TestSharedSchema_RawPathStillDeclaresItsOwnVocabulary(t *testing.T) {
	crit, err := NewCriteria().
		String("Name", &omnicorepb.StringFilter{Conditions: []*omnicorepb.StringCondition{{
			Op: omnicorepb.StringOp_STRING_OP_STARTSWITH, Values: []string{"Dr"},
		}}}).
		Build()
	if err != nil {
		t.Fatalf("a raw mount must accept the filter it declared by calling for it: %v", err)
	}
	if _, ok := crit.Filter["Name"]; !ok {
		t.Errorf("the declared filter did not reach the criteria: %v", crit.Filter)
	}
}

// TestSharedSchema_TwoDTOsKeepSeparateVocabularies — the cache is keyed by
// reflect.Type, so a leak on one DTO would surface on every OTHER endpoint that
// happens to read the same one. Two plans over two DTOs, run in either order,
// must each keep their own answer.
func TestSharedSchema_TwoDTOsKeepSeparateVocabularies(t *testing.T) {
	narrow := queryschema.ExtractRequestSchema(reflect.TypeOf(narrowSearchDTO{}))
	wide := queryschema.ExtractRequestSchema(reflect.TypeOf(searchGadgetsDTO{}))
	narrowBefore, wideBefore := snapshotVocabulary(narrow), snapshotVocabulary(wide)

	plan := isolationPlan(t)
	if _, _, err := plan.buildCriteria(isolationRequest().ProtoReflect()); err != nil {
		t.Fatalf("buildCriteria: %v", err)
	}

	if !reflect.DeepEqual(narrowBefore, snapshotVocabulary(narrow)) {
		t.Error("a request against one DTO changed what a DIFFERENT DTO accepts")
	}
	if !reflect.DeepEqual(wideBefore, snapshotVocabulary(wide)) {
		t.Error("the request's own DTO vocabulary changed")
	}
}

// TestSortIndex_FoldsSeparatorsAndCaseDeterministically — the ordering
// vocabulary is compiled to a normalized index once, so a proto `created_at`
// meets the DTO's `createdAt` with one lookup. The fold is the behavior; the
// index is only how it is paid for.
func TestSortIndex_FoldsSeparatorsAndCaseDeterministically(t *testing.T) {
	index := sortableIndex(map[string]queryschema.SortSpec{
		"createdAt":         {GoPath: "CreatedAt", Asc: true},
		"addresses.zipCode": {GoPath: "Addresses.ZipCode", Asc: true},
	})
	for wire, want := range map[string]string{
		"created_at":         "createdAt",
		"createdAt":          "createdAt",
		"addresses.zip_code": "addresses.zipCode",
	} {
		if got := index[normalizePath(wire)]; got != want {
			t.Errorf("the wire spelling %q folded to %q, want %q", wire, got, want)
		}
	}
	if _, ok := index[normalizePath("bogus")]; ok {
		t.Error("an undeclared path must not resolve — the assembler names it back verbatim")
	}
}
