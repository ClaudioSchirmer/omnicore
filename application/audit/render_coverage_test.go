package audit

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

// TestRenderLabelsInJSON_DeepMalformedShapes drives the type-assertion
// "else" branches not covered elsewhere: a children entry not a list, a
// list element not a map, a changes value not a list, a changes element not
// a map, and a changes element with a missing/empty fieldLabelKey.
func TestRenderLabelsInJSON_DeepMalformedShapes(t *testing.T) {
	// children entries not a list, then a list element that is not a map.
	RenderLabelsInJSON(map[string]any{
		"children": map[string]any{
			"NotAList": "scalar",
			"Address":  []any{"not-a-map"},
		},
	}, basePTBR(), configuration.LangPTBR)

	// child map with a changes block holding a missing-key and a populated key.
	doc := map[string]any{
		"children": map[string]any{
			"Address": []any{
				map[string]any{
					"changes": []any{
						map[string]any{"field": "street"},                      // no fieldLabelKey
						map[string]any{"fieldLabelKey": ""},                    // empty key
						map[string]any{"fieldLabelKey": "AddressZipCodeField"}, // rendered
						"not-a-map", // skipped
					},
				},
				map[string]any{"changes": 42}, // changes not a list → ignored
			},
		},
	}
	RenderLabelsInJSON(doc, basePTBR(), configuration.LangPTBR)

	addr := doc["children"].(map[string]any)["Address"].([]any)
	changes := addr[0].(map[string]any)["changes"].([]any)
	rendered := changes[2].(map[string]any)
	if rendered["fieldLabel"] != "CEP" {
		t.Errorf("expected rendered fieldLabel=CEP, got %v", rendered["fieldLabel"])
	}
	if _, ok := rendered["fieldLabelKey"]; ok {
		t.Error("expected fieldLabelKey removed after render")
	}
}
