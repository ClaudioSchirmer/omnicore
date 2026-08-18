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
// canonical route constructors raise it at Mount, before the service serves
// anything.
func FormatAutoFromResultGuard(resultType, respType reflect.Type, reason string) string {
	return fmt.Sprintf(
		"[auto-map] %s declares fwresponses.Auto but cannot be mapped from %s:\n"+
			"  - %s\n"+
			"\n"+
			"WHAT THIS MEANS\n"+
			"  An auto-mapped Response travels field by field: every field reads the SAME-NAMED field on\n"+
			"  the Result and must be directly assignable from it. No serialization is involved, so a pair\n"+
			"  the copier cannot prove is exact is refused rather than silently degraded.\n"+
			"\n"+
			"HOW TO FIX IT — pick one\n"+
			"  1. Align the two types. When the wire wants a shape the Result does not hold, produce the\n"+
			"     rendered value in the Query's FromQueryResult hook — where read-side computation\n"+
			"     belongs — and let the Response mirror the Result's type.\n"+
			"  2. Drop fwresponses.Auto from %[1]s and write FromResult by hand. That is the escape hatch\n"+
			"     for exactly this: renaming a field, flattening a nested segment, folding two Result\n"+
			"     fields into one wire value, or any conversion the framework will not invent for you.\n"+
			"  3. Remove the field from the Response if the wire does not need it. A Response is free to\n"+
			"     expose a SUBSET of the Result — the reverse (a wire field with no Result backing) is\n"+
			"     what this guard refuses, because it would render null on every response, forever.\n"+
			"\n"+
			"WHAT TRAVELS AUTOMATICALLY\n"+
			"  identical types · pointer wrapping and unwrapping · same-family numeric conversion\n"+
			"  (int32→int64, float32→float64; out of range leaves the field zero) · domain.ID ↔ string ·\n"+
			"  struct→struct, slice→slice and map→map under an identical key type, resolved recursively.\n"+
			"  What does NOT: two DIFFERENT types where one owns its own JSON/text codec (time.Time→string\n"+
			"  is the usual one — only MarshalJSON knows the RFC 3339 form), a conversion whose rounding\n"+
			"  belongs to the codec (float64→int64), an interface source, or a map whose KEY type changes.",
		typeName(respType), typeName(resultType), reason)
}

// formatRuntimeGuard is the same diagnostic raised from [AutoFromResult]
// itself, plus the one thing the boot text cannot say: WHY it arrived on a
// request instead of at startup.
//
// The Mount-time check lives in the canonical route constructors — the only
// place both types are known before any request exists. Every other call site
// maps on its own: a hand-written handler, the GraphQL and gRPC surfaces, a
// CSV/XLSX export, whatever surface comes next. There is no seat to hook there,
// so the contract is enforced here instead. Same rule, later moment.
func formatRuntimeGuard(resultType, respType reflect.Type, reason string) string {
	return FormatAutoFromResultGuard(resultType, respType, reason) +
		"\n" +
		"\nWHY THIS SURFACED ON A REQUEST AND NOT AT BOOT" +
		"\n  The pair is normally validated at Mount by the canonical route constructors —" +
		"\n  QueryWithParams, QueryByID, CommandByID, CommandWithBody, CommandWithBodyID — which receive" +
		"\n  both the Result and the Response as type parameters and can check them before the service" +
		"\n  serves anything. This call did not come through one of them, so no boot moment existed to" +
		"\n  check it. That is expected for: a hand-written fiber.Handler that maps the Result itself," +
		"\n  the GraphQL and gRPC surfaces, the CSV/XLSX export, or any future surface that calls the" +
		"\n  mapper directly." +
		"\n" +
		"\n  This is NOT a different failure from the boot one, and the endpoint is not partly working:" +
		"\n  the pair could never have been mapped. Only the moment of discovery moved, from startup to" +
		"\n  the first request that exercised this path — which is why it can appear on a rarely-hit" +
		"\n  endpoint long after deploy. The fix is identical to the boot case above." +
		"\n" +
		"\n  To get boot-time (or CI-time) detection on a surface the constructors do not cover, assert" +
		"\n  the pair yourself: responses.AutoFromResultReason(resultType, respType) returns the empty" +
		"\n  string when the pair maps, and this same reason when it does not. A test that sweeps your" +
		"\n  service's (Result, Response) pairs turns this into a red build instead of a 500."
}
