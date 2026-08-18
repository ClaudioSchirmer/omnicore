package web

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	fwrequests "github.com/ClaudioSchirmer/omnicore/web/requests"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
)

// The Result→Response mapping contract, enforced at Mount.
//
// A Response that embeds fwresponses.Auto has DECLARED that the framework maps
// it — and the declaration is only worth something if it is checked. Both
// route constructor families (command and query) run this at Mount, which is
// boot: a Response that cannot honor its own declaration fails there, naming
// the field, instead of shipping a silently-null column or paying a
// serialization round trip on every request.
//
// The two halves, deliberately split:
//
//   - PURITY binds every Result, auto-mapped or not: no json wire tags on an
//     application-layer type (the three-name model).
//   - ALIGNMENT + CONVERTIBILITY bind only the auto-mapped pair: each Response
//     field needs a same-named Result field it can be assigned from. A Response
//     WITHOUT the marker is not checked at all — FromResult is hand-written
//     there, free to rename, flatten or fold.
//
// The check is one-way, which is what lets a Response cut fields off the wire:
// only the fields the RESPONSE declares are examined, so the Result may carry
// anything beyond them.

// autoMapperType is the marker interface a Response satisfies by embedding
// fwresponses.Auto.
var autoMapperType = reflect.TypeOf((*responses.AutoMapper)(nil)).Elem()

// declaresAutoMap reports whether respType opted into the generic mapping.
// Both the value and the pointer receiver sets are consulted, since the marker
// is promoted from an embedded struct either way.
func declaresAutoMap(respType reflect.Type) bool {
	if respType == nil {
		return false
	}
	return respType.Implements(autoMapperType) ||
		reflect.PointerTo(respType).Implements(autoMapperType)
}

// validateResponseMapping enforces the contract for one (Result, Response)
// pair at Mount. Panics with a field-level diagnostic on violation — the
// framework's posture for a structural contract it cannot honor at runtime.
func validateResponseMapping(resultType, respType reflect.Type) {
	resultType = derefBootType(resultType)
	respType = derefBootType(respType)

	if errs := queryschema.ValidateResultPurity(resultType); len(errs) > 0 {
		panic(queryschema.FormatResultPurityGuard(resultType, errs))
	}
	if !declaresAutoMap(respType) {
		return // hand-written FromResult: the body is the consumer's business
	}
	if respType.Kind() != reflect.Struct || resultType.Kind() != reflect.Struct {
		return
	}
	if errs := queryschema.ValidateResultAlignment(resultType, respType); len(errs) > 0 {
		panic(queryschema.FormatResultAlignmentGuard(resultType, respType, errs))
	}
	// Names line up; now the values must actually travel. This is what lets
	// Map drop its serialization fallback: a pair that reaches a request is a
	// pair the boot proved copyable.
	if reason := responses.AutoFromResultReason(resultType, respType); reason != "" {
		panic(responses.FormatAutoFromResultGuard(resultType, respType, reason))
	}
}

// derefBootType walks pointers for the boot-time reflection.
func derefBootType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// autoRequestType is the marker interface a Request satisfies by embedding
// fwrequests.Auto.
var autoRequestType = reflect.TypeOf((*fwrequests.AutoMapper)(nil)).Elem()

// declaresAutoRequest reports whether reqType opted into the generic
// Request→Command mapping.
func declaresAutoRequest(reqType reflect.Type) bool {
	if reqType == nil {
		return false
	}
	return reqType.Implements(autoRequestType) ||
		reflect.PointerTo(reqType).Implements(autoRequestType)
}

// validateRequestMapping enforces the input half of the contract at Mount: a
// Request that declared fwrequests.Auto must be able to build its Command by
// assignment. The REQUEST drives — a wire field with nowhere to land is the
// failure, while Command fields the Request does not supply are legitimate
// (the path id, an identity overlay, a handler default).
func validateRequestMapping(reqType, cmdType reflect.Type) {
	reqType = derefBootType(reqType)
	if !declaresAutoRequest(reqType) {
		return // hand-written ToCommand: renaming and reshaping are its purpose
	}
	if reason := fwrequests.AutoRequestReason(reqType, cmdType); reason != "" {
		panic(fwrequests.FormatAutoRequestGuard(reqType, cmdType, reason))
	}
}
