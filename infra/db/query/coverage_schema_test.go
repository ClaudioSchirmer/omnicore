package query

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TableSchema resolution-helper tests moved to infra/db (they exercise the
// schema's unexported methods). This file keeps the infra-root ViewDefinition
// getters.

func TestViewDefinition_Getters(t *testing.T) {
	root := builderTestSchema
	child := core.NewTableSchema[fakeVO]("c").PK("id").FK("t_id").Field("Label", "label")
	v := View("users").Version(4).Schema(root).
		EmbedMany("kids", FromSchema(child))

	if v.SchemaDef() != root {
		t.Error("SchemaDef must return the attached root schema")
	}
	if v.VersionNumber() != 4 {
		t.Errorf("VersionNumber = %d, want 4", v.VersionNumber())
	}
	embeds := v.Embeds()
	if len(embeds) != 1 {
		t.Fatalf("Embeds len = %d, want 1", len(embeds))
	}
}

func TestViewDefinition_SchemaDef_NilWhenUnset(t *testing.T) {
	v := View("bare").Version(1)
	if v.SchemaDef() != nil {
		t.Error("SchemaDef must be nil before Schema(...) is called")
	}
}
