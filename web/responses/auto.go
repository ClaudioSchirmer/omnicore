package responses

import (
	"fmt"
	"reflect"
)

// The opt-in marker for the generic Result→Response mapping.
//
// Every seat in this framework is hand-written by default — ToCommand,
// ApplyTo, ApplyPartiallyTo, FromEntity, ToCriteria, ToQuery. The Result→
// Response travel is no exception: FromResult is YOUR method, and the body is
// yours to write. [Map] is the framework's offer to write it for you, and
// this marker is how a Response ACCEPTS that offer.
//
// Embed it to declare the type auto-mapped:
//
//	type FindUsersByParamsResponse struct {
//	    fwresponses.Auto
//
//	    ID   *string `json:"id,omitempty"`
//	    Name *string `json:"name,omitempty"`
//	}
//
//	func (FindUsersByParamsResponse) FromResult(r appqueries.FindUsersResult) FindUsersByParamsResponse {
//	    return fwresponses.AutoFromResult[FindUsersByParamsResponse](r)
//	}
//
// What the declaration buys, and what it costs:
//
//   - [AutoFromResult] becomes callable. Without the marker it does not COMPILE — the
//     generic constraint rejects the type — so a Response that never opted in
//     can never silently ride a mapping it did not ask for.
//   - The route constructors validate the pair at Mount (boot): every Response
//     field must have a same-named Result field, and every mapped field pair
//     must be directly convertible. A violation is a boot panic naming the
//     field, not a silent null or a silent slowdown at request time.
//
// The check is one-way: the Response may expose a SUBSET of the Result (that
// is how a DTO cuts fields off the wire — swap the Response, the read model
// stays). Only the fields the Response actually declares are validated, so a
// Result may carry anything it likes beyond them.
//
// A Response WITHOUT the marker keeps the framework's default posture: no
// guard, no generic mapper, `FromResult` written by hand — free to rename
// (`Result.Name` → `Response.Nickname`), flatten a nested segment or fold two
// fields into one wire value.
//
// Embedding is invisible on the wire: Auto declares no exported fields, so it
// contributes no JSON key.
type Auto struct{}

// autoFromResult is the sealed opt-in signal. It is unexported on purpose:
// the ONLY way to obtain it is to embed [Auto], so the declaration always
// comes from the framework and can never be forged by declaring a method.
func (Auto) autoFromResult() {}

// AutoMapper is the constraint [AutoFromResult] places on its destination
// type: the Response must have opted in by embedding [Auto].
type AutoMapper interface {
	autoFromResult()
}

// FormatAutoFromResultGuard assembles the diagnostic for a Response that declared
// [Auto] but whose field travel cannot be served by the compiled copier. The
// route constructors raise it at Mount; [Map] raises the identical text if it
// ever meets such a pair outside them.
func FormatAutoFromResultGuard(resultType, respType reflect.Type, reason string) string {
	return fmt.Sprintf(
		"[auto-map] %s declares fwresponses.Auto but cannot be mapped from %s:\n  - %s\n"+
			"An auto-mapped Response travels field by field: each field reads the same-named Result "+
			"field and must be directly assignable from it. Align the two types (let the Query's "+
			"FromQueryResult produce the rendered value), or drop fwresponses.Auto and write FromResult by hand.",
		typeName(respType), typeName(resultType), reason)
}
