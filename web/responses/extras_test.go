package responses

import (
	"reflect"
	"testing"

	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
)

func TestNoBody_ReturnsEmptyNone(t *testing.T) {
	got := NoBody(fwresults.None{})
	if !reflect.DeepEqual(got, None{}) {
		t.Errorf("NoBody = %+v, want None{}", got)
	}
}

func TestRawDoc_Identity(t *testing.T) {
	in := map[string]any{"a": 1, "b": "two"}
	got := RawDoc(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("RawDoc = %+v, want identity", got)
	}
	// Same underlying map: mutating the source visible through the returned ref.
	in["c"] = 3
	if got["c"] != 3 {
		t.Errorf("RawDoc should return the same map reference (identity projector)")
	}
}

func TestRawDoc_NilInput(t *testing.T) {
	if got := RawDoc(nil); got != nil {
		t.Errorf("RawDoc(nil) = %v, want nil", got)
	}
}
