package query

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Projector-helper coverage: the watermark guards and the surgical child ops.

func TestGuardedSetStage_Semantics(t *testing.T) {
	set := Document{"name": lit("Ana")}
	st := guardedSetStage(docRevisionField, set, 7)
	inner := st["$set"].(Document)
	if _, ok := inner["name"].(Document)["$cond"]; !ok {
		t.Fatalf("a positive revision must guard every column, got %v", inner)
	}
	if _, ok := inner[docRevisionField]; !ok {
		t.Fatalf("the watermark itself must advance, got %v", inner)
	}
}

func TestBuildProjectionStages_OrderAndGuards(t *testing.T) {
	ev, ok := decodePayloadEvent(pdSchema(), []byte(`{
		"name":"Ana",
		"_ids":{"id":"r1","revision":3},
		"_children":{"pdChild":[{"_op":"insert","id":"c1","label":"x","rank":1}]}
	}`))
	if !ok {
		t.Fatal("decode failed")
	}
	stages := buildProjectionStages(pdSchema(), ev)
	if len(stages) != 3 {
		t.Fatalf("want child stage + guarded scalar stage + segment normalize, got %d: %v", len(stages), stages)
	}
	// The final stage materializes every declared child segment ($ifNull → [])
	// so the projected shape matches the composed one.
	norm := stages[2]["$set"].(Document)
	if _, ok := norm["pdChilds"]; !ok {
		t.Fatalf("segment normalize stage must cover declared children, got %v", norm)
	}
	// Stage order is load-bearing: the child edit (guarded by the ORIGINAL
	// _revision) must precede the scalar stage that advances the watermark.
	childSet := stages[0]["$set"].(Document)
	for seg, expr := range childSet {
		if _, ok := expr.(Document)["$cond"]; !ok {
			t.Errorf("own child stage %q must be revision-guarded, got %v", seg, expr)
		}
	}
	scalarSet := stages[1]["$set"].(Document)
	if _, ok := scalarSet[docRevisionField]; !ok {
		t.Errorf("the final stage must advance _revision, got %v", scalarSet)
	}
}

func TestChildArrayExpr_DeleteAndArchive(t *testing.T) {
	del := childArrayExpr("kids", "id", "c1", childOp{Op: "delete"}, 0, false, nil)
	if _, ok := del["$filter"]; !ok {
		t.Errorf("delete must filter the element out, got %v", del)
	}
	arc := childArrayExpr("kids", "id", "c1",
		childOp{Op: "archive", Fields: Document{"id": "c1"}}, 5, true, pdSchema().ChildSchemas()[0])
	if _, ok := arc["$map"]; !ok {
		t.Errorf("archive must map-mutate the element, got %v", arc)
	}
}

func TestSameFieldShape_IgnoresFrameworkKeys(t *testing.T) {
	fresh := Document{"name": "Ana", docRevisionField: int64(3)}
	stored := Document{"_id": "r1", "name": "Ana"}
	if !sameFieldShape(fresh, stored) {
		t.Error("watermark-only differences must never read as drift")
	}
}

// A sibling group that arrives ALL-NULL is the removed-row marker: the
// projector must DROP the document keys ($$REMOVE) — shape parity with the
// composer, which omits a missing sibling row — while a partially-null group
// (live row with a null column) projects literally.
func TestBuildProjectionStages_SiblingClearRemovesKeys(t *testing.T) {
	type sibRoot struct {
		ID    string
		Name  string
		Email *string
		SMS   *string
	}
	sib := core.NewSiblingSchema[*sibRoot]("root_cfg").
		Field("Email", "email_notification").Field("SMS", "sms_notification")
	schema := core.NewTableSchema[*sibRoot]("roots").PK("id").
		Field("Name", "name").Sibling(sib)

	ev, ok := decodePayloadEvent(schema, []byte(`{
		"name":"Ana","email_notification":null,"sms_notification":null,
		"_ids":{"id":"r1","revision":3}
	}`))
	if !ok {
		t.Fatal("decode failed")
	}
	stages := buildProjectionStages(schema, ev)
	var set Document
	for _, st := range stages {
		if s, okc := st["$set"].(Document); okc {
			if _, hasName := s["name"]; hasName {
				set = s
				break
			}
		}
	}
	if set == nil {
		t.Fatalf("no own-scalar stage found in %v", stages)
	}
	for _, col := range []string{"email_notification", "sms_notification"} {
		cond, okc := set[col].(Document)
		if !okc {
			t.Fatalf("%s must be present (guarded), got %T", col, set[col])
		}
		arms, okc := cond["$cond"].([]any)
		if !okc || len(arms) != 3 {
			t.Fatalf("%s must be revision-guarded, got %v", col, cond)
		}
		if arms[1] != "$$REMOVE" {
			t.Errorf("%s: an all-null sibling group must project $$REMOVE, got %v", col, arms[1])
		}
	}

	// Partially-null group = live row → literal values (explicit null kept).
	ev2, _ := decodePayloadEvent(schema, []byte(`{
		"name":"Ana","email_notification":true,"sms_notification":null,
		"_ids":{"id":"r1","revision":4}
	}`))
	stages2 := buildProjectionStages(schema, ev2)
	found := false
	for _, st := range stages2 {
		if s, okc := st["$set"].(Document); okc {
			if v, has := s["email_notification"]; has {
				found = true
				arms := v.(Document)["$cond"].([]any)
				if lit, okc := arms[1].(Document); !okc || lit["$literal"] != true {
					t.Errorf("live sibling column must project literally, got %v", arms[1])
				}
			}
		}
	}
	if !found {
		t.Fatal("live sibling column missing from stages")
	}
}
