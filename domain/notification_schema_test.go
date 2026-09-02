package domain

import "testing"

func TestSchemaViolationNotification_Semantic(t *testing.T) {
	got := SchemaViolationNotification{}.Semantic()
	if got != SemanticSchema {
		t.Errorf("expected SemanticSchema, got %v", got)
	}
}

func TestRequiredFieldNotification_DefaultSemantic(t *testing.T) {
	got := RequiredFieldNotification{}.Semantic()
	if got != SemanticValidation {
		t.Errorf("expected SemanticValidation (default), got %v", got)
	}
}

func TestRequiredFieldNotification_WithSchemaSemantic(t *testing.T) {
	n := RequiredFieldNotification{}.WithSemantic(SemanticSchema)
	if got := n.Semantic(); got != SemanticSchema {
		t.Errorf("WithSemantic(Schema) → expected SemanticSchema, got %v", got)
	}
}

func TestRequiredFieldNotification_WithSemanticDoesNotAffectOriginal(t *testing.T) {
	original := RequiredFieldNotification{}
	derived := original.WithSemantic(SemanticSchema)

	if original.Semantic() != SemanticValidation {
		t.Errorf("original mutated: expected SemanticValidation, got %v", original.Semantic())
	}
	if derived.Semantic() != SemanticSchema {
		t.Errorf("derived: expected SemanticSchema, got %v", derived.Semantic())
	}
}

func TestSemanticSchema_StringRender(t *testing.T) {
	if got := SemanticSchema.String(); got != "Schema" {
		t.Errorf("expected \"Schema\", got %q", got)
	}
}

func TestMalformedIDNotification_Semantic(t *testing.T) {
	got := MalformedIDNotification{}.Semantic()
	if got != SemanticSchema {
		t.Errorf("expected SemanticSchema, got %v", got)
	}
}

func TestUnknownIDAddressNotification_Semantic(t *testing.T) {
	got := UnknownIDAddressNotification{}.Semantic()
	if got != SemanticNotFound {
		t.Errorf("expected SemanticNotFound, got %v", got)
	}
}

// The three id-related keys must stay distinct spellings. Two of them answer
// the same HTTP status (UnknownIDAddress and RecordNotFound both 404) and two
// share the "bad id" wording, so the KEY is the only thing a consumer can
// branch on to tell a malformed address from an unknown one from an id that
// was wrong inside a payload.
func TestIDNotificationKeys_AreDistinct(t *testing.T) {
	keys := map[string]bool{}
	for _, n := range []Notification{
		MalformedIDNotification{},
		UnknownIDAddressNotification{},
		InvalidIDUUIDNotification{},
		RecordNotFoundNotification{},
	} {
		k := NotificationKey(n)
		if keys[k] {
			t.Fatalf("duplicate notification key %q", k)
		}
		keys[k] = true
	}
}
