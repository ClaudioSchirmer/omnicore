package mongo

import (
	"context"
	"fmt"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ─── filter-aware fake collection ────────────────────────────────────────────
//
// The composed reader's leg fetches carry real filters ($in batches, per-parent
// FK equality, the soft-delete gate) and real find options (limit, sort,
// projection). filterColl honors the filter and the limit — enough to verify
// grouping, LEFT semantics, archived gates and truncation — and captures both
// so tests can assert exactly what was sent to the driver.

type filterColl struct {
	docs    []bson.M
	count   int64
	findErr error

	filters []bson.M
	opts    []*options.FindOptions
	finds   int
}

func (c *filterColl) CountDocuments(ctx context.Context, filter any, opts ...countOpt) (int64, error) {
	return c.count, nil
}

func foldFindOpts(opts []findOpt) *options.FindOptions {
	f := &options.FindOptions{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		for _, fn := range o.List() {
			_ = fn(f)
		}
	}
	return f
}

func matchesFilter(doc bson.M, filter bson.M) bool {
	for k, v := range filter {
		switch cond := v.(type) {
		case nil:
			if dv, present := doc[k]; present && dv != nil {
				return false
			}
		case bson.M:
			in, ok := cond["$in"]
			if !ok {
				return false // operator not emulated — fail loudly in fixtures
			}
			items, ok := in.([]any)
			if !ok {
				return false
			}
			found := false
			for _, item := range items {
				if fmt.Sprintf("%v", item) == fmt.Sprintf("%v", doc[k]) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		default:
			if fmt.Sprintf("%v", doc[k]) != fmt.Sprintf("%v", v) {
				return false
			}
		}
	}
	return true
}

func (c *filterColl) Find(ctx context.Context, filter any, fopts ...findOpt) (*mongo.Cursor, error) {
	c.finds++
	if c.findErr != nil {
		return nil, c.findErr
	}
	f, _ := filter.(bson.M)
	c.filters = append(c.filters, f)
	folded := foldFindOpts(fopts)
	c.opts = append(c.opts, folded)

	var out []any
	for _, d := range c.docs {
		if matchesFilter(d, f) {
			out = append(out, d)
		}
	}
	if folded.Limit != nil && *folded.Limit > 0 && int64(len(out)) > *folded.Limit {
		out = out[:*folded.Limit]
	}
	return mongo.NewCursorFromDocuments(out, nil, nil)
}

func (c *filterColl) FindOne(ctx context.Context, filter any, opts ...findOneOpt) *mongo.SingleResult {
	f, _ := filter.(bson.M)
	for _, d := range c.docs {
		if matchesFilter(d, f) {
			return mongo.NewSingleResultFromDocument(d, nil, nil)
		}
	}
	return mongo.NewSingleResultFromDocument(bson.M{}, mongo.ErrNoDocuments, nil)
}

func (c *filterColl) UpdateOne(ctx context.Context, filter, update any, opts ...updateOpt) (*mongo.UpdateResult, error) {
	return &mongo.UpdateResult{}, nil
}

func (c *filterColl) DeleteOne(ctx context.Context, filter any, opts ...deleteOpt) (*mongo.DeleteResult, error) {
	return &mongo.DeleteResult{}, nil
}

// ─── fixtures ────────────────────────────────────────────────────────────────

type cvrGadget struct{ ID, Code, MirrorID string }
type cvrNote struct{ ID, GadgetID, Text string }

func cvrPrimaryView() *query.ViewDefinition {
	schema := core.NewTableSchema[cvrGadget]("gadgets").
		PK("id").
		Field("Code", "code").
		Field("MirrorID", "mirror_id").
		SoftDelete("deleted_at")
	return query.View("gadgets").Version(1).Root("gadgets").Schema(schema)
}

func cvrNotesView() *query.ViewDefinition {
	schema := core.NewTableSchema[cvrNote]("gadget_notes").
		PK("id").
		Field("GadgetID", "gadget_id").
		Field("Text", "text").
		SoftDelete("deleted_at")
	return query.View("gadget_notes").Version(1).Root("gadget_notes").Schema(schema)
}

func cvrUpstreamSchema() *core.TableSchema {
	return core.NewExternalSchema("upstream_gadgets").PK("id").Field("Code", "code")
}

func cvrComposed(primary, notes *query.ViewDefinition) *query.ComposedViewDefinition {
	return query.ComposedView("gadgets_full").
		Primary(primary).
		Link("upstreamMirror", query.JoinUpstream(cvrUpstreamSchema()).
			FK("id").
			As("UpstreamMirror")).
		LinkMany("notes", query.JoinView(notes).
			FK("gadget_id").
			As("Notes"). // the fixture type is cvrNote — override the derived segment
			OrderBy("text").
			MaxLinkManyLimit(2))
}

type cvrEnv struct {
	reader  *ComposedViewReader
	gadgets *filterColl
	mirror  *filterColl
	notes   *filterColl
}

func newCVREnv() *cvrEnv {
	env := &cvrEnv{
		gadgets: &filterColl{
			count: 2,
			docs: []bson.M{
				{"_id": "g1", "code": "A", "mirror_id": "m1"},
				{"_id": "g2", "code": "B", "mirror_id": "m9"},
			},
		},
		mirror: &filterColl{
			docs: []bson.M{{"_id": "g1", "code": "UP-A"}},
		},
		notes: &filterColl{
			docs: []bson.M{
				{"_id": "n1", "gadget_id": "g1", "text": "a"},
				{"_id": "n4", "gadget_id": "g1", "text": "z", "deleted_at": "2026-01-01"},
				{"_id": "n2", "gadget_id": "g1", "text": "b"},
				{"_id": "n3", "gadget_id": "g2", "text": "c"},
			},
		},
	}
	db := newFakeMongoFunc(func(name string) mongoColl {
		switch name {
		case "gadgets":
			return env.gadgets
		case "upstream_gadgets":
			return env.mirror
		case "gadget_notes":
			return env.notes
		}
		return &filterColl{}
	})
	primary, notes := cvrPrimaryView(), cvrNotesView()
	inner := NewMongoViewReader(db).SetViews([]*query.ViewDefinition{primary, notes})
	env.reader = NewComposedViewReader(inner,
		[]*query.ComposedViewDefinition{cvrComposed(primary, notes)}, 0)
	return env
}

// ─── passthrough ─────────────────────────────────────────────────────────────

func TestComposedReader_PassthroughForRegularView(t *testing.T) {
	env := newCVREnv()
	page, err := env.reader.ReadPage(context.Background(), "gadgets", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page.Items))
	}
	if _, attached := page.Items[0]["UpstreamMirror"]; attached {
		t.Fatal("a regular view read must not be enriched")
	}
	if env.mirror.finds+env.notes.finds != 0 {
		t.Fatal("a regular view read must not touch leg collections")
	}
}

// ─── enrichment ──────────────────────────────────────────────────────────────

func TestComposedReader_ReadPageEnrichesItems(t *testing.T) {
	env := newCVREnv()
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Items) != 2 || page.Total != 2 {
		t.Fatalf("primary pagination must pass through, got %d items total %d", len(page.Items), page.Total)
	}

	g1 := page.Items[0]
	mirror, ok := g1["UpstreamMirror"].(map[string]any)
	if !ok || mirror["Code"] != "UP-A" {
		t.Fatalf("expected the 1:1 segment translated to Go keys, got %#v", g1["UpstreamMirror"])
	}
	notes, ok := g1["Notes"].([]any)
	if !ok || len(notes) != 2 {
		t.Fatalf("expected 2 active notes under the ceiling, got %#v", g1["Notes"])
	}
	first, _ := notes[0].(map[string]any)
	if first["Text"] != "a" || first["GadgetID"] != "g1" {
		t.Fatalf("expected Go-keyed note docs, got %#v", first)
	}

	// LEFT semantics: g2 has no mirror → explicit null; one note → array of 1.
	g2 := page.Items[1]
	if v, present := g2["UpstreamMirror"]; !present || v != nil {
		t.Fatalf("expected an explicit null 1:1 segment, got %#v (present=%v)", v, present)
	}
	if arr, _ := g2["Notes"].([]any); len(arr) != 1 {
		t.Fatalf("expected 1 note for g2, got %#v", g2["Notes"])
	}

	// The 1:1 leg resolves in ONE $in find; the 1:N leg one find per parent.
	if env.mirror.finds != 1 {
		t.Fatalf("expected one $in find on the mirror leg, got %d", env.mirror.finds)
	}
	if env.notes.finds != 2 {
		t.Fatalf("expected one find per parent on the notes leg, got %d", env.notes.finds)
	}
	// Per-parent finds carry the resolved ceiling and the declared order.
	folded := env.notes.opts[0]
	if folded.Limit == nil || *folded.Limit != 2 {
		t.Fatalf("expected the resolved MaxLinkManyLimit(2) on the leg find, got %#v", folded.Limit)
	}
	sortDoc, _ := folded.Sort.(bson.D)
	if len(sortDoc) != 2 || sortDoc[0].Key != "text" || sortDoc[1].Key != "_id" {
		t.Fatalf("expected the declared order + _id tiebreaker, got %#v", folded.Sort)
	}
}

func TestComposedReader_ArchivedGatePerLeg(t *testing.T) {
	env := newCVREnv()

	// Default read: the archived note n4 is gated out by the leg's own
	// soft-delete column; the external mirror leg has no soft-delete, so its
	// filter carries no gate (the knob is a no-op there).
	_, err := env.reader.ReadPage(context.Background(), "gadgets_full", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, gated := env.mirror.filters[0]["deleted_at"]; gated {
		t.Fatal("a leg without soft-delete must not carry an archived gate")
	}
	if v, gated := env.notes.filters[0]["deleted_at"]; !gated || v != nil {
		t.Fatalf("the notes leg must gate archived docs by default, got %#v", env.notes.filters[0])
	}

	// includeArchived propagates to every leg: n4 surfaces (fixture order
	// places it second; the ceiling of 2 keeps [n1, n4]).
	env2 := newCVREnv()
	page, err := env2.reader.ReadPage(context.Background(), "gadgets_full",
		queries.ReadCriteria{IncludeArchived: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	notes, _ := page.Items[0]["Notes"].([]any)
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(notes))
	}
	second, _ := notes[1].(map[string]any)
	if second["Text"] != "z" {
		t.Fatalf("expected the archived note to surface with includeArchived, got %#v", second)
	}
}

// ─── criteria routing ────────────────────────────────────────────────────────

func TestComposedReader_SegmentFilterRoutesToLegOnly(t *testing.T) {
	env := newCVREnv()
	crit := queries.ReadCriteria{Filter: map[string]any{"Notes.Text": "b"}}
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full", crit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The primary filter never sees the segment entry (R2): row selection
	// stays the primary's, so BOTH gadgets return even though only g1 has a
	// matching note.
	for k := range env.gadgets.filters[0] {
		if k == "Notes.Text" || k == "notes.text" {
			t.Fatalf("segment filter leaked into the primary query: %#v", env.gadgets.filters[0])
		}
	}
	if len(page.Items) != 2 {
		t.Fatalf("a segment filter must never select rows, got %d items", len(page.Items))
	}
	// The leg filter carries the entry translated to the physical column.
	if env.notes.filters[0]["text"] != "b" {
		t.Fatalf("leg filter must carry the translated segment filter: %#v", env.notes.filters[0])
	}
	// It shapes the segment content: g1 keeps [b], g2's non-matching note drops.
	g1notes, _ := page.Items[0]["Notes"].([]any)
	if len(g1notes) != 1 {
		t.Fatalf("expected g1's segment filtered to [b], got %#v", g1notes)
	}
	first, _ := g1notes[0].(map[string]any)
	if first["Text"] != "b" {
		t.Fatalf("expected the matching note only, got %#v", first)
	}
	g2notes, _ := page.Items[1]["Notes"].([]any)
	if len(g2notes) != 0 {
		t.Fatalf("expected g2's segment emptied by the filter, got %#v", g2notes)
	}

	// Row filters keep flowing to the primary untouched.
	env2 := newCVREnv()
	crit2 := queries.ReadCriteria{Filter: map[string]any{"Code": "A", "Notes.Text": "b"}}
	page2, err := env2.reader.ReadPage(context.Background(), "gadgets_full", crit2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env2.gadgets.filters[0]["code"] != "A" {
		t.Fatalf("primary filter lost the row-selecting entry: %#v", env2.gadgets.filters[0])
	}
	if len(page2.Items) != 1 {
		t.Fatalf("the row filter selects rows as always, got %d items", len(page2.Items))
	}
}

func TestComposedReader_SegmentSortRejected(t *testing.T) {
	env := newCVREnv()
	_, err := env.reader.ReadPage(context.Background(), "gadgets_full",
		queries.ReadCriteria{Sort: []queries.SortField{{Field: "Notes.Text"}}})
	if err == nil {
		t.Fatal("a sort path into a leg segment must be rejected (R3)")
	}
	var infra *core.InfrastructureError
	if !asInfra(err, &infra) {
		t.Fatalf("expected the canonical Schema rejection, got %T: %v", err, err)
	}
}

func TestComposedReader_OnlyTotalSkipsLegs(t *testing.T) {
	env := newCVREnv()
	env.mirror.findErr = context.Canceled // any leg fetch would fail loudly
	env.notes.findErr = context.Canceled
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full",
		queries.ReadCriteria{OnlyTotal: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !page.OnlyTotal || page.Total != 2 {
		t.Fatalf("expected the count-only page, got %+v", page)
	}
	if env.mirror.finds+env.notes.finds != 0 {
		t.Fatal("onlyTotal must short-circuit before any leg fetch")
	}
}

// ─── projection routing ──────────────────────────────────────────────────────

func TestComposedReader_InclusionProjectionSelectsLegs(t *testing.T) {
	env := newCVREnv()
	crit := queries.ReadCriteria{Projection: map[string]int{
		"Code":       1,
		"Notes.Text": 1,
		"_id":        0,
	}}
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full", crit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	item := page.Items[0]
	if _, present := item["UpstreamMirror"]; present {
		t.Fatal("a leg outside the inclusion projection must not attach")
	}
	if _, present := item["Notes"]; !present {
		t.Fatal("the requested leg must attach")
	}
	// The decorator needed _id to group the 1:N leg, then restored the
	// consumer's exclusion.
	if _, present := item["_id"]; present {
		t.Fatal("the _id helper inclusion must be stripped from the wire shape")
	}
	// The leg find carried the translated sparse projection; _id stays as an
	// INCLUSION (queryable — the attach step may group by it) and is stripped
	// from the wire shape after translation.
	folded := env.notes.opts[0]
	proj, _ := folded.Projection.(bson.D)
	got := map[string]int{}
	for _, e := range proj {
		got[e.Key], _ = e.Value.(int)
	}
	if got["text"] != 1 || got["_id"] != 1 {
		t.Fatalf("unexpected leg projection: %#v", folded.Projection)
	}
	notesArr, _ := item["Notes"].([]any)
	if len(notesArr) > 0 {
		first, _ := notesArr[0].(map[string]any)
		if _, present := first["_id"]; present {
			t.Fatal("the grouping _id must be stripped from partial-projection segment entries")
		}
	}
	// Projection echo carries the COMPOSED projection for export pruning.
	if page.Projection["Notes.Text"] != 1 {
		t.Fatalf("expected the composed projection echoed, got %#v", page.Projection)
	}
}

func TestComposedReader_ExclusionProjectionDropsLeg(t *testing.T) {
	env := newCVREnv()
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full",
		queries.ReadCriteria{Projection: map[string]int{"Notes": 0}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	item := page.Items[0]
	if _, present := item["Notes"]; present {
		t.Fatal("an excluded segment must not attach")
	}
	if _, present := item["UpstreamMirror"]; !present {
		t.Fatal("untouched legs must attach in exclusion mode")
	}
}

func TestComposedReader_WholeSegmentInclusion(t *testing.T) {
	env := newCVREnv()
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full",
		queries.ReadCriteria{Projection: map[string]int{"Notes": 1, "_id": 0}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	item := page.Items[0]
	notes, _ := item["Notes"].([]any)
	if len(notes) != 2 {
		t.Fatalf("expected the whole segment attached, got %#v", item["Notes"])
	}
	if _, present := item["_id"]; present {
		t.Fatal("the _id helper inclusion must be stripped")
	}
}

// ─── cursors ─────────────────────────────────────────────────────────────────

func TestComposedReader_CursorsCarryTheComposedContext(t *testing.T) {
	env := newCVREnv()
	crit := queries.ReadCriteria{Filter: map[string]any{"Notes.Text": "b"}}
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full", crit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fullHash := queries.HashContext(crit.Filter, nil, "", false)
	if len(page.ItemCursors) == 0 {
		t.Fatal("expected item cursors")
	}
	cur, err := queries.DecodeCursor(page.ItemCursors[0])
	if err != nil {
		t.Fatalf("undecodable outgoing cursor: %v", err)
	}
	if cur.H != fullHash {
		t.Fatalf("outgoing cursors must be stamped with the composed context hash (segment filters included)")
	}

	// Round trip: the outgoing cursor navigates the same composed context.
	env2 := newCVREnv()
	crit2 := queries.ReadCriteria{Filter: map[string]any{"Notes.Text": "b"}, After: page.ItemCursors[0]}
	if _, err := env2.reader.ReadPage(context.Background(), "gadgets_full", crit2); err != nil {
		t.Fatalf("a round-tripped cursor must be accepted: %v", err)
	}

	// A cursor stamped against a DIFFERENT composed context is rejected.
	foreign, _ := queries.EncodeCursor([]any{"g1"}, "deadbeef")
	env3 := newCVREnv()
	crit3 := queries.ReadCriteria{Filter: map[string]any{"Notes.Text": "b"}, After: foreign}
	if _, err := env3.reader.ReadPage(context.Background(), "gadgets_full", crit3); err == nil {
		t.Fatal("a cursor from another listing context must be rejected")
	}
}

// ─── ReadByID ────────────────────────────────────────────────────────────────

func TestComposedReader_ReadByIDEnriches(t *testing.T) {
	env := newCVREnv()
	doc, found, err := env.reader.ReadByID(context.Background(), "gadgets_full", "g1", queries.ReadCriteria{})
	if err != nil || !found {
		t.Fatalf("unexpected result: found=%v err=%v", found, err)
	}
	mirror, _ := doc["UpstreamMirror"].(map[string]any)
	if mirror["Code"] != "UP-A" {
		t.Fatalf("expected the 1:1 segment attached, got %#v", doc["UpstreamMirror"])
	}
	notes, _ := doc["Notes"].([]any)
	if len(notes) != 2 {
		t.Fatalf("expected the 1:N segment attached, got %#v", doc["Notes"])
	}
}

func TestComposedReader_ReadByIDPassthrough(t *testing.T) {
	env := newCVREnv()
	doc, found, err := env.reader.ReadByID(context.Background(), "gadgets", "g1", queries.ReadCriteria{})
	if err != nil || !found {
		t.Fatalf("unexpected result: found=%v err=%v", found, err)
	}
	if _, attached := doc["UpstreamMirror"]; attached {
		t.Fatal("a regular by-id read must not be enriched")
	}
}

// ─── errors ──────────────────────────────────────────────────────────────────

func TestComposedReader_LegErrorPropagates(t *testing.T) {
	env := newCVREnv()
	env.notes.findErr = context.Canceled
	_, err := env.reader.ReadPage(context.Background(), "gadgets_full", queries.ReadCriteria{})
	if err == nil {
		t.Fatal("a leg fetch error must fail the read")
	}
}

// asInfra wraps errors.As without importing errors twice across files.
func asInfra(err error, target **core.InfrastructureError) bool {
	for err != nil {
		if ie, ok := err.(*core.InfrastructureError); ok {
			*target = ie
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ─── coverage of the narrower branches ───────────────────────────────────────

// cvrComposedByMirrorID joins the 1:1 leg through a NON-PK primary column, so
// the parent key is a Go field ("MirrorID") instead of _id.
func newCVREnvByMirrorID() *cvrEnv {
	env := &cvrEnv{
		gadgets: &filterColl{
			count: 2,
			docs: []bson.M{
				{"_id": "g1", "code": "A", "mirror_id": "m1"},
				{"_id": "g2", "code": "B", "mirror_id": nil},
			},
		},
		mirror: &filterColl{docs: []bson.M{{"_id": "m1", "code": "UP-A"}}},
		notes:  &filterColl{},
	}
	db := newFakeMongoFunc(func(name string) mongoColl {
		switch name {
		case "gadgets":
			return env.gadgets
		case "upstream_gadgets":
			return env.mirror
		}
		return &filterColl{}
	})
	primary := cvrPrimaryView()
	composed := query.ComposedView("gadgets_mirrored").
		Primary(primary).
		Link("upstreamMirror", query.JoinUpstream(cvrUpstreamSchema()).
			FK("mirror_id").
			As("UpstreamMirror"))
	inner := NewMongoViewReader(db).SetViews([]*query.ViewDefinition{primary})
	env.reader = NewComposedViewReader(inner, []*query.ComposedViewDefinition{composed}, 0)
	return env
}

func TestComposedReader_NonPKParentKey(t *testing.T) {
	env := newCVREnvByMirrorID()

	// Whole-doc read: the parent key is the MirrorID Go field; a nil FK value
	// yields the explicit null segment without ever querying for it.
	page, err := env.reader.ReadPage(context.Background(), "gadgets_mirrored", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mirror, _ := page.Items[0]["UpstreamMirror"].(map[string]any)
	if mirror["Code"] != "UP-A" {
		t.Fatalf("expected the mirror joined through MirrorID, got %#v", page.Items[0]["UpstreamMirror"])
	}
	if v, present := page.Items[1]["UpstreamMirror"]; !present || v != nil {
		t.Fatalf("expected an explicit null for the nil FK, got %#v", v)
	}

	// Inclusion projection without the FK field: the decorator includes it as
	// a helper and strips it afterwards.
	env2 := newCVREnvByMirrorID()
	page2, err := env2.reader.ReadPage(context.Background(), "gadgets_mirrored",
		queries.ReadCriteria{Projection: map[string]int{"Code": 1, "UpstreamMirror": 1, "_id": 0}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	item := page2.Items[0]
	if _, present := item["MirrorID"]; present {
		t.Fatal("the helper-included FK field must be stripped from the wire shape")
	}
	if _, present := item["UpstreamMirror"]; !present {
		t.Fatal("the requested segment must attach")
	}

	// Exclusion projection that excludes the FK field: the exclusion is
	// lifted for the join and restored afterwards.
	env3 := newCVREnvByMirrorID()
	page3, err := env3.reader.ReadPage(context.Background(), "gadgets_mirrored",
		queries.ReadCriteria{Projection: map[string]int{"MirrorID": 0}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	item3 := page3.Items[0]
	if _, present := item3["MirrorID"]; present {
		t.Fatal("the excluded FK field must not surface")
	}
	if m, _ := item3["UpstreamMirror"].(map[string]any); m["Code"] != "UP-A" {
		t.Fatalf("the join must still resolve, got %#v", item3["UpstreamMirror"])
	}
}

func TestComposedReader_AttachOneWithNoKeysSkipsQuery(t *testing.T) {
	env := newCVREnvByMirrorID()
	env.gadgets.docs = []bson.M{{"_id": "g2", "code": "B", "mirror_id": nil}}
	page, err := env.reader.ReadPage(context.Background(), "gadgets_mirrored", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.mirror.finds != 0 {
		t.Fatal("no parent carries a key — the leg query must be skipped")
	}
	if v, present := page.Items[0]["UpstreamMirror"]; !present || v != nil {
		t.Fatalf("expected the explicit null segment, got %#v", v)
	}
}

func TestComposedReader_ReadByIDNotFoundAndErrors(t *testing.T) {
	env := newCVREnv()
	if _, found, err := env.reader.ReadByID(context.Background(), "gadgets_full", "missing", queries.ReadCriteria{}); err != nil || found {
		t.Fatalf("expected a clean not-found, got found=%v err=%v", found, err)
	}
	env.notes.findErr = context.Canceled
	if _, _, err := env.reader.ReadByID(context.Background(), "gadgets_full", "g1", queries.ReadCriteria{}); err == nil {
		t.Fatal("a leg error must fail the by-id read")
	}
}

func TestComposedReader_CursorEdgeCases(t *testing.T) {
	// Malformed incoming cursor (with a composed context in play).
	env := newCVREnv()
	crit := queries.ReadCriteria{Filter: map[string]any{"Notes.Text": "b"}, After: "not-base64!"}
	if _, err := env.reader.ReadPage(context.Background(), "gadgets_full", crit); err == nil {
		t.Fatal("a malformed cursor must be rejected")
	}

	// Before path: the round-trip re-stamps through the same machinery.
	env2 := newCVREnv()
	first, err := env2.reader.ReadPage(context.Background(), "gadgets_full",
		queries.ReadCriteria{Filter: map[string]any{"Notes.Text": "b"}, Limit: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a next cursor on a truncated page")
	}
	env3 := newCVREnv()
	back := queries.ReadCriteria{Filter: map[string]any{"Notes.Text": "b"}, Limit: 1, Before: first.NextCursor}
	if _, err := env3.reader.ReadPage(context.Background(), "gadgets_full", back); err != nil {
		t.Fatalf("the before path must accept a composed cursor: %v", err)
	}

	// Without segment knobs the hashes coincide — cursors pass through the
	// wrapped reader untouched.
	env4 := newCVREnv()
	plain, err := env4.reader.ReadPage(context.Background(), "gadgets_full", queries.ReadCriteria{Limit: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cur, err := queries.DecodeCursor(plain.NextCursor)
	if err != nil {
		t.Fatalf("undecodable cursor: %v", err)
	}
	if cur.H != "" {
		t.Fatalf("the default context hash must stay canonical-empty, got %q", cur.H)
	}
}

// ─── regressions from the E2E round ──────────────────────────────────────────

// A partial inclusion into a 1:1 segment must not break the join: the attach
// step groups by the leg _id, so the column stays in the Mongo projection and
// is stripped from the wire shape afterwards.
func TestComposedReader_PartialMirrorProjectionStillJoins(t *testing.T) {
	env := newCVREnv()
	page, err := env.reader.ReadPage(context.Background(), "gadgets_full",
		queries.ReadCriteria{Projection: map[string]int{
			"Code":                1,
			"UpstreamMirror.Code": 1,
			"_id":                 0,
		}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mirror, ok := page.Items[0]["UpstreamMirror"].(map[string]any)
	if !ok || mirror["Code"] != "UP-A" {
		t.Fatalf("partial mirror projection lost the join: %#v", page.Items[0]["UpstreamMirror"])
	}
	if _, present := mirror["_id"]; present {
		t.Fatal("the grouping _id must be stripped from the wire shape")
	}
	// The Mongo projection carried _id as an INCLUSION (queryable for the join).
	folded := env.mirror.opts[0]
	proj, _ := folded.Projection.(bson.D)
	got := map[string]int{}
	for _, e := range proj {
		got[e.Key], _ = e.Value.(int)
	}
	if got["_id"] != 1 || got["code"] != 1 {
		t.Fatalf("unexpected mirror leg projection: %#v", folded.Projection)
	}
}

// SetComposedViews installs the composition by MUTATION on the shared reader
// instance — a handler that captured the reader BEFORE bootstrap wiring
// finished (e.g. a GraphQL field registered inside the consumer's Wire())
// must still resolve composed names.
func TestMongoViewReader_SetComposedViewsMutatesInPlace(t *testing.T) {
	env := &cvrEnv{
		gadgets: &filterColl{count: 1, docs: []bson.M{{"_id": "g1", "code": "A", "mirror_id": "m1"}}},
		mirror:  &filterColl{docs: []bson.M{{"_id": "g1", "code": "UP-A"}}},
		notes:   &filterColl{},
	}
	db := newFakeMongoFunc(func(name string) mongoColl {
		switch name {
		case "gadgets":
			return env.gadgets
		case "upstream_gadgets":
			return env.mirror
		case "gadget_notes":
			return env.notes
		}
		return &filterColl{}
	})
	primary, notes := cvrPrimaryView(), cvrNotesView()
	mvr := NewMongoViewReader(db).SetViews([]*query.ViewDefinition{primary, notes})

	// The early capture — the port value a handler stored before wiring.
	var captured queries.ViewReader = mvr

	// Composed reads BEFORE installation hit the regular path (empty unknown
	// collection → no items), proving the delegation is what changes behavior.
	pre, err := captured.ReadPage(context.Background(), "gadgets_full", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pre.Items) != 0 {
		t.Fatalf("expected no items before installation, got %d", len(pre.Items))
	}

	mvr.SetComposedViews([]*query.ComposedViewDefinition{cvrComposed(primary, notes)}, 0)

	page, err := captured.ReadPage(context.Background(), "gadgets_full", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected the composed read through the early capture, got %d items", len(page.Items))
	}
	mirror, _ := page.Items[0]["UpstreamMirror"].(map[string]any)
	if mirror["Code"] != "UP-A" {
		t.Fatalf("composed enrichment missing through the early capture: %#v", page.Items[0])
	}
	// Reset clears the delegation.
	mvr.SetComposedViews(nil, 0)
	post, err := captured.ReadPage(context.Background(), "gadgets_full", queries.ReadCriteria{})
	if err != nil || len(post.Items) != 0 {
		t.Fatalf("expected the reset to restore the regular path, got %d items err=%v", len(post.Items), err)
	}
}

// The maintainer's guarantee (2026-07-05): a security overlay layered in
// ToCriteria onto a PAGED query never breaks cursor navigation. The reader
// stamps cursors from — and validates them against — the post-ToCriteria
// context it receives, so as long as the overlay is deterministic per
// identity, the round trip holds; a genuinely changed context still rejects.
func TestMongoViewReader_OverlayFilterCursorRoundTrip(t *testing.T) {
	coll := &filterColl{
		count: 2,
		docs: []bson.M{
			{"_id": "g1", "code": "A", "mirror_id": "t1"},
			{"_id": "g2", "code": "A", "mirror_id": "t1"},
		},
	}
	primary := query.View("gadgets").Version(1).Root("gadgets").Schema(
		core.NewTableSchema[cvrGadget]("gadgets").
			PK("id").
			Field("Code", "code").
			Field("MirrorID", "mirror_id").
			SoftDelete("deleted_at"))
	r := NewMongoViewReader(newFakeMongo(coll)).SetViews([]*query.ViewDefinition{primary})

	// The criteria AS THE READER SEES IT: the wire filter (Code) plus a
	// ToCriteria security overlay (MirrorID standing in for a tenant column).
	overlaid := queries.ReadCriteria{
		Filter: map[string]any{"Code": "A", "MirrorID": "t1"},
		Limit:  1,
	}
	page, err := r.ReadPage(context.Background(), "gadgets", overlaid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor on the truncated page")
	}

	// Same overlaid context + the issued cursor → accepted.
	next := overlaid
	next.After = page.NextCursor
	if _, err := r.ReadPage(context.Background(), "gadgets", next); err != nil {
		t.Fatalf("overlay-stamped cursor must round-trip: %v", err)
	}

	// A genuinely changed context (different wire filter) → still rejected.
	changed := queries.ReadCriteria{
		Filter: map[string]any{"Code": "B", "MirrorID": "t1"},
		Limit:  1,
		After:  page.NextCursor,
	}
	if _, err := r.ReadPage(context.Background(), "gadgets", changed); err == nil {
		t.Fatal("a changed listing context must still reject the cursor")
	}
}
