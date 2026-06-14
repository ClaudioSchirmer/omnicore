package infra

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ─── ViewOf root with no children ────────────────────────────────────────────

type viewFlatEntity struct {
	domain.BaseEntity
	Name string
}

func (e *viewFlatEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate}
}
func (e *viewFlatEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func TestViewOf_FlatEntity_NoEmbeds(t *testing.T) {
	v := ViewOf[*viewFlatEntity]()
	if v.Name() != "view_flat_entities" {
		t.Errorf("collection name = %q, want %q", v.Name(), "view_flat_entities")
	}
	if v.RootTable() != "view_flat_entities" {
		t.Errorf("rootTable = %q, want %q", v.RootTable(), "view_flat_entities")
	}
	if got := len(v.Embeds()); got != 0 {
		t.Errorf("embeds = %d, want 0", got)
	}
}

// ─── ViewOf root with children declared via AggregateChildren ────────────────

type viewChildItem struct {
	Label string
}

func (v viewChildItem) GetID() string                                    { return "" }
func (v viewChildItem) BuildRules(string, domain.Service, *domain.Rules) {}

type viewAggEntity struct {
	domain.AggregateRoot
	Name string
}

func (e *viewAggEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate}
}
func (e *viewAggEntity) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *viewAggEntity) GetAggregateRoot() *domain.AggregateRoot         { return &e.AggregateRoot }
func (e *viewAggEntity) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{viewChildItem{}}
}

func TestViewOf_AggregateChildAutoEmbed(t *testing.T) {
	v := ViewOf[*viewAggEntity]()

	if v.Name() != "view_agg_entities" {
		t.Errorf("collection = %q, want %q", v.Name(), "view_agg_entities")
	}
	if v.RootTable() != "view_agg_entities" {
		t.Errorf("rootTable = %q, want %q", v.RootTable(), "view_agg_entities")
	}

	embeds := v.Embeds()
	if len(embeds) != 1 {
		t.Fatalf("embeds = %d, want 1", len(embeds))
	}
	e := embeds[0]
	if e.field != "view_child_items" {
		t.Errorf("embed field = %q, want %q", e.field, "view_child_items")
	}
	if !e.many {
		t.Error("embed should be EmbedMany (collection)")
	}
	if e.source.table != "view_child_items" {
		t.Errorf("source.table = %q, want %q", e.source.table, "view_child_items")
	}
	if e.source.joinKey != "view_agg_entity_id" {
		t.Errorf("source.joinKey = %q, want %q", e.source.joinKey, "view_agg_entity_id")
	}
}

// ─── ViewOf allows post-construction extension ───────────────────────────────

func TestViewOf_IsMutableAfterCreation(t *testing.T) {
	v := ViewOf[*viewFlatEntity]().
		EmbedMany("custom_table", From("custom_table").On("flat_id"))

	embeds := v.Embeds()
	if len(embeds) != 1 {
		t.Fatalf("expected 1 extra embed after extension, got %d", len(embeds))
	}
	if embeds[0].field != "custom_table" {
		t.Errorf("custom embed field = %q", embeds[0].field)
	}
}

// ─── DeleteOnArchive opt-in ──────────────────────────────────────────────────

// TestViewDefinition_DeleteOnArchiveDefaultFalse_Flat locks the canonical
// default for a root-only view: without the opt-in, DeletesOnArchive() is
// false → ARCHIVED events go through compose+upsert (the Mongo projection
// mirrors PostgreSQL symmetrically — archived rows survive with deleted_at
// populated). Default-false matters because the framework's documented
// contract is keep-by-default; flipping it is the consumer's explicit
// hot-tier choice.
func TestViewDefinition_DeleteOnArchiveDefaultFalse_Flat(t *testing.T) {
	v := View("things").Root("things")
	if v.DeletesOnArchive() {
		t.Fatal("DeletesOnArchive() must default to false on a flat view")
	}
}

// TestViewDefinition_DeleteOnArchiveDefaultFalse_Aggregate locks the same
// default for a view with embedded children — the flag governs the whole
// aggregate projection (root + every embed) as a single cascade, so the
// default must be observable identically whether or not the view has
// EmbedMany/Embed entries declared.
func TestViewDefinition_DeleteOnArchiveDefaultFalse_Aggregate(t *testing.T) {
	v := View("users").Root("users").
		EmbedMany("addresses", From("addresses").On("user_id"))
	if v.DeletesOnArchive() {
		t.Fatal("DeletesOnArchive() must default to false on an aggregate view")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("aggregate view should carry 1 embed, got %d", len(v.Embeds()))
	}
}

// TestViewDefinition_DeleteOnArchiveBuilder_Flat verifies the fluent builder
// both flips the flag and returns *ViewDefinition for chaining — canonical
// usage on a root-only view is View("x").DeleteOnArchive().Root("x").
func TestViewDefinition_DeleteOnArchiveBuilder_Flat(t *testing.T) {
	v := View("things").DeleteOnArchive().Root("things")
	if !v.DeletesOnArchive() {
		t.Fatal("expected DeletesOnArchive() = true after .DeleteOnArchive() builder")
	}
	if v.RootTable() != "things" {
		t.Errorf("chaining broken: RootTable = %q, want %q", v.RootTable(), "things")
	}
}

// TestViewDefinition_DeleteOnArchiveBuilder_Aggregate verifies the builder
// composes with EmbedMany so the opt-in survives an aggregate definition
// (canonical usage View("users").DeleteOnArchive().Root("users").EmbedMany(...)
// for the hot-tier projection of an aggregate). The cascade is structural —
// the same flag value drives root + every child fetch in Compose — so the
// view-level state is what every embed consults.
func TestViewDefinition_DeleteOnArchiveBuilder_Aggregate(t *testing.T) {
	v := View("users").DeleteOnArchive().Root("users").
		EmbedMany("addresses", From("addresses").On("user_id"))
	if !v.DeletesOnArchive() {
		t.Fatal("expected DeletesOnArchive() = true after builder on aggregate view")
	}
	if v.RootTable() != "users" {
		t.Errorf("chaining broken: RootTable = %q, want %q", v.RootTable(), "users")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("aggregate view should carry 1 embed, got %d", len(v.Embeds()))
	}
}

// TestViewOf_FlatEntity_DefaultsToKeep documents that ViewOf — the convention
// path that infers collection/table from the type — produces a view that
// keeps archived rows in the projection by default, matching the framework's
// canonical default. A consumer that wants the hot-tier projection extends
// the returned *ViewDefinition with .DeleteOnArchive() (ViewDefinition is
// mutable after creation).
func TestViewOf_FlatEntity_DefaultsToKeep(t *testing.T) {
	v := ViewOf[*viewFlatEntity]()
	if v.DeletesOnArchive() {
		t.Fatal("ViewOf default must keep archived rows (DeletesOnArchive() = false)")
	}
}

// TestViewOf_AggregateEntity_DefaultsToKeep is the same guarantee for the
// aggregate path of ViewOf: the convention-derived view of an aggregate
// (with auto-discovered EmbedMany) keeps archived rows by default. The
// opt-in is identical: `ViewOf[*User]().DeleteOnArchive()`.
func TestViewOf_AggregateEntity_DefaultsToKeep(t *testing.T) {
	v := ViewOf[*viewAggEntity]()
	if v.DeletesOnArchive() {
		t.Fatal("ViewOf default must keep archived rows on aggregate views")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("ViewOf aggregate should carry 1 embed, got %d", len(v.Embeds()))
	}
}
