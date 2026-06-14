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
