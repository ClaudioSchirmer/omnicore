package responses

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// wName is a raw value object over string (passthrough in Map).
type wName string

func (n wName) Value() string                                    { return string(n) }
func (n wName) IsValid(string, *domain.NotificationContext) bool { return true }

// wTier is an enum value object over int (member 1; Unknown = 0).
type wTier int

const (
	wTierUnknown wTier = 0
	wTierGold    wTier = 1
)

func (t wTier) Value() int                               { return int(t) }
func (t wTier) Values() []wTier                          { return []wTier{wTierGold} }
func (t wTier) UnknownNotification() domain.Notification { return wTierNote{} }

type wTierNote struct{ domain.DomainNotificationBase }

// wResult is the application-pure twin: same field names, no wire tags. The
// Result carries the same VO types (identical deref'd types are aligned by
// construction under the boot guard).
type wResult struct {
	Name wName
	Tier wTier
	Rank *wTier
}

type wResp struct {
	Auto
	Name wName  `json:"name"`
	Tier wTier  `json:"tier"`
	Rank *wTier `json:"rank,omitempty"`
}

// TestMap_ValueObjects proves a value-object field round-trips through Map's
// JSON step, and that an out-of-set ENUM value converges to Unknown — parity
// with the entity reconstruction and idempotent with the application-side
// ResultFromDoc converge.
func TestMap_ValueObjects(t *testing.T) {
	// valid data: raw VO passes through, enum member preserved
	r := AutoFromResult[wResp](wResult{Name: "Ada", Tier: wTierGold})
	if r.Name != wName("Ada") {
		t.Errorf("Name = %q want Ada", r.Name)
	}
	if r.Tier != wTierGold {
		t.Errorf("Tier = %d want wTierGold", r.Tier)
	}

	// out-of-set enum → Unknown (a stale/tampered Result value never surfaces
	// as a phantom member on the wire)
	if got := AutoFromResult[wResp](wResult{Tier: 99}); got.Tier != wTierUnknown {
		t.Errorf("Tier(99) = %d want wTierUnknown (converge)", got.Tier)
	}

	// nullable enum: nil → nil (untouched — absence, not Unknown), out-of-set
	// pointed-to value → &Unknown
	if got := AutoFromResult[wResp](wResult{Tier: wTierGold}); got.Rank != nil {
		t.Errorf("Rank nil = %v want nil", got.Rank)
	}
	bad := wTier(99)
	if got := AutoFromResult[wResp](wResult{Tier: wTierGold, Rank: &bad}); got.Rank == nil || *got.Rank != wTierUnknown {
		t.Errorf("Rank(99) = %v want &wTierUnknown", got.Rank)
	}
}

// Enum convergence must recurse: an out-of-set enum inside a nested struct and
// inside a slice-of-struct element both converge to Unknown.

type wSegResult struct {
	Tier wTier
}

type wNestedResult struct {
	Segment  wSegResult
	Segments []wSegResult
}

type wSegResp struct {
	Tier wTier `json:"tier"`
}

type wNestedResp struct {
	Auto
	Segment  wSegResp   `json:"segment"`
	Segments []wSegResp `json:"segments"`
}

func TestMap_EnumConverge_NestedDepths(t *testing.T) {
	got := AutoFromResult[wNestedResp](wNestedResult{
		Segment:  wSegResult{Tier: 99},
		Segments: []wSegResult{{Tier: wTierGold}, {Tier: 42}},
	})
	if got.Segment.Tier != wTierUnknown {
		t.Errorf("nested struct enum(99) = %d want wTierUnknown", got.Segment.Tier)
	}
	if len(got.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(got.Segments))
	}
	if got.Segments[0].Tier != wTierGold {
		t.Errorf("in-set slice-element enum must be preserved, got %d", got.Segments[0].Tier)
	}
	if got.Segments[1].Tier != wTierUnknown {
		t.Errorf("slice-element enum(42) = %d want wTierUnknown", got.Segments[1].Tier)
	}
}
