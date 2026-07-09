package domain

import "testing"

// The remaining NotificationSemantic.String labels not exercised elsewhere.
func TestNotificationSemanticString_RemainingLabels(t *testing.T) {
	cases := map[NotificationSemantic]string{
		SemanticSchema:            "Schema",
		SemanticInternal:          "Internal",
		SemanticMethodNotAllowed:  "MethodNotAllowed",
		SemanticPayloadTooLarge:   "PayloadTooLarge",
		SemanticRequestTimeout:    "RequestTimeout",
		NotificationSemantic(999): "Validation", // default fallback
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(s), got, want)
		}
	}
}

// itoa: zero, positive, and the negative branch (reachable only via the direct
// package-internal helper — index segments are never negative in renderPath).
func TestItoa_Branches(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 123: "123", -5: "-5", -100: "-100"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// renderPath with an index segment renders the "[N]" form (covers the index
// branch and the itoa call site inside renderPath).
func TestRenderPath_IndexSegment(t *testing.T) {
	idx := 2
	got := renderPath([]PathSegment{{Name: "Addresses"}, {Index: &idx}, {Name: "ZipCode"}})
	if got != "addresses[2].zipCode" {
		t.Errorf("renderPath = %q, want addresses[2].zipCode", got)
	}
	// A leading empty name segment is skipped without writing a separator.
	got2 := renderPath([]PathSegment{{Name: ""}, {Name: "Name"}})
	if got2 != "name" {
		t.Errorf("renderPath with empty leading name = %q, want name", got2)
	}
}

// PluralizeWord covers the sh/ch, s/x/z, consonant-y, and default branches.
func TestPluralizeWord_Branches(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"Address":   "Addresses",  // s → es
		"box":       "boxes",      // x → es
		"buzz":      "buzzes",     // z → es
		"dish":      "dishes",     // sh → es
		"match":     "matches",    // ch → es
		"Category":  "Categories", // consonant + y → ies
		"day":       "days",       // vowel + y → s
		"OrderLine": "OrderLines",
	}
	for in, want := range cases {
		if got := PluralizeWord(in); got != want {
			t.Errorf("PluralizeWord(%q) = %q, want %q", in, got, want)
		}
	}
}
