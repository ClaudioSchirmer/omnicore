package web

import (
	"log/slog"
	"reflect"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/web/responses"
)

// The Result→Response mapping advisory: one boot warning per degraded pair.
//
// responses.Map serves the Result→Response travel with a copier compiled per
// type pair. When a pair's shape cannot be proven to copy exactly like the
// JSON round trip it replaced, Map keeps that round trip — correct, but it
// pays a marshal + unmarshal PER ITEM on every request, which on a page read
// multiplies by the page size. That decision is invisible at runtime, so the
// read-side constructors resolve it at Mount and warn about the degraded pairs
// ONLY: a compatible pair says nothing, because a log line per healthy
// endpoint is noise nobody reads.
//
// The check is also a pre-warm — the verdict it resolves is the same one Map
// caches, so the first request no longer pays the compilation.

// warnedMappings dedupes the advisory by TYPE PAIR: several endpoints (and
// the export twin of a listing) can share one Result/Response pair, and the
// pair is the single fix site, so it earns exactly one line.
var warnedMappings sync.Map // [2]reflect.Type → struct{}

// warnMappingFallback emits the boot advisory when the (result, response)
// pair cannot take the optimized mapping. Called by the read-side
// constructors at Mount, after the alignment guards have passed.
func warnMappingFallback(reqType, resultType, respType reflect.Type) {
	reason := responses.MappingFallbackReason(resultType, respType)
	if reason == "" {
		return
	}
	if _, dup := warnedMappings.LoadOrStore([2]reflect.Type{resultType, respType}, struct{}{}); dup {
		return
	}
	slog.Warn("query.response.mapping: this endpoint's Response (web) ↔ Result (application) travel is not compatible with the optimized mapping, so each item falls back to a marshal+unmarshal round trip — a small extra CPU and allocation cost per request, multiplied by the page size. See the manual, Auto query handlers → \"Response mapping — the optimized travel and its fallback\", for what makes a pair compatible.",
		"request", typeLabel(reqType),
		"result", typeLabel(resultType),
		"response", typeLabel(respType),
		"reason", reason)
}

// typeLabel renders a reflect.Type for the advisory, nil-safe.
func typeLabel(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}
	return t.String()
}
