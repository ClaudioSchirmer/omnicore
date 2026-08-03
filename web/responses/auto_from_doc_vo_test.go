package responses

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// wName is a raw value object over string (passthrough in AutoFromDoc).
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

type wResp struct {
	Name wName  `json:"name"`
	Tier wTier  `json:"tier"`
	Rank *wTier `json:"rank,omitempty"`
}

// TestAutoFromDoc_ValueObjects proves a value-object DTO field round-trips
// through AutoFromDoc's JSON step, and that an out-of-set ENUM value converges
// to Unknown — parity with the entity reconstruction (D8), for BOTH read
// backings (they both feed AutoFromDoc a Go-keyed doc).
func TestAutoFromDoc_ValueObjects(t *testing.T) {
	// valid data: raw VO passes through, enum member preserved
	r := AutoFromDoc[wResp](map[string]any{"Name": "Ada", "Tier": 1})
	if r.Name != wName("Ada") {
		t.Errorf("Name = %q want Ada", r.Name)
	}
	if r.Tier != wTierGold {
		t.Errorf("Tier = %d want wTierGold", r.Tier)
	}

	// out-of-set enum (relational int form) → Unknown
	if got := AutoFromDoc[wResp](map[string]any{"Tier": 99}); got.Tier != wTierUnknown {
		t.Errorf("Tier(99) = %d want wTierUnknown (converge)", got.Tier)
	}

	// out-of-set enum (Mongo float64 form) → Unknown
	if got := AutoFromDoc[wResp](map[string]any{"Tier": float64(99)}); got.Tier != wTierUnknown {
		t.Errorf("Tier(99.0) = %d want wTierUnknown (converge)", got.Tier)
	}

	// nullable enum: absent → nil (untouched), out-of-set value → &Unknown
	if got := AutoFromDoc[wResp](map[string]any{"Tier": 1}); got.Rank != nil {
		t.Errorf("Rank absent = %v want nil", got.Rank)
	}
	if got := AutoFromDoc[wResp](map[string]any{"Tier": 1, "Rank": 99}); got.Rank == nil || *got.Rank != wTierUnknown {
		t.Errorf("Rank(99) = %v want &wTierUnknown", got.Rank)
	}
}
