package query

import (
	"testing"
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
	// revision <= 0 (pre-4b3 payload) degrades to an unconditional set.
	st0 := guardedSetStage(docRevisionField, Document{"name": lit("Ana")}, 0)
	if _, has := st0["$set"].(Document)[docRevisionField]; has {
		t.Errorf("revision 0 must not write a watermark, got %v", st0)
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
