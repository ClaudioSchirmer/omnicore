package domain

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestID_MarshalJSON_RandomRoundTrip(t *testing.T) {
	id := NewRandomID()

	raw, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("expected JSON string, got %s: %v", raw, err)
	}
	if got != id.Value() {
		t.Fatalf("expected JSON to carry %q, got %q", id.Value(), got)
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("marshaled value %q is not a valid UUID: %v", got, err)
	}
}

func TestID_MarshalJSON_Empty(t *testing.T) {
	var id ID

	raw, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if string(raw) != `""` {
		t.Fatalf("expected empty ID to marshal as \"\", got %s", raw)
	}
}

func TestID_MarshalJSON_InsideStruct(t *testing.T) {
	// Regression: the original bug surfaced via a containing struct's `any`
	// field (web.Response.Data). Reproduce that shape directly to lock the
	// fix in place.
	wrapper := struct {
		Data any `json:"data,omitempty"`
	}{Data: NewID("11111111-2222-3333-4444-555555555555")}

	raw, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	expected := `{"data":"11111111-2222-3333-4444-555555555555"}`
	if string(raw) != expected {
		t.Fatalf("expected %s, got %s", expected, raw)
	}
}
