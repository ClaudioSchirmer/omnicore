package query

import "testing"

func TestView_EmbedAddsOneToOneSource(t *testing.T) {
	v := View("v").Root("v").Schema(rootSchema("v")).Embed("child", pgEmbed("child", "").FK("v_id")).Version(1)
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
