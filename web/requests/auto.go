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
// raise it at Mount; [AutoFromRequest] raises the identical text if it ever
// meets such a pair outside them.
func FormatAutoRequestGuard(reqType, cmdType reflect.Type, reason string) string {
	return fmt.Sprintf(
		"[auto-request] %s declares fwrequests.Auto but cannot build %s:\n  - %s\n"+
			"An auto-built Command travels field by field: each Request field writes the same-named "+
			"Command field and must be directly assignable to it. Align the two types, or drop "+
			"fwrequests.Auto and write ToCommand by hand (renaming and reshaping are exactly what the "+
			"hand-written seat is for).",
		typeLabel(reqType), typeLabel(cmdType), reason)
}

func typeLabel(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}
	return t.String()
}
