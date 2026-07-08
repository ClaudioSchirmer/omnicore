package grpc

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/internal/testpb"
	pb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
)

// Remaining branch coverage for the bridge internals: the DTO reflection
// edges, the rename fast paths, the marshal error surfaces, and the filter
// apply/type guards — each exercised directly on the private seam.

// ─── collectDTOFields edges ──────────────────────────────────────────────────

type bindEmbeddedBase struct {
	Inherited string `json:"inherited"`
}

type bindPtrEmbedDTO struct {
	*bindEmbeddedBase
	Own   string `json:"own"`
	Blank string `query:","` // query tag with an empty leading segment → skipped spelling
}

func TestCollectDTOFields_PointerEmbedAndEmptyQuerySpelling(t *testing.T) {
	fields := dtoFieldsOf(reflect.TypeOf(bindPtrEmbedDTO{}))
	if _, ok := fields[normalizeName("inherited")]; !ok {
		t.Error("a pointer anonymous embed must contribute its fields")
	}
	if _, ok := fields[normalizeName("own")]; !ok {
		t.Error("own field missing")
	}
	if _, ok := fields[normalizeName("blank")]; !ok {
		t.Error("the json fallback spelling must register even when the query tag is blank")
	}
}

// ─── compileBindPlan / compileFieldPlan edges ────────────────────────────────

type bindChildDTO struct {
	FullName string `json:"fullName"` // protojson's JSON name — end-to-end agreement, no rename
}

type bindParentExactDTO struct {
	Kids     []*bindChildDTO `json:"kids"`
	Favorite bindChildDTO    `json:"favorite"`
}

func TestCompileBindPlan_PointerDTOAndExactNestedMatch(t *testing.T) {
	parentMD := (&testpb.ParentThing{}).ProtoReflect().Descriptor()
	// Pointer DTO type is dereferenced; nested JSON forms agree end to end
	// (full_name == full_name, slice of pointers) → a plan with no renames.
	plan, err := compileBindPlan("t", "request", parentMD, reflect.TypeOf(&bindParentExactDTO{}), nil, nil)
	if err != nil {
		t.Fatalf("compileBindPlan: %v", err)
	}
	if plan.hasRenames() {
		t.Errorf("exact nested match must produce no renames, got %v", plan.renames)
	}
}

type bindBadChildDTO struct {
	Different string `json:"different"`
}

type bindParentBadChildDTO struct {
	Kids     []bindChildDTO  `json:"kids"`
	Favorite bindBadChildDTO `json:"favorite"`
}

func TestCompileFieldPlan_NestedMismatchPropagates(t *testing.T) {
	parentMD := (&testpb.ParentThing{}).ProtoReflect().Descriptor()
	_, err := compileBindPlan("t", "request", parentMD, reflect.TypeOf(bindParentBadChildDTO{}), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no counterpart") {
		t.Fatalf("expected the nested mismatch to propagate, got %v", err)
	}
}

// ─── rename fast paths ───────────────────────────────────────────────────────

func TestRenameFastPaths(t *testing.T) {
	empty := &bindPlan{}
	m := map[string]any{"a": 1}
	empty.renameToDTO(m)  // no renames → untouched
	empty.renameToWire(m) // no renames → untouched
	if len(m) != 1 {
		t.Errorf("empty plan must not mutate, got %v", m)
	}

	// A rename whose key is absent from the payload is skipped.
	p := &bindPlan{renames: map[string]renameNode{"wire_key": {dtoKey: "dtoKey"}}}
	m = map[string]any{"other": 1}
	p.renameToWire(m)
	if _, moved := m["wire_key"]; moved {
		t.Errorf("absent key must not materialize, got %v", m)
	}
	p.renameToDTO(m)
	if len(m) != 1 {
		t.Errorf("absent key must be skipped, got %v", m)
	}
}

// ─── marshal error surfaces ──────────────────────────────────────────────────

func TestDtoToPB_NonProtoTarget(t *testing.T) {
	if _, err := dtoToPB[int](&bindPlan{}, createGadgetDTO{}); err == nil ||
		!strings.Contains(err.Error(), "not a proto.Message") {
		t.Fatalf("expected the non-proto target error, got %v", err)
	}
}

func TestJsonMarshalDTO_Errors(t *testing.T) {
	// Unmarshalable DTO (channel field).
	if _, err := jsonMarshalDTO(&bindPlan{}, struct{ Ch chan int }{make(chan int)}); err == nil {
		t.Fatal("expected the json.Marshal error")
	}
	// A rename plan needs an object payload; a scalar DTO cannot re-key.
	p := &bindPlan{renames: map[string]renameNode{"a": {dtoKey: "b"}}}
	if _, err := jsonMarshalDTO(p, 42); err == nil {
		t.Fatal("expected the unmarshal-to-map error")
	}
}

func TestBuildListResponse_NonProtoEnvelope(t *testing.T) {
	if _, err := buildListResponse[int](&listEnvelope{}, func(map[string]any) gadgetItemDTO { return gadgetItemDTO{} }, queries.Page{}); err == nil ||
		!strings.Contains(err.Error(), "not a proto.Message") {
		t.Fatalf("expected the non-proto envelope error, got %v", err)
	}
}

func TestDtoToMessage_MarshalErrorPropagates(t *testing.T) {
	target := &testpb.GadgetItem{}
	if _, err := dtoToMessage(&bindPlan{}, struct{ Ch chan int }{make(chan int)}, target); err == nil {
		t.Fatal("expected the marshal error")
	}
}

// ─── query plan edges ────────────────────────────────────────────────────────

func TestCompileQueryPlan_Errors(t *testing.T) {
	itemMD := (&testpb.GadgetItem{}).ProtoReflect().Descriptor()
	respDTO := reflect.TypeOf(gadgetItemDTO{})

	t.Run("twoFilterGroups", func(t *testing.T) {
		reqMD := (&testpb.TwoGroupsRequest{}).ProtoReflect().Descriptor()
		_, err := compileQueryPlan("t", reqMD, reflect.TypeOf(searchGadgetsDTO{}), itemMD, respDTO, nil)
		if err == nil || !strings.Contains(err.Error(), "two filter groups") {
			t.Fatalf("expected the two-groups error, got %v", err)
		}
	})
	t.Run("groupLeafWithoutFilterTag", func(t *testing.T) {
		reqMD := (&testpb.SearchGadgetsRequest{}).ProtoReflect().Descriptor()
		type bareDTO struct{} // no filter-tagged leaves at all
		_, err := compileQueryPlan("t", reqMD, reflect.TypeOf(bareDTO{}), itemMD, respDTO, nil)
		if err == nil || !strings.Contains(err.Error(), "no `filter:`-tagged counterpart") {
			t.Fatalf("expected the missing-allowlist error, got %v", err)
		}
	})
	t.Run("bespokeScalarInput", func(t *testing.T) {
		reqMD := (&testpb.GetGadgetRequest{}).ProtoReflect().Descriptor()
		_, err := compileQueryPlan("t", reqMD, reflect.TypeOf(searchGadgetsDTO{}), itemMD, respDTO, nil)
		if err == nil || !strings.Contains(err.Error(), "shared read contract") {
			t.Fatalf("expected the bespoke-input error, got %v", err)
		}
	})
}

func TestCompileListEnvelope_ItemPlanErrorPropagates(t *testing.T) {
	respMD := (&testpb.SearchGadgetsResponse{}).ProtoReflect().Descriptor()
	type strayItemDTO struct {
		ID string `json:"id"`
		// name/kind have no counterpart → the item bridge compile fails
	}
	_, err := compileListEnvelope("t", respMD, reflect.TypeOf(strayItemDTO{}), nil)
	if err == nil || !strings.Contains(err.Error(), "no counterpart") {
		t.Fatalf("expected the item-plan error, got %v", err)
	}
}

// ─── filter apply type guards ────────────────────────────────────────────────

// A wrapper of the wrong concrete type is ignored (defensive: the descriptor
// match upstream makes this unreachable in production).
func TestFilterApply_WrongWrapperTypeIsIgnored(t *testing.T) {
	b := NewCriteria()
	cases := []struct {
		kind    filterKind
		wrapper proto.Message
	}{
		{filterString, &pb.Int64Filter{}},
		{filterInt64, &pb.StringFilter{}},
		{filterDouble, &pb.StringFilter{}},
		{filterBool, &pb.StringFilter{}},
		{filterTimestamp, &pb.StringFilter{}},
	}
	for _, c := range cases {
		fb := filterBinding{kind: c.kind, goPath: "X", ops: []string{"eq"}}
		if bad := fb.apply(b, c.wrapper); bad != "" {
			t.Errorf("kind %v with mismatched wrapper: expected silent skip, got %q", c.kind, bad)
		}
	}
}
