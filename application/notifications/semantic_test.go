package notifications

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// FieldAccessForbiddenNotification overrides the default classification: an
// ACTIVE reference to a restricted field is a 403, not a 422 — the promotion
// override is the mechanism (gotcha: a renamed method silently falls back to
// SemanticValidation).
func TestFieldAccessForbidden_SemanticIsForbidden(t *testing.T) {
	var n domain.Notification = FieldAccessForbiddenNotification{}
	if got := n.Semantic(); got != domain.SemanticForbidden {
		t.Fatalf("Semantic() = %v, want SemanticForbidden (403)", got)
	}
}
