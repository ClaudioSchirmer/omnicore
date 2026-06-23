package web

import "github.com/ClaudioSchirmer/omnicore/web/queryschema"

// Operator constants for filter declarations in a Request DTO's `filter:"..."`
// struct tag. They are re-exported from web/queryschema (the single source of
// the operator vocabulary) so the long-standing public spelling fwweb.OpEq …
// keeps working for consumers while the canonical definitions, the wire
// parsing, and the criteria emission all live in one place shared by the REST
// wrappers, the OpenAPI generator and the GraphQL endpoint.
const (
	OpEq          = queryschema.OpEq
	OpNe          = queryschema.OpNe
	OpIn          = queryschema.OpIn
	OpNin         = queryschema.OpNin
	OpGte         = queryschema.OpGte
	OpLte         = queryschema.OpLte
	OpGt          = queryschema.OpGt
	OpLt          = queryschema.OpLt
	OpStartsWith  = queryschema.OpStartsWith
	OpContains    = queryschema.OpContains
	OpIEq         = queryschema.OpIEq
	OpINe         = queryschema.OpINe
	OpIIn         = queryschema.OpIIn
	OpINin        = queryschema.OpINin
	OpIStartsWith = queryschema.OpIStartsWith
	OpIContains   = queryschema.OpIContains
)
