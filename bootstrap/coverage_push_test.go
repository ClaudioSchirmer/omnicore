package bootstrap

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra"
)

// TestValidateUpstreamSubscriptions_SurfacesMaterializingSource drives the
// §8.3 accumulation branch inside validateUpstreamSubscriptions: a view embeds
// an external collection with a covering index (so §8.1 passes) but no
// subscription declares that collection, so guardMaterializingSource returns
// an error that validateUpstreamSubscriptions folds into the violation list.
func TestValidateUpstreamSubscriptions_SurfacesMaterializingSource(t *testing.T) {
	views := []*infra.ViewDefinition{
		infra.View("orders").Root("orders").
			Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
			Indexes(infra.Index("buyer_id")). // §8.1 satisfied
			Version(1),
	}
	// No subscriptions → "users" has no materializing source → §8.3.
	err := validateUpstreamSubscriptions(nil, views, profileDev)
	if err == nil || !strings.Contains(err.Error(), "§8.3") {
		t.Fatalf("expected §8.3 materializing-source violation, got %v", err)
	}
}
