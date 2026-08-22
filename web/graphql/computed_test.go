package graphql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// COMPUTED-field parity on this surface. A computed field is declared on the
// RESPONSE DTO (`computed:"A,B"`, naming Result fields) and has no column
// behind it: the Query's FromQueryResult derives it after the read. The two
// consequences REST answers with `?fields=` pushdown and a schema violation on
// `?orderBy=` land here in the language's own idiom — the selection IS the
// projection, and the ordering vocabulary is an ENUM the computed field is
// simply not a member of, so gqlparser rejects an ordering by it during
// validation, before any resolver runs.

// ── fixture: a read whose Response carries one computed field ────────────────

// wsp is this file's own pointer helper, so the fixtures stay self-contained.
func wsp(s string) *string { return &s }

// widgetResult is the application-layer Result — pure data, no wire tags. It
// carries BOTH the computed field's sources (Name / Code) and the derived
// Display the Query's FromQueryResult fills in.
type widgetResult struct {
	ID      *string
	Name    *string
	Code    *string
	Display *string
}

type widgetQuery struct {
	pipeline.QueryBase
	crit queries.ReadCriteria
}

func (q *widgetQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.crit, nil
}

// FromQueryResult is the derivation seat: Display exists only here, composed
// from the two source fields the projection pushed down.
func (q *widgetQuery) FromQueryResult(_ *configuration.AppContext, r widgetResult) (widgetResult, error) {
	if r.Name != nil && r.Code != nil {
		display := *r.Name + " (" + *r.Code + ")"
		r.Display = &display
	}
	return r, nil
}

type widgetRequest struct {
	Name    *string `query:"name" filter:"eq" sort:"asc,desc"`
	First   *int64  `query:"first"`
	OrderBy *string `query:"orderBy"`
}

func (widgetRequest) ToQuery(crit queries.ReadCriteria) *widgetQuery {
	return &widgetQuery{crit: crit}
}

// widgetResponse declares `display` as COMPUTED over the Result's Name+Code.
// Code itself never reaches the wire — a source is mandatory on the Result,
// optional on the Response.
type widgetResponse struct {
	ID      *string `json:"id,omitempty"`
	Name    *string `json:"name,omitempty"`
	Display *string `json:"display,omitempty" computed:"Name,Code"`
}

func (widgetResponse) FromResult(r widgetResult) widgetResponse {
	return widgetResponse{ID: r.ID, Name: r.Name, Display: r.Display}
}

// widgetHandler runs the Result through FromQueryResult exactly as the
// framework's read handler does, so the derived value is what the resolver
// projects.
type widgetHandler struct {
	captured queries.ReadCriteria
	items    []widgetResult
}

func (h *widgetHandler) Handle(ctx *configuration.AppContext, q *widgetQuery) (queries.PageOf[widgetResult], error) {
	h.captured = q.crit
	out := make([]widgetResult, 0, len(h.items))
	for _, r := range h.items {
		derived, err := q.FromQueryResult(ctx, r)
		if err != nil {
			return queries.PageOf[widgetResult]{}, err
		}
		out = append(out, derived)
	}
	return queries.PageOf[widgetResult]{Items: out, ItemCursors: make([]string, len(out)), TotalCount: int64(len(out))}, nil
}

func newWidgetRegistry(h *widgetHandler) (*Registry, *configuration.AppContext) {
	reg := New(pipeline.New(translation.Default())).Register(
		QueryWithParams[widgetRequest]("widgets", "Widget", widgetResponse{}.FromResult, h),
	)
	return reg, configuration.NewAppContextWithRandomID(configuration.LangENG)
}

// enumBody returns the body of the named SDL enum, or "" when absent.
func enumBody(sdl, name string) string {
	head := "enum " + name + " {"
	i := strings.Index(sdl, head)
	if i < 0 {
		return ""
	}
	rest := sdl[i+len(head):]
	j := strings.Index(rest, "}")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestComputed_OrderFieldEnumOmitsComputedPath — the sortable enum is built
// from the Response's projection schema MINUS its computed paths, so the
// refusal is expressed in the schema itself. The field stays selectable: it is
// still a member of the node object.
func TestComputed_OrderFieldEnumOmitsComputedPath(t *testing.T) {
	reg, _ := newWidgetRegistry(&widgetHandler{})
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("SDL: %v", err)
	}
	body := enumBody(sdl, "WidgetOrderField")
	if body == "" {
		t.Fatalf("WidgetOrderField enum missing from SDL:\n%s", sdl)
	}
	if !strings.Contains(body, "NAME") {
		t.Errorf("sortable enum must carry the declared field, got %q", body)
	}
	if strings.Contains(body, "DISPLAY") {
		t.Errorf("a computed path must NOT be sortable, got %q", body)
	}
	if !strings.Contains(sdl, "display: String") {
		t.Errorf("a computed field must stay selectable on the node type:\n%s", sdl)
	}
}

// TestComputed_OrderFieldMapExcludesComputed — a computed path is not part of
// the ordering vocabulary because that vocabulary is the Request DTO's, and a
// derived value backs no column to order by.
func TestComputed_OrderFieldMapExcludesComputed(t *testing.T) {
	values, byValue := orderFieldMap("Widget", queryschema.ExtractRequestSchema(reflect.TypeOf(widgetRequest{})).Sortable)
	if _, sortable := byValue["DISPLAY"]; sortable {
		t.Errorf("computed path leaked into the order vocabulary: %v", values)
	}
	if _, sortable := byValue["NAME"]; !sortable {
		t.Errorf("stored paths must stay sortable: %v", values)
	}
}

// TestComputed_OrderByComputedFailsValidation — ordering by the computed field
// is rejected by gqlparser as an unknown enum member: this surface's native
// spelling of the schema violation REST answers with.
func TestComputed_OrderByComputedFailsValidation(t *testing.T) {
	h := &widgetHandler{}
	reg, ctx := newWidgetRegistry(h)
	resp := reg.Execute(ctx, `query { widgets(orderBy: [{field: DISPLAY}]) { edges { node { display } } } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("ordering by a computed field must be refused")
	}
	if !strings.Contains(resp.Errors[0].Message, "DISPLAY") {
		t.Errorf("the rejection must name the offending value, got %q", resp.Errors[0].Message)
	}
	if h.captured.OrderBy != nil {
		t.Errorf("the handler must never be reached: %+v", h.captured)
	}
	// The stored sibling still orders — only the computed member is missing.
	if resp := reg.Execute(ctx, `query { widgets(orderBy: [{field: NAME, direction: DESC}]) { totalCount } }`, nil, ""); len(resp.Errors) != 0 {
		t.Fatalf("ordering by a stored path must pass: %+v", resp.Errors)
	}
}

// TestComputed_SelectionPushesSources — selecting ONLY the computed field
// pushes its declared SOURCES to the store (Name + Code), never the computed
// Go path, which resolves to no column. The REST `?fields=display` pushdown,
// expressed as a selection set.
func TestComputed_SelectionPushesSources(t *testing.T) {
	h := &widgetHandler{}
	reg, ctx := newWidgetRegistry(h)
	if resp := reg.Execute(ctx, `query { widgets { edges { node { display } } } }`, nil, ""); len(resp.Errors) != 0 {
		t.Fatalf("selecting a computed field must resolve: %+v", resp.Errors)
	}
	want := queries.ProjectOnlyPaths("Name", "Code")
	if !reflect.DeepEqual(h.captured.Projection, want) {
		t.Fatalf("projection must carry the sources, got %+v", h.captured.Projection)
	}
}

// TestComputed_SelectionMixesSourcesWithStoredPaths — a mixed selection folds
// the computed sources in beside the stored paths, once each.
func TestComputed_SelectionMixesSourcesWithStoredPaths(t *testing.T) {
	h := &widgetHandler{}
	reg, ctx := newWidgetRegistry(h)
	if resp := reg.Execute(ctx, `query { widgets { edges { node { id name display } } } }`, nil, ""); len(resp.Errors) != 0 {
		t.Fatalf("mixed selection must resolve: %+v", resp.Errors)
	}
	want := queries.ProjectOnlyPaths("ID", "Name", "Code")
	if !reflect.DeepEqual(h.captured.Projection, want) {
		t.Fatalf("projection: %+v", h.captured.Projection)
	}
}

// TestComputed_ResolvesFromProjectedResponse — the derived value reaches the
// wire: the node field resolves from the projected Response (its json tags are
// the wire contract), exactly like every other field of the node.
func TestComputed_ResolvesFromProjectedResponse(t *testing.T) {
	h := &widgetHandler{items: []widgetResult{{ID: wsp("w-1"), Name: wsp("Drill"), Code: wsp("DRL")}}}
	reg, ctx := newWidgetRegistry(h)
	resp := reg.Execute(ctx, `query { widgets { edges { node { display } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("execute: %+v", resp.Errors)
	}
	edges := resp.Data["widgets"].(map[string]any)["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("edges: %+v", edges)
	}
	node := edges[0].(map[string]any)["node"].(map[string]any)
	if node["display"] != "Drill (DRL)" {
		t.Fatalf("computed value must render from the projected Response, got %v", node["display"])
	}
}
