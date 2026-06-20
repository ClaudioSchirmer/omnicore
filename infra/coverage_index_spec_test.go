package infra

import (
	"testing"
	"time"
)

func TestIndexModel_TextOptions(t *testing.T) {
	spec := TextIndex("name", "email").
		Weights(map[string]int{"name": 2}).
		DefaultLanguage("portuguese").
		LanguageOverride("lang").
		Name("text_idx")
	m := spec.IndexModel()
	if m.Options == nil {
		t.Fatal("expected populated options")
	}
	if !spec.IsText() {
		t.Error("TextIndex spec must report IsText = true")
	}
}

func TestIndexModel_GeoAndGenericOptions(t *testing.T) {
	spec := GeoIndex("loc").
		Name("geo_idx").
		Unique().
		Sparse().
		Hidden().
		Partial(Exists("loc", true)).
		TTL(time.Hour).
		Collation(&CollationSpec{Locale: "pt", Strength: 1}).
		Min(-180).
		Max(180).
		Bits(26).
		BucketSize(4)
	m := spec.IndexModel()
	if m.Options == nil {
		t.Fatal("expected populated options")
	}
	// Confirm the geo boundary setters landed on the spec.
	if spec.geoMin == nil || *spec.geoMin != -180 {
		t.Errorf("Min not applied: %v", spec.geoMin)
	}
	if spec.geoMax == nil || *spec.geoMax != 180 {
		t.Errorf("Max not applied: %v", spec.geoMax)
	}
	if spec.geoBits == nil || *spec.geoBits != 26 {
		t.Errorf("Bits not applied: %v", spec.geoBits)
	}
	if spec.geoBucketSize == nil || *spec.geoBucketSize != 4 {
		t.Errorf("BucketSize not applied: %v", spec.geoBucketSize)
	}
}

func TestIndexModel_HashedAndDesc(t *testing.T) {
	_ = Index("x").Hashed().IndexModel()
	_ = Index("y").Desc().IndexModel()
	_ = Compound("a", "b").IndexModel()
}

func TestEncodeOrder_AllBranches(t *testing.T) {
	cases := []struct {
		order IndexKeyOrder
		want  any
	}{
		{IndexOrderAsc, int32(1)},
		{IndexOrderDesc, int32(-1)},
		{IndexOrderText, "text"},
		{IndexOrderGeo2D, "2d"},
		{IndexOrderGeo2DSph, "2dsphere"},
		{IndexOrderHashed, "hashed"},
		{IndexKeyOrder("garbage"), int32(1)}, // default fallback
	}
	for _, c := range cases {
		if got := encodeOrder(c.order); got != c.want {
			t.Errorf("encodeOrder(%q) = %v, want %v", c.order, got, c.want)
		}
	}
}

func TestDriverCollation_NilAndPopulated(t *testing.T) {
	var nilSpec *CollationSpec
	if nilSpec.DriverCollation() != nil {
		t.Error("nil CollationSpec must yield nil driver collation")
	}
	got := (&CollationSpec{Locale: "pt", Strength: 2, NumericOrdering: true}).DriverCollation()
	if got == nil || got.Locale != "pt" || got.Strength != 2 || !got.NumericOrdering {
		t.Errorf("DriverCollation drifted: %+v", got)
	}
}
