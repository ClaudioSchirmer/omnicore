// Package requests carries the framework side of the wire→application input
// boundary: the opt-in marker a Request DTO embeds to have its Command built
// for it, and the generic builder that does it.
//
// It is the twin of web/responses on the way in. Same posture, mirrored
// direction: there the Response declares what it renders FROM a Result; here
// the Request declares what it produces INTO a Command.
package requests

import (
	"fmt"
	"reflect"
)

// Auto is the opt-in marker for the generic Request→Command mapping.
//
// Every seat in this framework is hand-written by default — ToCommand,
// ApplyTo, FromEntity, ToCriteria. [AutoFromRequest] is the framework's offer
// to write ToCommand for you, and this marker is how a Request ACCEPTS it:
//
//	type InsertUserRequest struct {
//	    fwrequests.Auto
//
//	    Name  string  `json:"name"`
//	    Phone *string `json:"phone,omitempty"`
//	}
//
//	func (r InsertUserRequest) ToCommand() *commands.InsertUserCommand {
//	    return fwrequests.AutoFromRequest[*commands.InsertUserCommand](r)
//	}
//
// What the declaration buys, and what it costs:
//
//   - [AutoFromRequest] becomes callable. Without the marker it does not
//     COMPILE, so a Request never silently rides a mapping it did not ask for.
//   - The command constructors validate the pair at Mount (boot): every
//     Request field must have a same-named Command field it can be assigned
//     to. A violation is a boot panic naming the field, never a wire value
//     that quietly goes nowhere.
//
// The check runs one way, and it is the mirror of the read side. There the
// RESPONSE is fully checked (a wire field with no Result backing would always
// render null) while the Result may carry more. Here the REQUEST is fully
// checked (a wire field with no Command slot would be silently dropped) while
// the Command may carry more — its path id, an identity overlay, a default the
// handler fills. One rule underneath: no wire field is ever disconnected in
// silence.
//
// A Request WITHOUT the marker keeps the default posture: no guard, no
// generic builder, ToCommand written by hand — free to rename a field, fold a
// flat wire shape into a nested Command value, or compute anything it likes.
//
// Embedding is invisible to the wire: Auto declares no exported fields, so it
// neither renders nor parses.
type Auto struct{}

// autoToCommand is the sealed opt-in signal. Unexported on purpose: the only
// way to obtain it is to embed [Auto], so the declaration always comes from
// the framework and can never be forged.
func (Auto) autoToCommand() {}

// AutoMapper is the constraint [AutoFromRequest] places on its source type:
// the Request must have opted in by embedding [Auto].
type AutoMapper interface {
	autoToCommand()
}

// FormatAutoRequestGuard assembles the diagnostic for a Request that declared
// [Auto] but whose field travel cannot be served. The command constructors
// raise it at Mount, before the service serves anything.
func FormatAutoRequestGuard(reqType, cmdType reflect.Type, reason string) string {
	return fmt.Sprintf(
		"[auto-request] %s declares fwrequests.Auto but cannot build %s:\n"+
			"  - %s\n"+
			"\n"+
			"WHAT THIS MEANS\n"+
			"  An auto-built Command travels field by field: every Request field writes the SAME-NAMED\n"+
			"  field on the Command and must be directly assignable to it. No serialization is involved,\n"+
			"  so a pair the builder cannot prove is exact is refused rather than silently degraded.\n"+
			"\n"+
			"HOW TO FIX IT — pick one\n"+
			"  1. Align the two types, so the wire field and the Command field share a name and a shape.\n"+
			"  2. Drop fwrequests.Auto from %[1]s and write ToCommand by hand. That is the escape hatch for\n"+
			"     exactly this: renaming a field (document on the wire, DocumentKey on the Command),\n"+
			"     folding a flat wire shape into a nested Command value, or computing anything on the way.\n"+
			"  3. Remove the field from the Request if nothing consumes it. The Command is free to carry\n"+
			"     MORE than the wire sends — its path id via SetPathID, an identity-derived field, a\n"+
			"     handler default. The reverse is what this guard refuses: a wire field with nowhere to\n"+
			"     land means the client sends a value that is dropped in silence.\n"+
			"\n"+
			"WHAT TRAVELS AUTOMATICALLY\n"+
			"  identical types · pointer wrapping and unwrapping · same-family numeric conversion\n"+
			"  (int32→int64, float32→float64; out of range leaves the field zero) · domain.ID ↔ string ·\n"+
			"  struct→struct, slice→slice and map→map under an identical key type, resolved recursively.\n"+
			"  What does NOT: two DIFFERENT types where one owns its own JSON/text codec (string→time.Time\n"+
			"  is the usual one), a conversion whose rounding belongs to the codec, an interface\n"+
			"  destination, or a map whose KEY type changes.",
		typeLabel(reqType), typeLabel(cmdType), reason)
}

// formatRuntimeGuard is the same diagnostic raised from [AutoFromRequest]
// itself, plus the one thing the boot text cannot say: WHY it arrived on a
// request instead of at startup. See the Response-side twin for the full
// rationale — the Mount check only exists where both types are known before a
// request does.
func formatRuntimeGuard(reqType, cmdType reflect.Type, reason string) string {
	return FormatAutoRequestGuard(reqType, cmdType, reason) +
		"\n" +
		"\nWHY THIS SURFACED ON A REQUEST AND NOT AT BOOT" +
		"\n  The pair is normally validated at Mount by the canonical command constructors —" +
		"\n  CommandWithBody and CommandWithBodyID — which receive both the Request and the Command as" +
		"\n  type parameters and can check them before the service serves anything. This call did not" +
		"\n  come through one of them, so no boot moment existed to check it. That is expected for a" +
		"\n  hand-written fiber.Handler that builds the Command itself, the gRPC surface, or any future" +
		"\n  surface that calls the builder directly." +
		"\n" +
		"\n  This is NOT a different failure from the boot one, and the endpoint is not partly working:" +
		"\n  the pair could never have been built. Only the moment of discovery moved, from startup to" +
		"\n  the first request that exercised this path — which is why it can appear on a rarely-hit" +
		"\n  endpoint long after deploy. The fix is identical to the boot case above." +
		"\n" +
		"\n  To get boot-time (or CI-time) detection on a surface the constructors do not cover, assert" +
		"\n  the pair yourself: requests.AutoRequestReason(reqType, cmdType) returns the empty string" +
		"\n  when the pair builds, and this same reason when it does not. A test that sweeps your" +
		"\n  service's (Request, Command) pairs turns this into a red build instead of a 500."
}

func typeLabel(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}
	return t.String()
}
