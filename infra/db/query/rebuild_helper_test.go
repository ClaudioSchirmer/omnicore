package query

import "testing"

func TestRegistryCombinedOrNone(t *testing.T) {
	if got := registryCombinedOrNone(nil); got != "<none>" {
		t.Errorf("nil row = %q, want <none>", got)
	}
	if got := registryCombinedOrNone(&ViewRegistryRow{CombinedHash: "abc"}); got != "abc" {
		t.Errorf("row = %q, want abc", got)
	}
}
