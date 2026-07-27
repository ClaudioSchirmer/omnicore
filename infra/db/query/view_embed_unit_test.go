package query

import (
	"testing"
)

func TestView_EmbedAddsOneToOneSource(t *testing.T) {
	v := View("v").Schema(rootSchema("v")).Embed("child", pgEmbed("child", "").FK("v_id")).Version(1)
	if len(v.Embeds()) != 1 || v.Embeds()[0].many {
		t.Errorf("Embed should mark many=false, got %+v", v.Embeds())
	}
}

// TestValidateViewSchemas_ManyTopLevelEmbedsAllowed proves width is uncapped: a
// view may declare any number of top-level Embed/EmbedMany (1:1 and 1:N mixed)
// and still boot. Depth is capped structurally — a *Source exposes no
// Embed/EmbedMany builder, so embed-of-embed is not expressible and fails to
// compile (no runtime guard, hence no negative test).
func TestValidateViewSchemas_ManyTopLevelEmbedsAllowed(t *testing.T) {
	// The 1:1 Embed requires a covering index on its parent join column (the
	// recompose ripple's reverse scan) — enforced at boot; EmbedMany is exempt.
	v := View("orders").Version(1).Schema(rootSchema("orders")).
		EmbedMany("buyers", mongoEmbed("buyers", "order_id").As("Buyers")).
		EmbedMany("items", mongoEmbed("items", "order_id").As("Items")).
		Embed("owner", mongoEmbed("owners", "").As("Owner").FK("owner_id")).
		Indexes(Index("owner_id"))
	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("any number of top-level embeds must validate, got: %v", err)
	}
}
