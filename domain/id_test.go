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

// UnmarshalJSON is the symmetric half of MarshalJSON: an ID round-trips
// through every JSON boundary (outbox payload, audit maps, DTO mapping) —
// including inside a struct and as a nullable pointer — with NO uuid
// validation (NewID parity; IsValid stays the explicit seam).
func TestID_UnmarshalJSON(t *testing.T) {
	t.Run("round-trips through a struct", func(t *testing.T) {
		type carrier struct {
			Ref ID  `json:"ref"`
			Opt *ID `json:"opt,omitempty"`
		}
		in := carrier{Ref: NewID("11111111-2222-3333-4444-555555555555")}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out carrier
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Ref.Value() != in.Ref.Value() {
			t.Errorf("Ref = %q, want %q", out.Ref.Value(), in.Ref.Value())
		}
		if out.Opt != nil {
			t.Errorf("Opt = %v, want nil (omitted null pointer)", out.Opt)
		}
	})

	t.Run("nullable pointer restores from null and from a value", func(t *testing.T) {
		type carrier struct {
			Opt *ID `json:"opt"`
		}
		var out carrier
		if err := json.Unmarshal([]byte(`{"opt":null}`), &out); err != nil || out.Opt != nil {
			t.Fatalf("null: got (%v, %v), want (nil, nil)", out.Opt, err)
		}
		if err := json.Unmarshal([]byte(`{"opt":"the-id"}`), &out); err != nil || out.Opt == nil || out.Opt.Value() != "the-id" {
			t.Fatalf("value: got (%v, %v), want &\"the-id\" (no uuid validation, NewID parity)", out.Opt, err)
		}
	})

	t.Run("non-string JSON errors", func(t *testing.T) {
		var id ID
		if err := json.Unmarshal([]byte(`42`), &id); err == nil {
			t.Fatal("expected an error for a non-string JSON value")
		}
	})
}
