package graphql

import "testing"

// The predicate that lets the schema — and ONLY the schema — past the auth
// middleware. Every case here is a security boundary: a false positive hands an
// anonymous caller a data query.

func TestIsIntrospectionOnlyQuery_Accepts(t *testing.T) {
	cases := map[string]string{
		"bare schema":           `{ __schema { queryType { name } } }`,
		"named operation":       `query IntrospectionQuery { __schema { types { name } } }`,
		"typed lookup":          `query { __type(name: "User") { name kind } }`,
		"typename":              `{ __typename }`,
		"several roots at once": `{ __schema { types { name } } __type(name: "User") { name } }`,
		"aliased root":          `{ s: __schema { queryType { name } } }`,
		"two query operations":  `query A { __schema { types { name } } } query B { __typename }`,
		"fragment under a root": `{ __schema { types { ...F } } } fragment F on __Type { name }`,
		"leading whitespace":    "  \n\t{ __typename }",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if !isIntrospectionOnlyQuery(query) {
				t.Errorf("query should be introspection-only: %s", query)
			}
		})
	}
}

func TestIsIntrospectionOnlyQuery_Refuses(t *testing.T) {
	cases := map[string]string{
		"plain data query":              `{ users { id } }`,
		"data mixed with schema":        `{ __schema { types { name } } users { id } }`,
		"schema next to data, reversed": `{ users { id } __schema { types { name } } }`,
		"mutation":                      `mutation { createUser(input: {name: "x"}) { id } }`,
		"introspection-shaped mutation": `mutation { __typename }`,
		"subscription":                  `subscription { __typename }`,
		// A decoy operation must not launder the document: the predicate judges
		// EVERY operation, not the one operationName would select.
		"decoy introspection operation": `query Intro { __schema { types { name } } } query Real { users { id } }`,
		// Root fragments are refused rather than followed — the gate answers a
		// security question and stays cheap and obviously correct.
		"fragment spread at the root": `{ ...F } fragment F on Query { __typename }`,
		"inline fragment at the root": `{ ... on Query { __typename } }`,
		"empty":                       ``,
		"whitespace only":             "   \n ",
		"unparsable":                  `{ __schema `,
		"fragment but no operation":   `fragment F on Query { __typename }`,
		"not graphql at all":          `select * from users`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if isIntrospectionOnlyQuery(query) {
				t.Errorf("query must NOT be treated as introspection-only: %s", query)
			}
		})
	}
}

func TestIsIntrospectionOnlyRequest_BodyShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"standard introspection POST", `{"query":"{ __schema { types { name } } }"}`, true},
		{"with variables and operationName", `{"query":"query I { __typename }","operationName":"I","variables":{}}`, true},
		{"data query", `{"query":"{ users { id } }"}`, false},
		{"no query key", `{"variables":{}}`, false},
		{"empty query", `{"query":""}`, false},
		{"not json", `query { __typename }`, false},
		{"empty body", ``, false},
		{"json array (batch)", `[{"query":"{ __typename }"}]`, false},
		{"null", `null`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIntrospectionOnlyRequest([]byte(tc.body)); got != tc.want {
				t.Errorf("IsIntrospectionOnlyRequest(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
