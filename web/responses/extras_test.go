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
