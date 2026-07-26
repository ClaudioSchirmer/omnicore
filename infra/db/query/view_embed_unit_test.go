package query

import (
	"strings"
	"testing"
)

func TestView_EmbedAddsOneToOneSource(t *testing.T) {
	v := View("v").Schema(rootSchema("v")).Embed("child", pgEmbed("child", "").FK("v_id")).Version(1)
	if len(v.Embeds()) != 1 || v.Embeds()[0].many {
		t.Errorf("Embed should mark many=false, got %+v", v.Embeds())
	}
}

func TestSource_EmbedAndEmbedManyAppend(t *testing.T) {
	s := pgEmbed("a", "").Embed("b", pgEmbed("b", "")).EmbedMany("c", pgEmbed("c", ""))
	if len(s.Embeds()) != 2 {
		t.Errorf("Source embeds len = %d", len(s.Embeds()))
	}
	if s.Embeds()[0].many != false || s.Embeds()[1].many != true {
		t.Errorf("Source embed many flags wrong: %+v", s.Embeds())
	}
}

// TestValidateViewSchemas_ManyTopLevelEmbedsAllowed proves the guard caps DEPTH,
// not WIDTH: a view may declare any number of top-level Embed/EmbedMany (1:1 and
// 1:N mixed) and still boot, as long as no embed source nests a further embed.
func TestValidateViewSchemas_ManyTopLevelEmbedsAllowed(t *testing.T) {
	v := View("orders").Version(1).Schema(rootSchema("orders")).
		EmbedMany("buyers", mongoEmbed("buyers", "order_id").As("Buyers")).
		EmbedMany("items", mongoEmbed("items", "order_id").As("Items")).
		Embed("owner", mongoEmbed("owners", "").As("Owner").FK("owner_id"))
	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("any number of top-level embeds must validate, got: %v", err)
	}
}

// TestValidateViewSchemas_RejectsNestedEmbed proves embed-of-embed is a fatal
// boot error: an embed whose source itself declares an Embed/EmbedMany is
// rejected, because the recompose-ripple that keeps an embed fresh is one-hop and
// a nested segment would drift silently.
func TestValidateViewSchemas_RejectsNestedEmbed(t *testing.T) {
	outer := mongoEmbed("buyers", "order_id").As("Buyers").
		EmbedMany("deep", mongoEmbed("deep", "buyer_id").As("Deep"))
	v := View("orders").Version(1).Schema(rootSchema("orders")).
		EmbedMany("buyers", outer)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "embed-of-embed is NOT supported") {
		t.Fatalf("nested embed must be rejected at boot, got: %v", err)
	}
}
