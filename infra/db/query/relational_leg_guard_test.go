package query

import "testing"

// A RelationalSource() view has NO Mongo collection — it is served from the SoR.
// So it can never be an Embed/Link SOURCE (the join would silently read empty).
// The boot validators reject the combination; these tests pin each site. The
// relationalView / noopRelReader helpers live in sync_relational_skip_test.go;
// wantProblem lives in composed_view_test.go.

// Embed family (appendViewLegProblems), reached from Embed / EmbedMany /
// EmbedInChild.
func TestValidateViewSchemas_RejectsRelationalEmbedSource(t *testing.T) {
	src := relationalView("gsrc")
	embedder := View("kits").Version(1).Schema(rootSchema("kits")).
		Embed(JoinView(src, "Part", "part")).On("kit_id")

	err := ValidateViewSchemas([]*ViewDefinition{embedder, src})
	wantProblem(t, err, "is a RelationalSource() view")
}

// The OTHER direction (Fix #17 coverage): a RelationalSource() view is a single
// plain aggregate read, so it cannot ITSELF carry an Embed — an embed is a Mongo
// read, not part of the relational load. The boot validator rejects the
// combination naming the marker.
func TestValidateViewSchemas_RejectsRelationalWithEmbed(t *testing.T) {
	src := View("psrc").Version(1).Schema(rootSchema("psrc"))
	relEmbedder := View("v").Version(1).Schema(rootSchema("v")).
		Embed(JoinView(src, "Part", "part")).On("v_id").
		RelationalSource(noopRelReader{table: "v"})

	err := ValidateViewSchemas([]*ViewDefinition{relEmbedder, src})
	wantProblem(t, err, "cannot be combined with Embed")
}

// ComposedView primary (ValidateComposedViews).
func TestValidateComposedViews_RejectsRelationalPrimary(t *testing.T) {
	relPrimary := cvPrimaryView().RelationalSource(noopRelReader{table: "gadgets"})
	c := ComposedView("gadgets_full").
		Primary(relPrimary).
		Link(JoinUpstream(cvUpstreamSchema(), "UpstreamMirror", "upstreamMirror")).On("id")

	err := validateOne(c, []*ViewDefinition{relPrimary, cvNotesView()}, cvUpstreams())
	wantProblem(t, err, "is a RelationalSource() view")
}

// ComposedView internal leg (validateComposedLinks).
func TestValidateComposedViews_RejectsRelationalLeg(t *testing.T) {
	relLeg := cvNotesView().RelationalSource(noopRelReader{table: "notes"})
	c := ComposedView("gadgets_full").
		Primary(cvPrimaryView()).
		Link(JoinUpstream(cvUpstreamSchema(), "UpstreamMirror", "upstreamMirror")).On("id").
		LinkMany(JoinView(relLeg, "Notes", "notes")).OrderBy("text").Desc().MaxLinkManyLimit(5).On("gadget_id")

	err := validateOne(c, []*ViewDefinition{cvPrimaryView(), relLeg}, cvUpstreams())
	wantProblem(t, err, "is a RelationalSource() view")
}
