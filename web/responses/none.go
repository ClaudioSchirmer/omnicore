// Package responses carries the framework's reusable wire-Response types
// consumed by the HTTP wrappers. A service typically defines its own
// Response type per endpoint, co-located with the Request DTO, and writes
// a method FromResult(R) Response that maps the application-layer Result
// to the JSON shape exposed at the boundary. The values declared here
// cover the "no body" default.
package responses

import fwresults "github.com/ClaudioSchirmer/omnicore/application/results"

// None is the canonical zero-data Response. Pair with fwresults.None at
// the handler's TResult and pass NoBody as the wrapper's response
// projection — the wrapper detects this type at runtime and emits the
// success envelope WITHOUT a "data" field, matching the conventional
// "204 No Content"-style shape for state-transition endpoints.
type None struct{}

// NoBody is the canonical response projection for wrappers paired with
// fwresults.None — converts the empty Result to the empty Response.
func NoBody(_ fwresults.None) None { return None{} }
