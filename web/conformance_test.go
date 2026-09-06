package web_test

// The read vocabulary has five consumers — the REST listing, the CSV/XLSX
// export, the OpenAPI document, the GraphQL connection and the gRPC procedure —
// and one Request DTO that answers, for all of them, what the endpoint accepts.
// Each surface owns only how its own wire SPELLS a value and how it renders a
// refusal; none of them owns an opinion about what exists.
//
// That is an invariant, and this file is where it is proven rather than
// assumed. Every case below is ONE logical request expressed in each idiom; the
// suite asserts the surfaces reach the SAME verdict. It exists because the
// divergences that shipped were never visible from one surface: a boolean
// spelling the export took and the listing refused, an ordering path gRPC could
// not resolve while REST resolved it fine, a cursor GraphQL accepted and REST
// rejected, a control the gRPC by-id ignored and the REST by-id refused. Each
// of those was one surface answering a question that was never its to answer.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
	fwweb "github.com/ClaudioSchirmer/omnicore/web"
	fwgraphql "github.com/ClaudioSchirmer/omnicore/web/graphql"
	fwgrpc "github.com/ClaudioSchirmer/omnicore/web/grpc"
	pb "github.com/ClaudioSchirmer/omnicore/web/grpc/pb"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// ─── one endpoint, declared once ─────────────────────────────────────────────

// conformRequest carries all three legal shapes of a query-tagged scalar, so
// every surface is asked about each of them:
//
//	code      filterable AND orderable, both directions
//	name      filterable only
//	createdAt orderable only, DESC only — a vocabulary leaf: it names an
//	          ordering path, takes no value on the wire, and is a column the
//	          Response does not even render
type conformRequest struct {
	Code      *string `query:"code" filter:"eq,in,startswith" sort:"asc,desc"`
	Name      *string `query:"name" filter:"eq,icontains"`
	CreatedAt *string `query:"createdAt" sort:"desc"`

	First           *int64  `query:"first"`
	Last            *int64  `query:"last"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	Fields          *string `query:"fields"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
	OrderBy         *string `query:"orderBy"`
}

func (r conformRequest) ToQuery(c queries.ReadCriteria) *conformQuery {
	return &conformQuery{Criteria: c}
}

type conformResult struct {
	ID   *string
	Code *string
	Name *string
}

type conformResponse struct {
	ID   *string `json:"id,omitempty"`
	Code *string `json:"code,omitempty"`
	Name *string `json:"name,omitempty"`
}

func (conformResponse) FromResult(r conformResult) conformResponse {
	return conformResponse{ID: r.ID, Code: r.Code, Name: r.Name}
}

type conformQuery struct {
	pipeline.QueryBase
	Criteria queries.ReadCriteria
}

func (q *conformQuery) ToCriteria(*configuration.AppContext) (queries.ReadCriteria, error) {
	return q.Criteria, nil
}

func (q *conformQuery) FromQueryResult(_ *configuration.AppContext, r conformResult) (conformResult, error) {
	return r, nil
}

type conformHandler struct{}

func (h *conformHandler) Handle(*configuration.AppContext, *conformQuery) (queries.PageOf[conformResult], error) {
	return queries.PageOf[conformResult]{}, nil
}

// ─── the verdict a surface reached ───────────────────────────────────────────

// verdict is what a surface decided about one request, reduced to the part that
// must be identical everywhere: was it accepted, and if not, WHICH declaration
// was refused. The rendering around that answer is each surface's own — REST
// puts `orderBy[<token>]` on a 400 envelope, gRPC drops the bracket because
// order_by is already a typed field there, GraphQL carries it in an extension —
// so the comparison is on the answer, not on the prose.
type verdict struct {
	accepted bool
	field    string
}

func (v verdict) String() string {
	if v.accepted {
		return "accepted"
	}
	return "refused(" + v.field + ")"
}

// foldToDeclaration reduces a surface's refusal to the DECLARATION it named.
//
// The spelling around it is each surface's own and is supposed to differ: the
// same ordering leaf is `createdAt` on a query string, `CREATED_AT` in a
// GraphQL enum and `created_at` on the proto wire, and REST wraps an ordering
// token as `orderBy[<token>]` while gRPC does not (order_by is already a typed
// field there). What must be identical is WHICH declaration was refused, so
// that is what this compares — a surface reporting a different declaration has
// answered a different question.
func foldToDeclaration(schema *queryschema.RequestSchema, field string) string {
	if token, wrapped := queryschema.OrderByToken(field); wrapped {
		field = token
	}
	field = strings.TrimPrefix(field, "-")
	// A composite control refusal (`onlyTotal[orderBy]`) names two controls and
	// is spelled identically everywhere.
	if strings.Contains(field, "[") {
		return field
	}
	// An operator rides on the key only on a query string; the declaration is
	// the leaf before it.
	if path, _ := queryschema.ParseKeyAgainstSchema(field, schema); path != "" {
		field = path
	}
	want := foldSpelling(field)
	for path := range schema.Filters {
		if foldSpelling(path) == want {
			return path
		}
	}
	for path := range schema.Sortable {
		if foldSpelling(path) == want {
			return path
		}
	}
	for key := range queryschema.ControlKeys {
		if foldSpelling(key) == want {
			return key
		}
	}
	return field
}

// foldSpelling drops the two things that separate one wire's spelling of a name
// from another's: word separators and case.
func foldSpelling(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || r == '-' || r == '.' {
			continue
		}
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ─── the four request decoders, each in its own idiom ────────────────────────

// listCase is one logical request. Each field expresses it on one wire; the
// shapes differ because the wires differ, and that is exactly the part a
// surface owns.
type listCase struct {
	name string
	// rest is the query string (without the leading `?`).
	rest string
	// gql is the argument list of the connection field.
	gql string
	// grpc builds the same request on the proto plane.
	grpc func(*fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder
	// want is the verdict every surface must reach.
	want verdict
	// restOnly marks a case whose idiom exists only on the query string (an
	// operator packed into the key, a boolean spelled as text). The other
	// wires cannot express it at all, which is a stronger guarantee than
	// agreeing about it.
	restOnly bool
}

func newPipeline() *pipeline.Pipeline { return pipeline.New(translation.Default()) }

// restVerdict drives the canonical listing route and reads the answer off the
// canonical envelope.
func restVerdict(t *testing.T, query string) verdict {
	t.Helper()
	app := fiber.New()
	app.Get("/items", fwweb.QueryWithParams(newPipeline(), conformRequest{},
		conformResponse{}.FromResult, &conformHandler{}))
	return httpVerdict(t, app, "/items?"+query)
}

// exportVerdict drives the CSV sibling of the same endpoint. It shares the DTO,
// so it must share the answer — except for the pagination keys, which the
// export documents as no-ops.
func exportVerdict(t *testing.T, query string) verdict {
	t.Helper()
	app := fiber.New()
	app.Get("/items.csv", fwweb.QueryAsCSV(newPipeline(), conformRequest{},
		conformResponse{}.FromResult, exportView{}, fwweb.ExportDeps{Translator: translation.Default()},
		&conformHandler{}))
	return httpVerdict(t, app, "/items.csv?"+query)
}

type exportView struct{}

func (exportView) Name() string                              { return "items" }
func (exportView) ResolveMaxExportRows(fallback int64) int64 { return fallback }

func httpVerdict(t *testing.T, app *fiber.App, url string) verdict {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", url, nil))
	if err != nil {
		t.Fatalf("app.Test(%s): %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusBadRequest {
		return verdict{accepted: true}
	}
	var parsed struct {
		Errors []struct {
			Messages []struct {
				Field string `json:"field"`
			} `json:"messages"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil ||
		len(parsed.Errors) == 0 || len(parsed.Errors[0].Messages) == 0 {
		t.Fatalf("a refusal must carry the canonical envelope, got %s", body)
	}
	return verdict{field: foldToDeclaration(conformSchema, parsed.Errors[0].Messages[0].Field)}
}

// graphqlVerdict drives the connection field of the same endpoint.
func graphqlVerdict(t *testing.T, args string) verdict {
	t.Helper()
	pipe := newPipeline()
	reg := fwgraphql.New(pipe).Register(
		fwgraphql.QueryWithParams[conformRequest]("items", "Item",
			conformResponse{}.FromResult, &conformHandler{}),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	q := "{ items"
	if args != "" {
		q += "(" + args + ")"
	}
	q += " { edges { node { code } } } }"
	resp := reg.Execute(ctx, q, nil, "")
	if len(resp.Errors) == 0 {
		return verdict{accepted: true}
	}
	field, _ := resp.Errors[0].Extensions["field"].(string)
	if field == "" {
		// The SDL cut it before any resolver ran — gqlparser names the
		// argument in prose. That is still a refusal, and the case table says
		// which declaration it is about.
		return verdict{field: "<schema>"}
	}
	return verdict{field: foldToDeclaration(conformSchema, field)}
}

// grpcVerdict drives the proto plane through the builder the Auto path and
// MountRaw share.
func grpcVerdict(t *testing.T, build func(*fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder) verdict {
	t.Helper()
	schema := queryschema.ExtractRequestSchema(reflect.TypeOf(conformRequest{}))
	b := fwgrpc.NewCriteria().
		Fields(map[string]string{"id": "ID", "code": "Code", "name": "Name"}).
		Sortable(schema.Sortable)
	if _, err := build(b).Build(); err != nil {
		return verdict{field: foldToDeclaration(conformSchema, grpcRefusedField(t, err))}
	}
	return verdict{accepted: true}
}

// grpcRefusedField unwraps the typed notification the proto plane refuses with.
func grpcRefusedField(t *testing.T, err error) string {
	t.Helper()
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("a gRPC refusal must be a typed notification, got %T: %v", err, err)
	}
	for _, nctx := range carrier.NotificationContexts() {
		for _, msg := range nctx.Messages() {
			return msg.ResolveFieldName()
		}
	}
	t.Fatalf("a gRPC refusal must name a field: %v", err)
	return ""
}

// ─── the table ───────────────────────────────────────────────────────────────

func conformanceCases() []listCase {
	orderBy := func(terms ...*pb.OrderByField) func(*fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder {
		return func(b *fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder { return b.OrderBy(terms...) }
	}
	page := func(p *pb.PaginationRequest) func(*fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder {
		return func(b *fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder { return b.Page(p) }
	}
	noop := func(b *fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder { return b }
	i64 := func(n int64) *int64 { return &n }

	return []listCase{
		// ── the ordering vocabulary ────────────────────────────────────────
		{
			name: "a declared path in a declared direction is accepted",
			rest: "orderBy=code",
			gql:  "orderBy: [{field: CODE}]",
			grpc: orderBy(&pb.OrderByField{Field: "code"}),
			want: verdict{accepted: true},
		},
		{
			name: "a vocabulary leaf orders by a column the Response never renders",
			rest: "orderBy=-createdAt",
			gql:  "orderBy: [{field: CREATED_AT, direction: DESC}]",
			grpc: orderBy(&pb.OrderByField{Field: "created_at", Desc: true}),
			want: verdict{accepted: true},
		},
		{
			name: "a direction the declaration does not admit is refused",
			rest: "orderBy=createdAt",
			gql:  "orderBy: [{field: CREATED_AT}]",
			grpc: orderBy(&pb.OrderByField{Field: "created_at"}),
			want: verdict{field: "createdAt"},
		},
		{
			name: "a path named twice is refused on the second occurrence",
			rest: "orderBy=code,-code",
			gql:  "orderBy: [{field: CODE}, {field: CODE, direction: DESC}]",
			grpc: orderBy(&pb.OrderByField{Field: "code"}, &pb.OrderByField{Field: "code", Desc: true}),
			want: verdict{field: "code"},
		},
		{
			name: "a filterable-but-not-orderable path is refused",
			rest: "orderBy=name",
			gql:  "orderBy: [{field: NAME}]",
			grpc: orderBy(&pb.OrderByField{Field: "name"}),
			want: verdict{field: "name"},
		},
		{
			name: "distinct paths in one ordering stay legal",
			rest: "orderBy=code,-createdAt",
			gql:  "orderBy: [{field: CODE}, {field: CREATED_AT, direction: DESC}]",
			grpc: orderBy(&pb.OrderByField{Field: "code"}, &pb.OrderByField{Field: "created_at", Desc: true}),
			want: verdict{accepted: true},
		},

		// ── the directional rule ───────────────────────────────────────────
		{
			name: "forward and backward together are refused",
			rest: "first=2&last=3",
			gql:  "first: 2, last: 3",
			grpc: page(&pb.PaginationRequest{First: i64(2), Last: i64(3)}),
			want: verdict{field: "last"},
		},
		{
			name: "a non-positive page size is refused",
			rest: "first=0",
			gql:  "first: 0",
			grpc: page(&pb.PaginationRequest{First: i64(0)}),
			want: verdict{field: "first"},
		},

		// ── the only-total conflict matrix ─────────────────────────────────
		{
			name: "only-total beside an ordering is refused",
			rest: "onlyTotal=true&orderBy=code",
			gql:  "", // expressed by the selection shape, not an argument
			grpc: func(b *fwgrpc.CriteriaBuilder) *fwgrpc.CriteriaBuilder {
				return b.Page(&pb.PaginationRequest{OnlyTotal: boolPtr(true)}).
					OrderBy(&pb.OrderByField{Field: "code"})
			},
			want:     verdict{field: "onlyTotal[orderBy]"},
			restOnly: true,
		},

		// ── the cursor's structure ─────────────────────────────────────────
		{
			name: "a cursor that does not decode is refused",
			rest: "after=not-a-cursor",
			gql:  `after: "not-a-cursor"`,
			grpc: page(&pb.PaginationRequest{After: strPtr("not-a-cursor")}),
			want: verdict{field: "after"},
		},

		// ── the filter allowlist ───────────────────────────────────────────
		{
			name: "a declared operator on a declared leaf is accepted",
			rest: "code.startswith=A",
			gql:  `where: {code: {startswith: "A"}}`,
			grpc: noop, // the proto plane declares its filters by calling for them
			want: verdict{accepted: true},
		},
		{
			name:     "an operator outside the leaf's declaration is refused",
			rest:     "name.startswith=A",
			want:     verdict{field: "name"},
			restOnly: true,
		},
		{
			name:     "a key no leaf declares is refused",
			rest:     "bogus=1",
			want:     verdict{field: "bogus"},
			restOnly: true,
		},
		{
			name:     "the vocabulary leaf carries no value on the wire",
			rest:     "createdAt=2020-01-01",
			want:     verdict{field: "createdAt"},
			restOnly: true,
		},

		// ── the boolean controls ───────────────────────────────────────────
		{
			name:     "a boolean spelled outside true/false is refused",
			rest:     "includeArchived=1",
			want:     verdict{field: "includeArchived"},
			restOnly: true,
		},
		{
			name:     "a boolean present with no value is refused",
			rest:     "includeArchived=",
			want:     verdict{field: "includeArchived"},
			restOnly: true,
		},
		{
			name:     "the false spelling is a no-op, not a refusal",
			rest:     "includeArchived=false",
			want:     verdict{accepted: true},
			restOnly: true,
		},
	}
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// ─── the invariant, asserted ─────────────────────────────────────────────────

// TestConformance_EverySurfaceReachesTheSameVerdict is the point of the file.
// One logical request, four idioms, one answer. A surface that disagrees has
// taken a decision that is not its to take.
func TestConformance_EverySurfaceReachesTheSameVerdict(t *testing.T) {
	for _, tc := range conformanceCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := restVerdict(t, tc.rest)
			if got != tc.want {
				t.Fatalf("REST listing: got %s, want %s", got, tc.want)
			}
			if tc.restOnly {
				return
			}
			if got := graphqlVerdict(t, tc.gql); !sameVerdict(got, tc.want) {
				t.Errorf("GraphQL: got %s, want %s", got, tc.want)
			}
			if got := grpcVerdict(t, tc.grpc); got != tc.want {
				t.Errorf("gRPC: got %s, want %s", got, tc.want)
			}
		})
	}
}

// sameVerdict compares a GraphQL answer, allowing the one refusal this surface
// makes EARLIER than the others: an argument the SDL never declared is cut by
// schema validation, before any resolver runs. That is a stronger refusal, not
// a different one.
func sameVerdict(got, want verdict) bool {
	if got.accepted != want.accepted {
		return false
	}
	return got.field == want.field || got.field == "<schema>"
}

// TestConformance_ExportAnswersLikeItsListing — the export shares the DTO, so it
// must share the answer. Its only licence is the pagination keys, which it
// documents as no-ops (an export streams the full filtered set), and that
// licence is asserted rather than assumed.
func TestConformance_ExportAnswersLikeItsListing(t *testing.T) {
	ignored := map[string]bool{"first": true, "last": true, "after": true, "before": true, "onlyTotal": true}
	for _, tc := range conformanceCases() {
		t.Run(tc.name, func(t *testing.T) {
			key, _, _ := strings.Cut(tc.rest, "=")
			key, _, _ = strings.Cut(key, ".")
			if ignored[key] || ignored[tc.want.field] {
				if got := exportVerdict(t, tc.rest); !got.accepted {
					t.Fatalf("a documented no-op must not refuse: got %s", got)
				}
				return
			}
			if got := exportVerdict(t, tc.rest); got != tc.want {
				t.Fatalf("export: got %s, want %s (the listing says %s)", got, tc.want, tc.want)
			}
		})
	}
}

// TestConformance_TheDocumentAdvertisesWhatTheParserAccepts closes the loop on
// the fifth surface. OpenAPI decodes no request, so its conformance is a
// different sentence with the same meaning: every parameter it advertises must
// be accepted by the route, and nothing the route refuses may be advertised.
//
// This is the assertion that would have caught the vocabulary leaf being
// published as a query parameter — a promise the parser answered with 400 on
// every call.
func TestConformance_TheDocumentAdvertisesWhatTheParserAccepts(t *testing.T) {
	reg := openapi.NewRegistry()
	openapi.Mount(reg, fiber.New(), fiber.MethodGet, "/items",
		func(fiber.Ctx) error { return nil },
		openapi.RouteSpec{
			RequestType:   reflect.TypeOf(conformRequest{}),
			ResponseType:  reflect.TypeOf(conformResponse{}),
			SuccessStatus: fiber.StatusOK,
			Paged:         true,
		}, openapi.Doc{Summary: "conformance"})

	raw, err := openapi.NewSpec(openapi.Config{Title: "t", Version: "1"}, reg).Build()
	if err != nil {
		t.Fatalf("build the document: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the document must be JSON: %v", err)
	}
	advertised := advertisedQueryParams(t, doc, "/items")
	if len(advertised) == 0 {
		t.Fatal("the document must advertise the endpoint's parameters")
	}
	for _, name := range advertised {
		// A parameter is a promise: send it, and the route must take it.
		if got := restVerdict(t, name+"="+sampleValueFor(name)); !got.accepted {
			t.Errorf("the document advertises %q, but the route answers %s", name, got)
		}
	}
	// And the reverse: the vocabulary leaf names an ordering path, takes no
	// value, and must therefore never appear as a parameter.
	for _, name := range advertised {
		if name == "createdAt" {
			t.Error("an ordering-only leaf must not be advertised as a query parameter")
		}
	}
}

func advertisedQueryParams(t *testing.T, doc map[string]any, path string) []string {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	entry, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("the document has no %s", path)
	}
	op, _ := entry["get"].(map[string]any)
	params, _ := op["parameters"].([]any)
	var out []string
	for _, raw := range params {
		p, _ := raw.(map[string]any)
		if p["in"] == "query" {
			out = append(out, p["name"].(string))
		}
	}
	return out
}

// sampleValueFor is a legal value for each advertised parameter: the point of
// the assertion is that the KEY is accepted, so the value must not be what
// fails.
func sampleValueFor(name string) string {
	switch {
	case name == "first" || name == "last":
		return "1"
	case name == "includeArchived" || name == "onlyTotal":
		return "false"
	case name == "orderBy":
		return "code"
	case name == "fields":
		return "code"
	case name == "after" || name == "before":
		return validCursor
	default:
		return "x"
	}
}

// validCursor is a well-formed cursor for an unordered read: the key tuple is
// `_id` alone, which is what len(OrderBy)==0 requires.
var validCursor = func() string {
	c, err := queries.EncodeCursor([]any{"id"}, "")
	if err != nil {
		panic("conformance: " + err.Error())
	}
	return c
}()

// conformSchema is the endpoint's declaration — the thing every surface answers
// from, and the dictionary the fold above resolves a refusal against.
var conformSchema = queryschema.ExtractRequestSchema(reflect.TypeOf(conformRequest{}))

// ─── the vocabulary is the DTO's, and stays the DTO's ────────────────────────

// vocabularySnapshot renders everything a Request DTO declares — the filter
// keys with their operators, the control opt-ins, the ordering vocabulary with
// its directions — as one deterministic string. It is "what this endpoint
// accepts", written down.
func vocabularySnapshot(s *queryschema.RequestSchema) string {
	var b strings.Builder
	keys := make([]string, 0, len(s.Filters))
	for k := range s.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ops := make([]string, 0, len(s.Filters[k].Ops))
		for op := range s.Filters[k].Ops {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		b.WriteString("filter " + k + "=" + strings.Join(ops, "|") + "\n")
	}
	reserved := make([]string, 0, len(s.Reserved))
	for k := range s.Reserved {
		reserved = append(reserved, k)
	}
	sort.Strings(reserved)
	b.WriteString("controls " + strings.Join(reserved, "|") + "\n")
	sortable := make([]string, 0, len(s.Sortable))
	for k := range s.Sortable {
		sortable = append(sortable, k)
	}
	sort.Strings(sortable)
	for _, k := range sortable {
		spec := s.Sortable[k]
		b.WriteString(fmt.Sprintf("sort %s asc=%v desc=%v\n", k, spec.Asc, spec.Desc))
	}
	return b.String()
}

// sweepEverySurface runs the whole conformance table on every surface and
// returns the verdicts, keyed by case name and surface.
func sweepEverySurface(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tc := range conformanceCases() {
		out[tc.name+"/rest"] = restVerdict(t, tc.rest).String()
		out[tc.name+"/export"] = exportVerdict(t, tc.rest).String()
		if tc.restOnly {
			continue
		}
		out[tc.name+"/graphql"] = graphqlVerdict(t, tc.gql).String()
		out[tc.name+"/grpc"] = grpcVerdict(t, tc.grpc).String()
	}
	return out
}

// TestConformance_NoSurfaceChangesWhatTheEndpointAccepts is the invariant
// behind every case above, stated once about the schema itself.
//
// A reflected Request schema is memoized per type and shared by all five
// consumers and by every request each of them serves. It is the DTO's answer to
// "what does this endpoint accept" — so a REQUEST is something that answer
// judges, never something that extends it. One surface that records into it
// makes the endpoint's contract depend on traffic history: after a gRPC call,
// REST accepted a key it had refused a moment earlier, and no document said so.
//
// The table above cannot see that, because each case asks one question once.
// This asks whether ASKING changed the answer.
func TestConformance_NoSurfaceChangesWhatTheEndpointAccepts(t *testing.T) {
	schema := queryschema.ExtractRequestSchema(reflect.TypeOf(conformRequest{}))
	before := vocabularySnapshot(schema)

	first := sweepEverySurface(t)

	if after := vocabularySnapshot(schema); after != before {
		t.Fatalf("a request changed what the endpoint accepts:\n--- before\n%s--- after\n%s", before, after)
	}

	// The consequence, asserted where a consumer would feel it: the same
	// question a second time gets the same answer.
	for name, second := range sweepEverySurface(t) {
		if first[name] != second {
			t.Errorf("%s: verdict changed between two identical requests — %s then %s", name, first[name], second)
		}
	}
}
