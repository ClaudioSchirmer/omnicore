package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// `__typename` is the GraphQL spec's String! meta-field: every object type
// answers it in every selection set, with the name of the type the selection is
// made on. It is not introspection — EnableIntrospection governs `__schema` /
// `__type` only — so both tests below pin the behavior with the flag OFF, which
// is the default posture an operator runs in.

// TestExecute_RootTypenameAnswersTheRootType covers the root position, which
// needs no fixture at all: an empty registry still answers `{ __typename }`
// with the operation's root type name, and answers it identically whether or
// not introspection is enabled.
func TestExecute_RootTypenameAnswersTheRootType(t *testing.T) {
	for _, introspection := range []bool{false, true} {
		reg := New(nil).EnableIntrospection(introspection)

		resp := reg.Execute(nil, `{ __typename }`, nil, "")

		if len(resp.Errors) != 0 {
			t.Fatalf("introspection=%v: unexpected errors: %+v", introspection, resp.Errors)
		}
		if got := resp.Data["__typename"]; got != "Query" {
			t.Errorf("introspection=%v: __typename = %v, want Query", introspection, got)
		}
	}
}

// TestExecute_NestedTypenameAnswersEachSelectedType covers the position that
// matters for real clients: Apollo, urql and Relay append `__typename` to every
// selection set for cache normalization (Apollo's normalized cache keys on
// `__typename` + `id`). Each level of a connection must answer its OWN type —
// the connection, the edge, the node and the shared PageInfo — and an alias
// must carry the value under the alias key.
func TestExecute_NestedTypenameAnswersEachSelectedType(t *testing.T) {
	h := &fakeReadHandler{page: queries.PageOf[execResult]{
		Items:       []execResult{{ID: sp("u1"), Name: sp("alice")}},
		ItemCursors: []string{"c1"},
		TotalCount:  1,
	}}
	reg, ctx := newExecRegistry(h) // introspection off — the default

	query := `{
	  users(first: 1) {
	    __typename
	    edges { __typename node { __typename name } }
	    pageInfo { kind: __typename }
	  }
	}`
	resp := reg.Execute(ctx, query, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}

	users := resp.Data["users"].(map[string]any)
	if users["__typename"] != "UserConnection" {
		t.Errorf("connection __typename = %v, want UserConnection", users["__typename"])
	}
	edge := users["edges"].([]any)[0].(map[string]any)
	if edge["__typename"] != "UserEdge" {
		t.Errorf("edge __typename = %v, want UserEdge", edge["__typename"])
	}
	node := edge["node"].(map[string]any)
	if node["__typename"] != "User" {
		t.Errorf("node __typename = %v, want User", node["__typename"])
	}
	// The data selection alongside it still resolves — `__typename` neither
	// shadows a real field nor disturbs the projection.
	if node["name"] != "alice" {
		t.Errorf("node name = %v, want alice", node["name"])
	}
	if got := users["pageInfo"].(map[string]any)["kind"]; got != "PageInfo" {
		t.Errorf("aliased pageInfo __typename = %v, want PageInfo under the alias key", got)
	}
}

// TestExecute_NestedTypenameLeavesTheProjectionIntact — the meta-field must not
// reach ReadCriteria.Projection. It backs no column, so emitting it as a wire
// path makes ParseProjection reject the whole set and the read silently widens
// to whole-document: pushdown lost, and (see the test below) the field-access
// gate disarmed — for EVERY client that appends `__typename`, which is all of
// them.
func TestExecute_NestedTypenameLeavesTheProjectionIntact(t *testing.T) {
	h := &fakeReadHandler{page: queries.PageOf[execResult]{
		Items:       []execResult{{ID: sp("u1"), Name: sp("alice")}},
		ItemCursors: []string{"c1"},
		TotalCount:  1,
	}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users { edges { node { __typename name } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	paths := h.captured.Projection.Paths
	if len(paths) != 1 || !paths["Name"] {
		t.Errorf("projection = %v, want exactly the selected leaf {Name} — `__typename` must not widen the read", paths)
	}
}

// TestFieldAccess_TypenameDoesNotDisarmTheRestrictGate — the security half of
// the projection rule, on the fixture whose ToCriteria restricts `Age` for a
// non-admin: explicitly selecting the restricted field is forbidden whether or
// not `__typename` rides along in the same selection set. Without the rule the
// second query resolves with no error at all, so a client that appends
// `__typename` (Apollo, urql, Relay) turns the declared 403 into a silent
// scrub — the value never leaks, but the contract the Query declares stops
// being enforced.
func TestFieldAccess_TypenameDoesNotDisarmTheRestrictGate(t *testing.T) {
	for _, query := range []string{
		`query { items { edges { node { name age } } } }`,
		`query { items { edges { node { __typename name age } } } }`,
	} {
		reg := restrictRegistry()
		ctx := configuration.NewAppContextWithRandomID(configuration.LangENG) // non-admin

		resp := reg.Execute(ctx, query, nil, "")

		if len(resp.Errors) == 0 {
			t.Fatalf("%s: selecting the restricted field must be forbidden", query)
		}
		if got := resp.Errors[0].Extensions["notificationKey"]; got != "FieldAccessForbiddenNotification" {
			t.Errorf("%s: notificationKey = %v, want FieldAccessForbiddenNotification", query, got)
		}
	}
}
