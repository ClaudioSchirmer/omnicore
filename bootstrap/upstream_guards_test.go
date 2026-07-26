package bootstrap

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// extEmbed builds an external (Mongo) embed source from a type-less schema for
// the guard tests — table + FK from the schema, .As supplies the Go segment.
func extEmbed(collection, fk, as string) *query.Source {
	return query.FromSchema(core.NewExternalSchema(collection).PK("id").FK(fk)).FK(fk).As(as)
}

func TestValidateViewSchemas_RejectsMissingRootSchema(t *testing.T) {
	v := query.View("orders").Version(1) // no .Schema(...)
	err := query.ValidateViewSchemas([]*query.ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no root") {
		t.Errorf("expected missing-root-schema error, got %v", err)
	}
}

func TestValidateViewSchemas_RejectsExternalEmbedWithoutAs(t *testing.T) {
	v := query.View("orders").
		Schema(core.NewExternalSchema("orders").PK("id")).
		Embed("buyer", query.FromSchema(core.NewExternalSchema("users").PK("id")).FK("buyer_id")). // no .As
		Version(1)
	err := query.ValidateViewSchemas([]*query.ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), ".As(") {
		t.Errorf("expected external-embed-missing-.As error, got %v", err)
	}
}

func TestValidateViewSchemas_PassesWhenComplete(t *testing.T) {
	v := query.View("orders").
		Schema(core.NewExternalSchema("orders").PK("id")).
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	if err := query.ValidateViewSchemas([]*query.ViewDefinition{v}); err != nil {
		t.Errorf("expected no error for a complete view, got %v", err)
	}
}

func TestResolveUpstreamSubscriptions_MergesCfgAndWiring(t *testing.T) {
	cfg := &Config{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
	}}
	w := Wiring{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "products.events", Collection: "products"},
	}}
	merged, err := resolveUpstreamSubscriptions(cfg, w)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(merged) != 2 {
		t.Errorf("len(merged) = %d, want 2", len(merged))
	}
}

func TestResolveUpstreamSubscriptions_RejectsTopicCollision(t *testing.T) {
	cfg := &Config{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
	}}
	w := Wiring{UpstreamSubscriptions: []UpstreamSubscription{
		{Topic: "users.events", Collection: "users_alt"},
	}}
	_, err := resolveUpstreamSubscriptions(cfg, w)
	if err == nil || !strings.Contains(err.Error(), "users.events") {
		t.Errorf("expected collision error, got %v", err)
	}
}

func TestGuardCollectionCollision_SubSub(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
		{Topic: "peer-users.events", Collection: "users"},
	}
	errs := guardCollectionCollision(subs, nil)
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.2") ||
		!strings.Contains(errs[0], "users.events") ||
		!strings.Contains(errs[0], "peer-users.events") {
		t.Errorf("expected sub-sub collision diagnostic, got %v", errs)
	}
}

func TestGuardCollectionCollision_SubLocalView(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
	}
	views := []*query.ViewDefinition{query.View("users").Version(1)}
	errs := guardCollectionCollision(subs, views)
	if len(errs) != 1 || !strings.Contains(errs[0], "local view") {
		t.Errorf("expected local-view collision diagnostic, got %v", errs)
	}
}

func TestGuardMaterializingSource_RejectsUnknownCollection(t *testing.T) {
	v := query.View("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	errs := guardMaterializingSource(nil, []*query.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.3") ||
		!strings.Contains(errs[0], `"users"`) {
		t.Errorf("expected §8.3 diagnostic for unknown collection, got %v", errs)
	}
}

func TestGuardMaterializingSource_AcceptsSubscriptionCollection(t *testing.T) {
	subs := []UpstreamSubscription{{Topic: "users.events", Collection: "users"}}
	v := query.View("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	errs := guardMaterializingSource(subs, []*query.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with materializing subscription, got %v", errs)
	}
}

func TestGuardMaterializingSource_RejectsLocalView(t *testing.T) {
	// View-on-view via an external FromSchema (targeting another local
	// ViewDefinition.Name()) is rejected at boot: the recompose ripple is
	// one-hop, so a change upstream of derivative_view would recompose
	// derivative_view but never re-ripple to orders. Drift would silently
	// accumulate. The guard catches the trap before any subscriber starts.
	views := []*query.ViewDefinition{
		query.View("orders").
			Embed("derivative", extEmbed("derivative_view", "orders_id", "Derivative")).
			Version(1),
		query.View("derivative_view").Version(1),
	}
	errs := guardMaterializingSource(nil, views)
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.3") ||
		!strings.Contains(errs[0], "view-on-view") ||
		!strings.Contains(errs[0], "NOT supported") {
		t.Errorf("expected §8.3 view-on-view diagnostic, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_RejectsMissingIndex(t *testing.T) {
	v := query.View("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Version(1)
	errs := guardJoinFieldIndex([]*query.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.1") ||
		!strings.Contains(errs[0], "buyer_id") {
		t.Errorf("expected §8.1 diagnostic, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_AcceptsSingleFieldIndex(t *testing.T) {
	v := query.View("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Indexes(query.Index("buyer_id")).
		Version(1)
	errs := guardJoinFieldIndex([]*query.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with covering single-field index, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_AcceptsCompoundIndexJoinFieldFirst(t *testing.T) {
	v := query.View("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Indexes(query.Compound("buyer_id", "created_at")).
		Version(1)
	errs := guardJoinFieldIndex([]*query.ViewDefinition{v})
	if len(errs) != 0 {
		t.Errorf("expected no diagnostic with covering compound index, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_RejectsCompoundIndexJoinFieldNotFirst(t *testing.T) {
	v := query.View("orders").
		Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
		Indexes(query.Compound("created_at", "buyer_id")).
		Version(1)
	errs := guardJoinFieldIndex([]*query.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.1") {
		t.Errorf("expected §8.1 diagnostic (suffix-only coverage), got %v", errs)
	}
}

func TestGuardJoinFieldIndex_EmbedManyNeedsNoIndex(t *testing.T) {
	// A one-to-many EmbedMany resolves its recompose-ripple by the CHANGED
	// child's FK value → the parent _id (always indexed), never a reverse scan
	// of the embedding view on the child FK column (which the parent doc does
	// not carry at top level). So — unlike a one-to-one Embed — it requires NO
	// covering index on the view, even without any Indexes(...) declared.
	src := query.FromSchema(core.NewExternalSchema("items").PK("id").FK("order_id")).As("Members")
	v := query.View("orders").
		EmbedMany("members", src).
		Version(1)
	if errs := guardJoinFieldIndex([]*query.ViewDefinition{v}); len(errs) != 0 {
		t.Errorf("an external EmbedMany must not require a covering index, got %v", errs)
	}
}

func TestGuardJoinFieldIndex_OneToOneStillNeedsIndexAlongsideEmbedMany(t *testing.T) {
	// A view mixing both embeds of the same collection: the 1:1 still needs its
	// covering index; the 1:N does not. With only the 1:1 index declared, the
	// guard is satisfied (no 1:N complaint).
	one := extEmbed("items", "featured_id", "Featured")
	many := query.FromSchema(core.NewExternalSchema("items").PK("id").FK("order_id")).As("Members")
	v := query.View("orders").
		Embed("featured", one).
		EmbedMany("members", many).
		Indexes(query.Index("featured_id")).
		Version(1)
	if errs := guardJoinFieldIndex([]*query.ViewDefinition{v}); len(errs) != 0 {
		t.Errorf("1:1 index present + 1:N index-free must pass, got %v", errs)
	}
}

func TestGuardAnonymizePolicy_RejectsEmptyFields(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", OnUpstreamDelete: UpstreamDeleteAnonymize},
	}
	errs := guardAnonymizePolicy(subs)
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.4") ||
		!strings.Contains(errs[0], "anonymizeFields") {
		t.Errorf("expected §8.4 diagnostic, got %v", errs)
	}
}

func TestGuardAnonymizePolicy_AcceptsCascadeWithEmptyFields(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", OnUpstreamDelete: UpstreamDeleteCascade},
	}
	errs := guardAnonymizePolicy(subs)
	if len(errs) != 0 {
		t.Errorf("cascade does not require AnonymizeFields, got %v", errs)
	}
}

func TestGuardAnonymizePolicy_AcceptsAnonymizeWithFields(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users",
			OnUpstreamDelete: UpstreamDeleteAnonymize,
			AnonymizeFields:  []string{"name", "email"}},
	}
	errs := guardAnonymizePolicy(subs)
	if len(errs) != 0 {
		t.Errorf("anonymize with fields should pass, got %v", errs)
	}
}

func TestValidateUpstreamSubscriptions_AccumulatesAllViolations(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users",
			OnUpstreamDelete: UpstreamDeleteAnonymize, // §8.4 — missing AnonymizeFields
			StartFrom:        StartFromLatest},
		{Topic: "users.events", Collection: "users", // §5 — duplicate sub-sub
			OnUpstreamDelete: UpstreamDeleteCascade,
			StartFrom:        StartFromLatest},
	}
	views := []*query.ViewDefinition{
		// §8.1 — external FromSchema without covering index
		query.View("orders").
			Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
			Version(1),
	}
	err := validateUpstreamSubscriptions(subs, views, profileDev, nil)
	if err == nil {
		t.Fatal("expected violations")
	}
	msg := err.Error()
	// Should list multiple violation lines (§8.1 + §8.2 + §8.4)
	if !strings.Contains(msg, "§8.1") || !strings.Contains(msg, "§8.2") ||
		!strings.Contains(msg, "§8.4") {
		t.Errorf("expected diagnostic to surface all three section codes, got: %s", msg)
	}
}

// extEmbedSD is extEmbed with a soft-delete column declared on the external
// schema — the §8.5 guard reads it via Source().SchemaDef().SoftDeleteColumn().
func extEmbedSD(collection, softDelete, fk, as string) *query.Source {
	return query.FromSchema(
		core.NewExternalSchema(collection).PK("id").SoftDelete(softDelete).FK(fk),
	).FK(fk).As(as)
}

func TestGuardSoftDeleteFilter_AbortsWhenFilterDropsSoftDelete(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", Filter: []string{"id", "name"}},
	}
	views := []*query.ViewDefinition{
		query.View("orders").
			Embed("buyer", extEmbedSD("users", "deleted_at", "buyer_id", "Buyer")).
			Version(1),
	}
	violations, warnings := guardSoftDeleteFilter(subs, views)
	if len(violations) != 1 || !strings.Contains(violations[0], "§8.5") ||
		!strings.Contains(violations[0], "deleted_at") {
		t.Errorf("expected one §8.5 abort naming deleted_at, got %v", violations)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings when the soft-delete column is declared, got %v", warnings)
	}
}

func TestGuardSoftDeleteFilter_OKWhenFilterKeepsSoftDelete(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", Filter: []string{"id", "name", "deleted_at"}},
	}
	views := []*query.ViewDefinition{
		query.View("orders").
			Embed("buyer", extEmbedSD("users", "deleted_at", "buyer_id", "Buyer")).
			Version(1),
	}
	violations, warnings := guardSoftDeleteFilter(subs, views)
	if len(violations) != 0 || len(warnings) != 0 {
		t.Errorf("a filter keeping the soft-delete column must be clean, got violations=%v warnings=%v", violations, warnings)
	}
}

func TestGuardSoftDeleteFilter_OKWhenFilterEmpty(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"}, // nil filter mirrors the full payload
	}
	views := []*query.ViewDefinition{
		query.View("orders").
			Embed("buyer", extEmbedSD("users", "deleted_at", "buyer_id", "Buyer")).
			Version(1),
	}
	violations, warnings := guardSoftDeleteFilter(subs, views)
	if len(violations) != 0 || len(warnings) != 0 {
		t.Errorf("an empty filter must be clean (mirrors everything), got violations=%v warnings=%v", violations, warnings)
	}
}

func TestGuardSoftDeleteFilter_WarnsWhenNoSoftDeleteDeclared(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", Filter: []string{"id", "name"}},
	}
	views := []*query.ViewDefinition{
		query.View("orders").
			Embed("buyer", extEmbed("users", "buyer_id", "Buyer")). // no soft-delete declared
			Version(1),
	}
	violations, warnings := guardSoftDeleteFilter(subs, views)
	if len(violations) != 0 {
		t.Errorf("a missing soft-delete declaration must not abort the boot, got %v", violations)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "§8.5") ||
		!strings.Contains(warnings[0], "Advisory") {
		t.Errorf("expected one §8.5 advisory warning, got %v", warnings)
	}
}

func TestGuardSoftDeleteFilter_SkipsCollectionEmbeddedByNoView(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users", Filter: []string{"id"}},
	}
	// No view embeds "users" → §8.3 owns the never-embedded case; §8.5 stays silent.
	violations, warnings := guardSoftDeleteFilter(subs, nil)
	if len(violations) != 0 || len(warnings) != 0 {
		t.Errorf("a never-embedded mirror must be silent in §8.5, got violations=%v warnings=%v", violations, warnings)
	}
}

func TestValidateUpstreamSubscriptions_SurfacesSoftDeleteAbort(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users",
			OnUpstreamDelete: UpstreamDeleteCascade,
			StartFrom:        StartFromLatest,
			Filter:           []string{"id", "name"}}, // drops deleted_at → §8.5 abort
	}
	views := []*query.ViewDefinition{
		query.View("orders").
			Embed("buyer", extEmbedSD("users", "deleted_at", "buyer_id", "Buyer")).
			Indexes(query.Index("buyer_id")). // satisfy §8.1 so only §8.5 fires
			Version(1),
	}
	// nil logger must be safe on the warn path.
	err := validateUpstreamSubscriptions(subs, views, profileDev, nil)
	if err == nil || !strings.Contains(err.Error(), "§8.5") ||
		!strings.Contains(err.Error(), "deleted_at") {
		t.Errorf("expected §8.5 abort naming deleted_at through the aggregator, got %v", err)
	}
}

func TestValidateUpstreamSubscriptions_LogsSoftDeleteAdvisory(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users",
			OnUpstreamDelete: UpstreamDeleteCascade,
			StartFrom:        StartFromLatest,
			Filter:           []string{"id", "name"}},
	}
	views := []*query.ViewDefinition{
		query.View("orders").
			Embed("buyer", extEmbed("users", "buyer_id", "Buyer")). // no soft-delete → advisory only
			Indexes(query.Index("buyer_id")).
			Version(1),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := validateUpstreamSubscriptions(subs, views, profileDev, logger); err != nil {
		t.Errorf("an advisory-only case must not abort the boot, got %v", err)
	}
}
