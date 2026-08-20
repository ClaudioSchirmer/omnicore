package bootstrap

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// verifySearchIndexes fails the boot when an endpoint accepts `?search=` over a
// projected view that declares no text index.
//
// Free-text search is a Mongo `$text` query, which the server refuses outright
// unless the collection carries a text index. The two halves of that contract
// are declared in places neither can see: `query:"search"` on the Request DTO
// at route registration, and `query.TextIndex(...)` on the ViewDefinition the
// feature contributes. Nothing put them side by side, so the mismatch surfaced
// as a raw store error on the first request that used the parameter — an
// endpoint that advertised a control it could never serve.
//
// This runs after the Mount phase, which is when both halves exist: the views
// are collected before mounting, and the wrappers record their declarations
// while mounting.
//
// Two cases are deliberately NOT failures:
//
//   - A view name the service does not declare. The registry is process-wide,
//     so it can carry entries from another composition root in the same binary
//     (tests, a multi-app process); an unknown name is simply not this boot's
//     business.
//   - A RelationalSource view. Free text over the SoR is a declared capability
//     boundary, answered with a typed 400 RelationalCapabilityNotification —
//     the endpoint's contract, not a misconfiguration. A DTO shared between a
//     Mongo view and its relational twin is the canonical shape, and the twin
//     must not fail the boot for it.
func verifySearchIndexes(features []Feature) error {
	optIns := queryschema.SearchOptIns()
	if len(optIns) == 0 {
		return nil
	}
	views, err := collectViews(features)
	if err != nil {
		return err
	}
	composed, err := collectComposedViews(features)
	if err != nil {
		return err
	}

	byName := make(map[string]*query.ViewDefinition, len(views)+len(composed))
	for _, v := range views {
		byName[v.Name()] = v
	}
	// A composed name reads through its primary, which is where the search
	// runs and therefore where the index has to be.
	for _, c := range composed {
		if p := c.PrimaryView(); p != nil {
			byName[c.Name()] = p
		}
	}

	var missing []string
	for _, o := range optIns {
		v, declaredHere := byName[o.View]
		switch {
		case !declaredHere, v.IsRelational(), hasTextIndex(v):
			continue
		}
		missing = append(missing, fmt.Sprintf("  - view %q, read by %s", o.View, o.Request))
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"bootstrap: these endpoints accept `?search=` over a view that declares no text index:\n%s\n"+
			"Free-text search runs as a Mongo $text query and the server refuses it without one. "+
			"Declare query.TextIndex(\"field\", ...) on the view, or drop query:\"search\" from the Request DTO",
		strings.Join(missing, "\n"))
}

// hasTextIndex reports whether the view declares an index carrying a text key.
func hasTextIndex(v *query.ViewDefinition) bool {
	for _, spec := range v.IndexSpecs() {
		for _, k := range spec.Keys {
			if k.Order == query.IndexOrderText {
				return true
			}
		}
	}
	return false
}
