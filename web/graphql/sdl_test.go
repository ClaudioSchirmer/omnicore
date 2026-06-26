package graphql

import (
	"reflect"
	"strings"
	"testing"
	"time"

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
	Name      *string `query:"name" filter:"eq,in,startswith"`
	Age       *int64  `query:"age" filter:"eq,gte,lte"`
	Addresses struct {
		ZipCode *string `query:"zipCode" filter:"eq"`
	} `query:"addresses"`
	Limit *int64 `query:"limit"`
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
	if f := conn.Fields.ForName("totalCount"); f == nil || f.Type.String() != "Int" {
		t.Errorf("UserConnection.totalCount = %v, want Int", f)
	}
}

func TestSDL_QueryParsesAndValidatesAgainstSchema(t *testing.T) {
	schema := buildReadSchema(t)
	query := `query {
	  users(where: { name: { startswith: "Bo" }, age: { gte: 18 } },
	        first: 10, orderBy: ["-name"], includeArchived: true) {
	    edges { node { id name age createdAt addresses { city zipCode } } cursor }
	    pageInfo { hasNextPage endCursor }
	    totalCount
	  }
	}`
	if _, errs := gqlparser.LoadQuery(schema, query); errs != nil {
		t.Fatalf("a well-formed query failed validation: %v", errs)
	}
}

func TestSDL_UnknownOperatorRejectedByValidation(t *testing.T) {
	schema := buildReadSchema(t)
	// `contains` was not declared on name (only eq,in,startswith) → validation error.
	query := `query { users(where: { name: { contains: "x" } }) { totalCount } }`
	_, errs := gqlparser.LoadQuery(schema, query)
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
