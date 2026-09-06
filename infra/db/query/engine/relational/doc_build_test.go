package relational

import (
	"context"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// ─── children ────────────────────────────────────────────────────────────────

// partVO is an aggregate child: the DOMAIN names the segment it occupies, and
// answers "is this the same child?" itself.
type partVO struct {
	domain.Managed
	Label string
}

func (p partVO) BuildRules(string, domain.Service, *domain.Rules) {}
func (p partVO) CollectionName() string                           { return "Parts" }
func (p partVO) IsSameBusinessIdentity(other domain.AggregateValueObject) bool {
	o, ok := other.(partVO)
	return ok && o.Label == p.Label
}

// kitEnt is an aggregate ROOT: it carries the AggregateRoot machinery the read
// side walks to find the children.
type kitEnt struct {
	domain.AggregateRoot
	Name string
}

func (e *kitEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *kitEnt) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *kitEnt) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *kitEnt) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{partVO{}}
}

func partSchema() *core.TableSchema {
	return core.NewTableSchema[partVO]("kit_parts").
		ID("id").
		ParentID("kit_id").
		Field("Label", "label").
		DeletedAt("deleted_at")
}

func kitSchema() *core.TableSchema {
	return core.NewTableSchema[*kitEnt]("kits").
		ID("id").
		Field("Name", "name").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Revision("revision").
		Child(partSchema())
}

// The document a relational read serves must carry the aggregate's own children
// nested under the segment the DOMAIN declares — the same name the projection
// composer nests them under, so both backings translate identically.
func TestBuildDocument_NestsOwnChildrenUnderTheDeclaredSegment(t *testing.T) {
	e := &kitEnt{Name: "starter"}
	e.SetID(domain.NewID("33333333-3333-3333-3333-333333333333"))
	p := partVO{Label: "bolt"}
	p.SetID(domain.NewID("44444444-4444-4444-4444-444444444444"))
	e.AggregateConstructor([]domain.AggregateValueObject{p})

	doc := BuildDocument(kitSchema(), e)

	rows, ok := doc["Parts"].([]query.Document)
	if !ok || len(rows) != 1 {
		t.Fatalf("children must nest under the declared segment, got %#v", doc["Parts"])
	}
	if rows[0]["label"] != "bolt" {
		t.Errorf("child field = %#v, want bolt", rows[0]["label"])
	}
	if _, has := rows[0]["id"]; !has {
		t.Error("a child row must carry its own id")
	}
}

func TestBuildDocument_ChildlessAggregateStillDeclaresTheSegment(t *testing.T) {
	e := &kitEnt{Name: "empty"}
	e.SetID(domain.NewID("55555555-5555-5555-5555-555555555555"))

	doc := BuildDocument(kitSchema(), e)
	rows, ok := doc["Parts"].([]query.Document)
	if !ok || len(rows) != 0 {
		t.Fatalf("a childless aggregate must carry an EMPTY segment, not a missing one: %#v", doc["Parts"])
	}
}

// ─── managed columns ─────────────────────────────────────────────────────────

// The managed timestamps land under their PHYSICAL columns, and the ROOT's
// revision rides the framework watermark — never its physical column, which is
// the shape the projection composer produces.
func TestBuildDocument_ManagedColumnsAndTheRevisionWatermark(t *testing.T) {
	e := &kitEnt{Name: "stamped"}
	e.SetID(domain.NewID("66666666-6666-6666-6666-666666666666"))
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	domain.SetManagedColumns(e, 4, &now, &now, nil)

	doc := BuildDocument(kitSchema(), e)

	if doc["created_at"] != now || doc["updated_at"] != now {
		t.Errorf("managed timestamps must land under their columns: %v / %v", doc["created_at"], doc["updated_at"])
	}
	if doc[query.DocRevisionField] != int64(4) {
		t.Errorf("the root revision must ride the watermark, got %#v", doc[query.DocRevisionField])
	}
	if _, physical := doc["revision"]; physical {
		t.Error("the root revision must NOT also sit under its physical column")
	}
	// A live row's DeletedAt is a PRESENT nil — the shape a fetched NULL has, so
	// the archived gate reads the two paths identically.
	v, present := doc["deleted_at"]
	if !present || v != nil {
		t.Errorf("a live row's deleted_at must be present and nil, got (%#v, %v)", v, present)
	}
}

// A schema declaring no managed columns gets none invented for it.
func TestBuildDocument_UndeclaredManagedColumnsAreAbsent(t *testing.T) {
	e := &guardEnt{Name: "bare"}
	e.SetID(domain.NewID("77777777-7777-7777-7777-777777777777"))

	doc := BuildDocument(guardSchema("gadgets"), e)
	for _, col := range []string{"created_at", "updated_at", "deleted_at", query.DocRevisionField} {
		if _, has := doc[col]; has {
			t.Errorf("%q must be absent when the schema declares none", col)
		}
	}
}

// ─── siblings ────────────────────────────────────────────────────────────────

// A sibling partitions the row, so its columns land FLAT on the document — the
// read-side mirror of how the write side split them.
func TestBuildDocument_MergesSiblingColumnsFlat(t *testing.T) {
	e := &guardEnt{Name: "widget", Material: "steel"}
	e.SetID(domain.NewID("88888888-8888-8888-8888-888888888888"))

	doc := BuildDocument(siblingSchema("gadgets"), e)
	if doc["material"] != "steel" {
		t.Errorf("the sibling column must land flat, got %#v", doc["material"])
	}
	if doc["name"] != "widget" {
		t.Errorf("the root column must survive, got %#v", doc["name"])
	}
}

// ─── ReadByID ────────────────────────────────────────────────────────────────

// byIDLoader answers a single-aggregate load and records the criteria it got, so
// the by-id path's predicate can be asserted without a database.
type byIDLoader struct {
	table string
	ents  []domain.Entity
	saw   *criteria.Query
}

func (l *byIDLoader) FindAllEntities(_ context.Context, q *criteria.Query) ([]domain.Entity, error) {
	l.saw = q
	return l.ents, nil
}
func (l *byIDLoader) CountEntities(context.Context, *criteria.Query) (int64, error) { return 0, nil }
func (l *byIDLoader) Schema() *core.TableSchema                                     { return guardSchema(l.table) }
func (l *byIDLoader) JoinFields() map[string][]string                               { return nil }

func byIDReader(t *testing.T, l *byIDLoader) *ViewReader {
	t.Helper()
	return NewViewReader([]*query.RelationalViewDefinition{query.RelationalView("v", l)})
}

func TestReadByID_ServesTheAggregateAsAGoKeyedDocument(t *testing.T) {
	e := &guardEnt{Name: "found"}
	e.SetID(domain.NewID("99999999-9999-9999-9999-999999999999"))
	l := &byIDLoader{table: "gadgets", ents: []domain.Entity{e}}

	doc, ok, err := byIDReader(t, l).ReadByID(context.Background(), "v", "99999999-9999-9999-9999-999999999999", queries.ReadCriteria{})
	if err != nil || !ok {
		t.Fatalf("ReadByID = (%v, %v, %v)", doc, ok, err)
	}
	if doc["Name"] != "found" {
		t.Errorf("the document must be keyed by GO field, got %#v", doc)
	}
	// The store's own identity key never reaches the caller.
	if _, leaked := doc["_id"]; leaked {
		t.Error("a served document must carry no store identity key")
	}
	if doc["ID"] == nil {
		t.Error("the identity must be served under the Go field ID")
	}
	if l.saw == nil || l.saw.LimitValue() != 1 {
		t.Errorf("a by-id read must be windowed to one row, got %+v", l.saw)
	}
}

// Not found is (nil, false, nil) — never an error: absence is an answer.
func TestReadByID_NotFoundIsNotAnError(t *testing.T) {
	l := &byIDLoader{table: "gadgets"}
	doc, ok, err := byIDReader(t, l).ReadByID(context.Background(), "v", "no-such-id", queries.ReadCriteria{})
	if err != nil || ok || doc != nil {
		t.Fatalf("absent = (%v, %v, %v), want (nil, false, nil)", doc, ok, err)
	}
}

// A security overlay from the Query (ReadCriteria.Filter) merges into the by-id
// predicate — the tenant gate must not be bypassable by reading a known id.
func TestReadByID_MergesTheSecurityOverlay(t *testing.T) {
	e := &guardEnt{Name: "scoped"}
	e.SetID(domain.NewID("12121212-1212-1212-1212-121212121212"))
	l := &byIDLoader{table: "gadgets", ents: []domain.Entity{e}}

	_, _, err := byIDReader(t, l).ReadByID(context.Background(), "v", "12121212-1212-1212-1212-121212121212",
		queries.ReadCriteria{Filter: map[string]any{"Name": "scoped"}})
	if err != nil {
		t.Fatalf("ReadByID: %v", err)
	}
	if l.saw == nil || l.saw.Condition() == nil {
		t.Fatal("the overlay must reach the loader as part of the predicate")
	}
}

// The overlay is subject to the SAME capability boundary as a listing filter: a
// field this read cannot serve is refused, not silently dropped.
func TestReadByID_RefusesAnUnservableOverlay(t *testing.T) {
	l := &byIDLoader{table: "gadgets"}
	_, _, err := byIDReader(t, l).ReadByID(context.Background(), "v", "some-id",
		queries.ReadCriteria{Filter: map[string]any{"Parts.Label": "bolt"}})
	assertUnsupportedCapability400(t, err, "parts.label")
}

func TestReadByID_UnregisteredViewIsAnError(t *testing.T) {
	r := NewViewReader(nil)
	if _, _, err := r.ReadByID(context.Background(), "nope", "x", queries.ReadCriteria{}); err == nil {
		t.Fatal("an unregistered view must error")
	}
}

// ─── remaining branches ──────────────────────────────────────────────────────

// A document is built from whatever the loader hands back. An entity that is not
// an aggregate root carries no children, and a schema declaring children over
// such an entity simply produces none — never a panic on the read path.
func TestBuildDocument_NonAggregateEntityYieldsNoChildSegments(t *testing.T) {
	e := &guardEnt{Name: "flat"}
	e.SetID(domain.NewID("13131313-1313-1313-1313-131313131313"))

	doc := BuildDocument(kitSchema(), e)
	if _, has := doc["Parts"]; has {
		t.Error("a non-aggregate entity must produce no child segment")
	}
}

// An entity with no id yet (never persisted) still maps its business columns —
// only the id is absent.
func TestBuildDocument_WithoutAnIDMapsTheRestAnyway(t *testing.T) {
	doc := BuildDocument(guardSchema("gadgets"), &guardEnt{Name: "unsaved"})
	if doc["name"] != "unsaved" {
		t.Errorf("business columns must map without an id, got %#v", doc["name"])
	}
	if _, has := doc["id"]; has {
		t.Error("an entity with no id must carry no id column")
	}
}

// The per-view page ceiling: a declared MaxLimit wins, and a view that declares
// none falls to the framework floor rather than reading unbounded.
func TestResolveMaxLimit_FallsBackWhenNoResolverOrNonPositive(t *testing.T) {
	r := NewViewReader([]*query.RelationalViewDefinition{
		query.RelationalView("v", guardLoader("gadgets")),
	})
	if got := r.resolveMaxLimit("v"); got != defaultPageLimit {
		t.Errorf("with no resolver installed = %d, want the framework floor %d", got, defaultPageLimit)
	}
	r.SetMaxLimitResolver(func(string) int64 { return 0 })
	if got := r.resolveMaxLimit("v"); got != defaultPageLimit {
		t.Errorf("a non-positive answer must fall back, got %d", got)
	}
	r.SetMaxLimitResolver(func(string) int64 { return 7 })
	if got := r.resolveMaxLimit("v"); got != 7 {
		t.Errorf("a declared ceiling must win, got %d", got)
	}
}

// A cursor is refused unless it decodes AND its context hash matches the current
// filter/sort — a stale cursor cannot silently page a different listing.
func TestDecodeOffset_RejectsMalformedAndStaleCursors(t *testing.T) {
	if _, err := decodeOffset("not-a-cursor", "ctx"); err == nil {
		t.Error("an undecodable cursor must be refused")
	}
	good, err := queries.EncodeCursor(offsetTuple(3, 0), "ctx")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if _, err := decodeOffset(good, "OTHER-CONTEXT"); err == nil {
		t.Error("a cursor from a different listing context must be refused")
	}
	got, err := decodeOffset(good, "ctx")
	if err != nil || got != 3 {
		t.Fatalf("decodeOffset = (%d, %v), want (3, nil)", got, err)
	}
	// A negative offset is not a position.
	bad, _ := queries.EncodeCursor([]any{-1}, "ctx")
	if _, err := decodeOffset(bad, "ctx"); err == nil {
		t.Error("a negative offset must be refused")
	}
}

// The cursor tuple is padded to len(sort)+1 so it passes the STRUCTURAL check
// every wire surface runs before the reader ever sees it.
func TestOffsetTuple_PadsToTheSortArity(t *testing.T) {
	if got := offsetTuple(9, 2); len(got) != 3 || got[0] != int64(9) {
		t.Fatalf("offsetTuple(9, 2) = %v, want the offset plus 2 inert slots", got)
	}
	if got := offsetTuple(0, 0); len(got) != 1 {
		t.Fatalf("an unordered read still carries one slot, got %v", got)
	}
}
