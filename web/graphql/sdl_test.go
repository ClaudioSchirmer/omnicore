package graphql

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// ── sample DTOs mirroring a canonical read endpoint ─────────────────────────

type sdlAddress struct {
	City    *string `json:"city,omitempty"`
	ZipCode *string `json:"zipCode,omitempty"`
}

type sdlUserResponse struct {
	ID        *string      `json:"id,omitempty"`
	Name      *string      `json:"name,omitempty"`
	Age       *int64       `json:"age,omitempty"`
	CreatedAt *time.Time   `json:"createdAt,omitempty"`
	Addresses []sdlAddress `json:"addresses,omitempty"`
}

type sdlUserRequest struct {
	Name      *string `query:"name" filter:"eq,in,startswith" sort:"asc,desc"`
	Age       *int64  `query:"age" filter:"eq,gte,lte" sort:"asc,desc"`
	Addresses struct {
		ZipCode *string `query:"zipCode" filter:"eq" sort:"asc,desc"`
	} `query:"addresses"`
	First           *int64  `query:"first"`
	Last            *int64  `query:"last"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
	OrderBy         *string `query:"orderBy"`
}

// buildReadSchema assembles the SDL for one read field and loads it through
// gqlparser, returning the validated schema (or failing the test).
func buildReadSchema(t *testing.T) *ast.Schema {
	t.Helper()
	b := newSDLBuilder()
	field := b.queryFieldSDL("users", "User",
		reflect.TypeOf(sdlUserRequest{}), reflect.TypeOf(sdlUserResponse{}))
	doc := b.document("type Query {\n" + field + "\n}")

	schema, err := gqlparser.LoadSchema(&ast.Source{Name: "schema.graphql", Input: doc})
	if err != nil {
		t.Fatalf("generated SDL failed to load:\n%s\n\nerror: %v", doc, err)
	}
	return schema
}

func TestSDL_GeneratedSchemaLoads(t *testing.T) {
	schema := buildReadSchema(t)
	if schema.Query == nil {
		t.Fatal("schema has no Query root")
	}
	if schema.Query.Fields.ForName("users") == nil {
		t.Errorf("expected a 'users' query field, got %v", schema.Query.Fields)
	}
	// The node object carries wire-named fields, including the nested list.
	user := schema.Types["User"]
	if user == nil {
		t.Fatal("User type not registered")
	}
	for _, want := range []string{"id", "name", "age", "createdAt", "addresses"} {
		if user.Fields.ForName(want) == nil {
			t.Errorf("User missing wire field %q", want)
		}
	}
	if schema.Types["sdlAddress"] == nil {
		t.Error("nested address object type not registered")
	}
}

func TestSDL_WhereInputCarriesDeclaredOperators(t *testing.T) {
	schema := buildReadSchema(t)
	where := schema.Types["UserWhereInput"]
	if where == nil {
		t.Fatal("UserWhereInput not registered")
	}
	// Flat leaf + nested embed leaf both surface (dotted path flattened).
	for _, want := range []string{"name", "age", "addresses_zipCode"} {
		if where.Fields.ForName(want) == nil {
			t.Errorf("UserWhereInput missing leaf %q", want)
		}
	}
	nameOp := schema.Types["User_name_Op"]
	if nameOp == nil {
		t.Fatal("User_name_Op operator input not registered")
	}
	for _, op := range []string{"eq", "in", "startswith"} {
		if nameOp.Fields.ForName(op) == nil {
			t.Errorf("User_name_Op missing operator %q", op)
		}
	}
	// `in` is a list; `eq` is a scalar.
	if got := nameOp.Fields.ForName("in").Type.String(); got != "[String!]" {
		t.Errorf("in operator type = %q, want [String!]", got)
	}
	if got := nameOp.Fields.ForName("eq").Type.String(); got != "String" {
		t.Errorf("eq operator type = %q, want String", got)
	}
}

func TestSDL_ConnectionAndPageInfo(t *testing.T) {
	schema := buildReadSchema(t)
	for _, want := range []string{"UserConnection", "UserEdge", "PageInfo"} {
		if schema.Types[want] == nil {
			t.Errorf("expected %q type", want)
		}
	}
	conn := schema.Types["UserConnection"]
	if f := conn.Fields.ForName("edges"); f == nil || f.Type.String() != "[UserEdge!]!" {
		t.Errorf("UserConnection.edges = %v, want [UserEdge!]!", f)
	}
	if f := conn.Fields.ForName("totalCount"); f == nil || f.Type.String() != "Int!" {
		t.Errorf("UserConnection.totalCount = %v, want Int! (always populated, GitHub-parity)", f)
	}
}

func TestSDL_QueryParsesAndValidatesAgainstSchema(t *testing.T) {
	schema := buildReadSchema(t)
	query := `query {
	  users(where: { name: { startswith: "Bo" }, age: { gte: 18 } },
	        first: 10, orderBy: [{field: NAME, direction: DESC}, {field: AGE}],
	        includeArchived: true) {
	    edges { node { id name age createdAt addresses { city zipCode } } cursor }
	    pageInfo { hasNextPage endCursor }
	    totalCount
	  }
	}`
	if _, errs := gqlparser.LoadQueryWithRules(schema, query, nil); errs != nil {
		t.Fatalf("a well-formed query failed validation: %v", errs)
	}
}

func TestSDL_UnknownOperatorRejectedByValidation(t *testing.T) {
	schema := buildReadSchema(t)
	// `contains` was not declared on name (only eq,in,startswith) → validation error.
	query := `query { users(where: { name: { contains: "x" } }) { totalCount } }`
	_, errs := gqlparser.LoadQueryWithRules(schema, query, nil)
	if errs == nil {
		t.Fatal("expected validation to reject an undeclared operator")
	}
	if !strings.Contains(errs.Error(), "contains") {
		t.Errorf("expected the error to name the bad operator, got: %v", errs)
	}
}

func TestSDL_DateTimeScalarDeclaredWhenUsed(t *testing.T) {
	b := newSDLBuilder()
	_ = b.queryFieldSDL("users", "User",
		reflect.TypeOf(sdlUserRequest{}), reflect.TypeOf(sdlUserResponse{}))
	doc := b.document("type Query {\n  _x: Int\n}")
	if !strings.Contains(doc, "scalar DateTime") {
		t.Errorf("expected `scalar DateTime` to be declared (CreatedAt uses it):\n%s", doc)
	}
}

// TestSDL_OrderByEnumReflectsSortableVocabulary — the orderBy argument is the
// typed `[UserOrder!]` over the reflected sortable-field enum: one enum value
// per Response wire path (the SAME allowlist REST's ?orderBy= validates),
// SCREAMING_SNAKE with nested paths flattened, plus the shared OrderDirection
// and the `direction = ASC` default.
func TestSDL_OrderByEnumReflectsSortableVocabulary(t *testing.T) {
	schema := buildReadSchema(t)
	users := schema.Query.Fields.ForName("users")
	if arg := users.Arguments.ForName("orderBy"); arg == nil || arg.Type.String() != "[UserOrder!]" {
		t.Fatalf("orderBy argument = %v, want [UserOrder!]", users.Arguments.ForName("orderBy"))
	}
	fieldEnum := schema.Types["UserOrderField"]
	if fieldEnum == nil {
		t.Fatal("UserOrderField enum not registered")
	}
	got := map[string]bool{}
	for _, v := range fieldEnum.EnumValues {
		got[v.Name] = true
	}
	// The enum is EXACTLY what the Request DTO declared orderable — not every
	// path the Response happens to render. A leaf without the tag (and an
	// embed group, which carries no value) stays out.
	want := map[string]bool{"NAME": true, "AGE": true, "ADDRESSES_ZIP_CODE": true}
	for v := range want {
		if !got[v] {
			t.Errorf("UserOrderField missing value %q (have %v)", v, got)
		}
	}
	for v := range got {
		if !want[v] {
			t.Errorf("UserOrderField advertises %q, which the DTO did not declare orderable", v)
		}
	}
	dir := schema.Types["OrderDirection"]
	if dir == nil || dir.EnumValues.ForName("ASC") == nil || dir.EnumValues.ForName("DESC") == nil {
		t.Fatalf("OrderDirection enum malformed: %v", dir)
	}
	order := schema.Types["UserOrder"]
	if order == nil {
		t.Fatal("UserOrder input not registered")
	}
	if f := order.Fields.ForName("field"); f == nil || f.Type.String() != "UserOrderField!" {
		t.Errorf("UserOrder.field = %v, want UserOrderField!", f)
	}
	d := order.Fields.ForName("direction")
	if d == nil || d.Type.String() != "OrderDirection" || d.DefaultValue == nil || d.DefaultValue.Raw != "ASC" {
		t.Errorf("UserOrder.direction must be OrderDirection with default ASC, got %v", d)
	}
}

// TestOrderEnumValue_Conversions pins the wire-path → enum-value derivation.
func TestOrderEnumValue_Conversions(t *testing.T) {
	for wire, want := range map[string]string{
		"id":                "ID",
		"name":              "NAME",
		"userName":          "USER_NAME",
		"addresses.zipCode": "ADDRESSES_ZIP_CODE",
		"createdAt":         "CREATED_AT",
	} {
		if got := orderEnumValue(wire); got != want {
			t.Errorf("orderEnumValue(%q) = %q, want %q", wire, got, want)
		}
	}
}

// TestOrderFieldMap_CollisionPanics — two wire names folding onto one enum
// value is a Request modeling error caught at boot.
func TestOrderFieldMap_CollisionPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a collision panic")
		}
	}()
	orderFieldMap("User", map[string]queryschema.SortSpec{
		"userName":  {GoPath: "UserName", Asc: true},
		"user_name": {GoPath: "UserName", Asc: true},
	})
}
